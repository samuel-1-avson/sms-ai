// Package config loads and validates runtime configuration from environment
// variables. Every setting has a development-friendly default; Validate
// enforces the stricter rules that apply outside development.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ModelPrice is the USD price per one million tokens.
type ModelPrice struct {
	InputPerM  float64
	OutputPerM float64
}

type Config struct {
	Env            string
	HTTPAddr       string
	DatabaseURL    string
	AutoMigrate    bool
	EmbeddedWorker bool
	LogLevel       string

	CurrentKeyVersion int
	EncryptionKeys    map[int][]byte
	HMACKeys          map[int][]byte
	InsecureDevKeys   bool

	PrimarySMSProvider  string
	PrimaryUSSDProvider string
	SMSEnvironment      string
	SMSServiceID        string
	USSDServiceCode     string
	SMSOutboundSenderID string
	InboundDebounce     time.Duration
	CoalesceExtend      time.Duration
	MaxCoalesce         int
	USSDFastPathTimeout time.Duration
	USSDMaxChars        int
	USSDSessionTTL      time.Duration
	MockWebhookSecret   string
	MockDLRCallbackURL  string

	// Arkesel (docs/PROVIDER_VALIDATION.md). The webhook secret verifies
	// signed delivery reports; USSD callbacks are unsigned, so they carry
	// one of ArkeselUSSDTokens as ?token= on the registered endpoint URL.
	ArkeselAPIKey         string
	ArkeselBaseURL        string
	ArkeselWebhookSecret  string
	ArkeselDLRCallbackURL string
	ArkeselUSSDTokens     []string
	ArkeselUSSDAllowedIPs []netip.Prefix
	ArkeselClientIPHeader string

	// Hubtel (docs/PROVIDER_VALIDATION.md). Hubtel signs no callbacks, so USSD
	// and delivery-report callbacks each carry one of their tokens as ?token=
	// on the registered URL, optionally combined with a source-IP allowlist.
	HubtelClientID       string
	HubtelClientSecret   string
	HubtelSMSBaseURL     string
	HubtelUSSDTokens     []string
	HubtelDLRTokens      []string
	HubtelUSSDAllowedIPs []netip.Prefix
	HubtelDLRAllowedIPs  []netip.Prefix
	HubtelClientIPHeader string

	// MTN's own developer platform (docs/PROVIDER_VALIDATION.md): USSD callbacks,
	// the SMS V2 API with OAuth V1 and delivery notifications. MTN documents no callback
	// authentication, so USSD and delivery-notification callbacks each carry one
	// of their tokens as ?token= on the registered URL, optionally combined with
	// a source-IP allowlist. The client id and secret are the app's Consumer Key
	// and Secret; MTNAPIKey is the USSD API's x-api-key and defaults to the
	// Consumer Key. MTNCallbackBaseURL is the public https origin used only by
	// `admin mtn-subscribe`, and the two subscription ids record what MTN
	// returned so `admin mtn-unsubscribe` can remove them.
	MTNClientID           string
	MTNClientSecret       string
	MTNAPIKey             string
	MTNBaseURL            string
	MTNTokenURL           string
	MTNCallbackBaseURL    string
	MTNUSSDTokens         []string
	MTNDLRTokens          []string
	MTNUSSDAllowedIPs     []netip.Prefix
	MTNDLRAllowedIPs      []netip.Prefix
	MTNClientIPHeader     string
	MTNUSSDServiceCode    string
	MTNCountryCode        string
	MTNTargetSystem       string
	MTNUSSDInputIsHistory bool
	// MTN's files disagree on which credential each API wants (see
	// docs/PROVIDER_VALIDATION.md): these add the other one, so a sandbox trial
	// can settle it without a code change.
	MTNSMSSendsAPIKey       bool
	MTNUSSDSendsBearer      bool
	MTNSMSLookupByClientRef bool
	MTNUSSDSubscriptionID   string
	MTNSMSSubscriptionID    string

	// mNotify (docs/PROVIDER_VALIDATION.md), the chosen provider for both USSD
	// and outbound SMS. The API key travels as ?key= on every outbound call, so
	// it is a secret that must never reach a log or an error. mNotify documents
	// no authentication for its USSD callbacks (shared and dedicated mode alike),
	// so they carry one of MNotifyUSSDTokens as ?token= on the callback URL,
	// optionally combined with a source-IP allowlist. MNotifyUSSDMode selects the
	// payload shape: "auto" detects it per request.
	MNotifyAPIKey         string
	MNotifyBaseURL        string
	MNotifyUSSDMode       string
	MNotifyUSSDTokens     []string
	MNotifyUSSDAllowedIPs []netip.Prefix
	MNotifyClientIPHeader string

	LLMProvider        string
	DeepInfraBaseURL   string
	DeepInfraAPIKey    string
	LLMModel           string
	LLMFallbackModel   string
	LLMReasoningEffort string
	LLMMaxOutputTokens int
	LLMTemperature     float64
	LLMTimeout         time.Duration
	LLMPrices          map[string]ModelPrice
	MockLLMLatency     time.Duration

	DailyPerNumberFreeCap    int
	GlobalDailySMSSegmentCap int
	GlobalDailySpendCapUSD   float64
	SMSSegmentCostUSD        float64
	ContextTTL               time.Duration
	ContextMaxTurns          int
	ContextMaxInputTokens    int
	RetentionDays            int
	MaxMorePages             int
	PerNumberRatePerMinute   int
	MaxFailedCodeAttempts    int
	AllowUCS2LocalLanguage   bool

	AdminAPIToken string
	MetricsToken  string

	GroundingSeedPath string
	GroundingRefresh  time.Duration

	WorkerConcurrency int
	WorkerPoll        time.Duration
	JobLease          time.Duration
	JobMaxAttempts    int
}

func (c *Config) IsDevelopment() bool { return c.Env == "development" }
func (c *Config) IsProduction() bool  { return c.Env == "production" }

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return LoadFrom(os.Environ())
}

// LoadFrom reads configuration from KEY=VALUE pairs (used by tests).
func LoadFrom(environ []string) (*Config, error) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	r := reader{env: env}

	c := &Config{
		Env:            r.str("APP_ENV", "development"),
		HTTPAddr:       r.str("HTTP_ADDR", ":8080"),
		DatabaseURL:    r.str("DATABASE_URL", "postgres://smsai:smsai_dev_password@localhost:5442/smsai?sslmode=disable"),
		LogLevel:       r.str("LOG_LEVEL", "info"),
		EmbeddedWorker: r.boolean("EMBEDDED_WORKER", false),

		CurrentKeyVersion: r.integer("CURRENT_KEY_VERSION", 1),

		// No default gateway: a real provider must be chosen explicitly, and
		// forgetting to fails validation instead of falling back to a
		// simulator. "mock" is only accepted in development and test. The
		// Arkesel adapter is kept but dormant (the owner chose not to use it).
		PrimarySMSProvider:  r.str("PRIMARY_SMS_PROVIDER", ""),
		PrimaryUSSDProvider: r.str("PRIMARY_USSD_PROVIDER", ""),
		SMSEnvironment:      r.str("SMS_ENVIRONMENT", "sandbox"),
		SMSServiceID:        r.str("SMS_SERVICE_ID", "SIM-SHORTCODE"),
		USSDServiceCode:     r.str("USSD_SERVICE_CODE", "*713*1#"),
		SMSOutboundSenderID: r.str("SMS_OUTBOUND_SENDER_ID", "SMS-AI"),
		InboundDebounce:     r.millis("INBOUND_DEBOUNCE_MS", 2500),
		CoalesceExtend:      r.millis("INBOUND_COALESCE_EXTEND_MS", 1500),
		MaxCoalesce:         r.integer("INBOUND_MAX_COALESCE", 3),
		USSDFastPathTimeout: r.millis("USSD_FAST_PATH_TIMEOUT_MS", 3200),
		USSDMaxChars:        r.integer("USSD_MAX_CHARS", 182),
		USSDSessionTTL:      time.Duration(r.integer("USSD_SESSION_TTL_SECONDS", 180)) * time.Second,
		MockWebhookSecret:   r.str("MOCK_WEBHOOK_SECRET", "dev-mock-webhook-secret"),
		MockDLRCallbackURL:  r.str("MOCK_DLR_CALLBACK_URL", ""),

		ArkeselAPIKey:         r.secret("ARKESEL_API_KEY"),
		ArkeselBaseURL:        strings.TrimRight(r.str("ARKESEL_BASE_URL", "https://sms.arkesel.com"), "/"),
		ArkeselWebhookSecret:  r.secret("ARKESEL_WEBHOOK_SECRET"),
		ArkeselDLRCallbackURL: r.str("ARKESEL_DLR_CALLBACK_URL", ""),
		ArkeselUSSDTokens:     r.secretList("ARKESEL_USSD_TOKEN"),
		ArkeselUSSDAllowedIPs: r.prefixes("ARKESEL_USSD_ALLOWED_IPS"),
		ArkeselClientIPHeader: r.str("ARKESEL_CLIENT_IP_HEADER", ""),

		HubtelClientID:       r.secret("HUBTEL_CLIENT_ID"),
		HubtelClientSecret:   r.secret("HUBTEL_CLIENT_SECRET"),
		HubtelSMSBaseURL:     strings.TrimRight(r.str("HUBTEL_SMS_BASE_URL", "https://sms.hubtel.com"), "/"),
		HubtelUSSDTokens:     r.secretList("HUBTEL_USSD_TOKEN"),
		HubtelDLRTokens:      r.secretList("HUBTEL_DLR_TOKEN"),
		HubtelUSSDAllowedIPs: r.prefixes("HUBTEL_USSD_ALLOWED_IPS"),
		HubtelDLRAllowedIPs:  r.prefixes("HUBTEL_DLR_ALLOWED_IPS"),
		HubtelClientIPHeader: r.str("HUBTEL_CLIENT_IP_HEADER", ""),

		MTNClientID:           r.secret("MTN_CLIENT_ID"),
		MTNClientSecret:       r.secret("MTN_CLIENT_SECRET"),
		MTNAPIKey:             r.secret("MTN_API_KEY"),
		MTNBaseURL:            strings.TrimRight(r.str("MTN_BASE_URL", "https://api.mtn.com"), "/"),
		MTNTokenURL:           r.str("MTN_TOKEN_URL", ""),
		MTNCallbackBaseURL:    strings.TrimRight(r.str("MTN_CALLBACK_BASE_URL", ""), "/"),
		MTNUSSDTokens:         r.secretList("MTN_USSD_TOKEN"),
		MTNDLRTokens:          r.secretList("MTN_DLR_TOKEN"),
		MTNUSSDAllowedIPs:     r.prefixes("MTN_USSD_ALLOWED_IPS"),
		MTNDLRAllowedIPs:      r.prefixes("MTN_DLR_ALLOWED_IPS"),
		MTNClientIPHeader:     r.str("MTN_CLIENT_IP_HEADER", ""),
		MTNCountryCode:        r.str("MTN_COUNTRY_CODE", "GH"),
		MTNTargetSystem:       r.str("MTN_TARGET_SYSTEM", "sms-ai-ghana"),
		MTNUSSDInputIsHistory: r.boolean("MTN_USSD_INPUT_IS_HISTORY", false),

		MTNSMSSendsAPIKey:       r.boolean("MTN_SMS_SEND_API_KEY", false),
		MTNUSSDSendsBearer:      r.boolean("MTN_USSD_SEND_BEARER", false),
		MTNSMSLookupByClientRef: r.boolean("MTN_SMS_LOOKUP_BY_CLIENT_REF", false),

		MTNUSSDSubscriptionID: r.str("MTN_USSD_SUBSCRIPTION_ID", ""),
		MTNSMSSubscriptionID:  r.str("MTN_SMS_SUBSCRIPTION_ID", ""),

		MNotifyAPIKey:         r.secret("MNOTIFY_API_KEY"),
		MNotifyBaseURL:        strings.TrimRight(r.str("MNOTIFY_BASE_URL", "https://api.mnotify.com/api"), "/"),
		MNotifyUSSDMode:       strings.ToLower(r.str("MNOTIFY_USSD_MODE", "auto")),
		MNotifyUSSDTokens:     r.secretList("MNOTIFY_USSD_TOKEN"),
		MNotifyUSSDAllowedIPs: r.prefixes("MNOTIFY_USSD_ALLOWED_IPS"),
		MNotifyClientIPHeader: r.str("MNOTIFY_CLIENT_IP_HEADER", ""),

		LLMProvider:      r.str("LLM_PROVIDER", "deepinfra"),
		DeepInfraBaseURL: strings.TrimRight(r.str("DEEPINFRA_BASE_URL", "https://api.deepinfra.com/v1/openai"), "/"),
		DeepInfraAPIKey:  r.secret("DEEPINFRA_API_KEY"),
		// Chosen after the 2026-10-17 benchmark (cmd/aibench): gpt-oss-20b met
		// the 3.2 s USSD deadline on 99% of calls vs 70% for Llama 3.3 70B.
		LLMModel:           r.str("LLM_MODEL", "openai/gpt-oss-20b"),
		LLMFallbackModel:   r.str("LLM_FALLBACK_MODEL", "meta-llama/Llama-3.3-70B-Instruct-Turbo"),
		LLMReasoningEffort: r.str("LLM_REASONING_EFFORT", "low"),
		LLMMaxOutputTokens: r.integer("LLM_MAX_OUTPUT_TOKENS", 300),
		LLMTemperature:     r.float("LLM_TEMPERATURE", 0.2),
		LLMTimeout:         r.millis("LLM_TIMEOUT_MS", 15000),
		MockLLMLatency:     r.millis("MOCK_LLM_LATENCY_MS", 400),

		DailyPerNumberFreeCap:    r.integer("DAILY_PER_NUMBER_FREE_CAP", 15),
		GlobalDailySMSSegmentCap: r.integer("GLOBAL_DAILY_SMS_SEGMENT_CAP", 500),
		GlobalDailySpendCapUSD:   r.float("GLOBAL_DAILY_SPEND_CAP_USD", 15.00),
		SMSSegmentCostUSD:        r.float("SMS_SEGMENT_COST_USD", 0.025),
		ContextTTL:               time.Duration(r.integer("CONTEXT_TTL_HOURS", 24)) * time.Hour,
		ContextMaxTurns:          r.integer("CONTEXT_MAX_TURNS", 6),
		ContextMaxInputTokens:    r.integer("CONTEXT_MAX_INPUT_TOKENS", 800),
		RetentionDays:            r.integer("RETENTION_DAYS", 30),
		MaxMorePages:             r.integer("MAX_MORE_PAGES", 1),
		PerNumberRatePerMinute:   r.integer("PER_NUMBER_RATE_LIMIT_PER_MINUTE", 6),
		MaxFailedCodeAttempts:    r.integer("MAX_FAILED_CODE_ATTEMPTS_PER_DAY", 5),
		AllowUCS2LocalLanguage:   r.boolean("ALLOW_UCS2_LOCAL_LANGUAGE", true),

		AdminAPIToken: r.secret("ADMIN_API_TOKEN"),
		MetricsToken:  r.secret("METRICS_TOKEN"),

		GroundingSeedPath: r.str("GROUNDING_SEED_PATH", ""),
		GroundingRefresh:  time.Duration(r.integer("GROUNDING_REFRESH_SECONDS", 300)) * time.Second,

		WorkerConcurrency: r.integer("WORKER_CONCURRENCY", 4),
		WorkerPoll:        r.millis("WORKER_POLL_MS", 500),
		JobLease:          time.Duration(r.integer("JOB_LEASE_SECONDS", 60)) * time.Second,
		JobMaxAttempts:    r.integer("JOB_MAX_ATTEMPTS", 3),
	}
	c.AutoMigrate = r.boolean("AUTO_MIGRATE", c.IsDevelopment())
	// The dialled code MTN is asked to route to us defaults to the one the
	// engine serves; the token endpoint follows the API host unless MTN says
	// otherwise.
	c.MTNUSSDServiceCode = r.str("MTN_USSD_SERVICE_CODE", c.USSDServiceCode)
	if c.MTNTokenURL == "" {
		c.MTNTokenURL = c.MTNBaseURL + "/v1/oauth/access_token"
	}

	prices, err := parsePrices(r.str("LLM_PRICES", ""))
	if err != nil {
		r.errs = append(r.errs, err)
	}
	c.LLMPrices = prices

	c.EncryptionKeys, c.HMACKeys = map[int][]byte{}, map[int][]byte{}
	for k, v := range env {
		if isPlaceholder(v) {
			continue
		}
		var target map[int][]byte
		var suffix string
		switch {
		case strings.HasPrefix(k, "ENCRYPTION_KEYS_V"):
			target, suffix = c.EncryptionKeys, strings.TrimPrefix(k, "ENCRYPTION_KEYS_V")
		case strings.HasPrefix(k, "HMAC_KEYS_V"):
			target, suffix = c.HMACKeys, strings.TrimPrefix(k, "HMAC_KEYS_V")
		default:
			continue
		}
		version, err := strconv.Atoi(suffix)
		if err != nil || version < 1 || version > 32767 {
			r.errs = append(r.errs, fmt.Errorf("%s: key version suffix must be a positive integer", k))
			continue
		}
		key, err := decodeKey(v)
		if err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s: %w", k, err))
			continue
		}
		target[version] = key
	}
	if len(c.EncryptionKeys) == 0 && len(c.HMACKeys) == 0 && c.IsDevelopment() {
		// Deterministic, publicly known keys so a local database survives
		// restarts. Validate refuses to run with these outside development.
		c.EncryptionKeys[c.CurrentKeyVersion] = devKey("encryption")
		c.HMACKeys[c.CurrentKeyVersion] = devKey("hmac")
		c.InsecureDevKeys = true
	}

	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	return c, nil
}

// Validate checks cross-field rules. Production rules are deliberately strict.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Env {
	case "development", "staging", "production", "test":
	default:
		add("APP_ENV must be development, test, staging or production (got %q)", c.Env)
	}
	if c.DatabaseURL == "" {
		add("DATABASE_URL is required")
	}
	if k, ok := c.EncryptionKeys[c.CurrentKeyVersion]; !ok {
		add("ENCRYPTION_KEYS_V%d is required for CURRENT_KEY_VERSION=%d", c.CurrentKeyVersion, c.CurrentKeyVersion)
	} else if len(k) != 32 {
		add("ENCRYPTION_KEYS_V%d must decode to exactly 32 bytes", c.CurrentKeyVersion)
	}
	if k, ok := c.HMACKeys[c.CurrentKeyVersion]; !ok {
		add("HMAC_KEYS_V%d is required for CURRENT_KEY_VERSION=%d", c.CurrentKeyVersion, c.CurrentKeyVersion)
	} else if len(k) < 32 {
		add("HMAC_KEYS_V%d must decode to at least 32 bytes", c.CurrentKeyVersion)
	}
	for v, k := range c.EncryptionKeys {
		if len(k) != 32 {
			add("ENCRYPTION_KEYS_V%d must decode to exactly 32 bytes", v)
		}
	}
	switch c.LLMProvider {
	case "deepinfra":
		if c.DeepInfraAPIKey == "" {
			add("DEEPINFRA_API_KEY is required when LLM_PROVIDER=deepinfra")
		}
		if c.LLMModel == "" {
			add("LLM_MODEL is required")
		}
	case "mock":
	default:
		add("LLM_PROVIDER must be deepinfra or mock (got %q)", c.LLMProvider)
	}
	if c.InboundDebounce < 0 || c.CoalesceExtend < 0 {
		add("debounce durations must not be negative")
	}
	if c.USSDMaxChars < 80 {
		add("USSD_MAX_CHARS must be at least 80")
	}
	if c.MaxMorePages < 0 {
		add("MAX_MORE_PAGES must not be negative")
	}
	if c.WorkerConcurrency < 1 {
		add("WORKER_CONCURRENCY must be at least 1")
	}
	if c.JobMaxAttempts < 1 {
		add("JOB_MAX_ATTEMPTS must be at least 1")
	}
	if c.GlobalDailySpendCapUSD <= 0 || c.GlobalDailySMSSegmentCap <= 0 || c.DailyPerNumberFreeCap < 0 {
		add("spend and segment caps must be positive")
	}
	for _, p := range []struct{ name, value string }{
		{"PRIMARY_SMS_PROVIDER", c.PrimarySMSProvider},
		{"PRIMARY_USSD_PROVIDER", c.PrimaryUSSDProvider},
	} {
		if p.value == "" {
			add("%s is required: name a real SMS/USSD provider (\"mock\" is accepted only in development and test)", p.name)
		}
	}
	c.validateArkesel(add)
	c.validateHubtel(add)
	c.validateMTN(add)
	c.validateMNotify(add)

	if !c.IsDevelopment() && c.Env != "test" {
		if c.InsecureDevKeys {
			add("insecure development keys are not allowed when APP_ENV=%s", c.Env)
		}
		if len(c.AdminAPIToken) < 24 {
			add("ADMIN_API_TOKEN (24+ chars) is required when APP_ENV=%s", c.Env)
		}
		if len(c.MetricsToken) < 24 {
			add("METRICS_TOKEN (24+ chars) is required when APP_ENV=%s", c.Env)
		}
	}
	if !c.AllowsMock() {
		if c.LLMProvider == "mock" {
			add("LLM_PROVIDER=mock is only allowed when APP_ENV is development or test (APP_ENV=%s)", c.Env)
		}
		if c.PrimarySMSProvider == "mock" || c.PrimaryUSSDProvider == "mock" {
			add("mock SMS/USSD providers are only allowed when APP_ENV is development or test (APP_ENV=%s)", c.Env)
		}
	}
	return errors.Join(errs...)
}

// AllowsMock reports whether simulators (mock gateway, mock model, the
// simulator page) may run: development and automated tests only. Staging and
// production talk to the real providers.
func (c *Config) AllowsMock() bool { return c.IsDevelopment() || c.Env == "test" }

func (c *Config) validateArkesel(add func(format string, args ...any)) {
	// Plain HTTP only for local fakes and tunnels.
	strict := !c.IsDevelopment() && c.Env != "test"
	if c.PrimarySMSProvider == "arkesel" {
		if c.ArkeselAPIKey == "" {
			add("ARKESEL_API_KEY is required when PRIMARY_SMS_PROVIDER=arkesel")
		}
		if n := utf8.RuneCountInString(c.SMSOutboundSenderID); n < 1 || n > 11 {
			add("SMS_OUTBOUND_SENDER_ID must be 1-11 characters for Arkesel (got %d)", n)
		}
		if c.SMSEnvironment != "sandbox" && c.SMSEnvironment != "live" {
			add("SMS_ENVIRONMENT must be sandbox or live when PRIMARY_SMS_PROVIDER=arkesel (got %q)", c.SMSEnvironment)
		}
		if _, err := checkURL(c.ArkeselBaseURL, strict); err != nil {
			add("ARKESEL_BASE_URL: %v", err)
		}
		if c.ArkeselDLRCallbackURL != "" {
			if u, err := checkURL(c.ArkeselDLRCallbackURL, strict); err != nil {
				add("ARKESEL_DLR_CALLBACK_URL: %v", err)
			} else if u.RawQuery != "" {
				add("ARKESEL_DLR_CALLBACK_URL must not have a query string: Arkesel appends and signs its own")
			}
			if c.ArkeselWebhookSecret == "" {
				add("ARKESEL_WEBHOOK_SECRET is required with ARKESEL_DLR_CALLBACK_URL; unsigned delivery reports are rejected")
			}
		}
	}
	if c.PrimaryUSSDProvider == "arkesel" {
		if len(c.ArkeselUSSDTokens) == 0 {
			add("ARKESEL_USSD_TOKEN is required when PRIMARY_USSD_PROVIDER=arkesel: Arkesel does not sign USSD callbacks")
		}
		for _, t := range c.ArkeselUSSDTokens {
			if len(t) < 24 {
				add("every ARKESEL_USSD_TOKEN value must be at least 24 characters")
				break
			}
		}
	}
}

func (c *Config) validateHubtel(add func(format string, args ...any)) {
	strict := !c.IsDevelopment() && c.Env != "test"
	if c.PrimarySMSProvider == "hubtel" {
		if c.HubtelClientID == "" || c.HubtelClientSecret == "" {
			add("HUBTEL_CLIENT_ID and HUBTEL_CLIENT_SECRET are required when PRIMARY_SMS_PROVIDER=hubtel")
		}
		if n := utf8.RuneCountInString(c.SMSOutboundSenderID); n < 1 || n > 11 {
			add("SMS_OUTBOUND_SENDER_ID must be 1-11 characters for Hubtel (got %d)", n)
		}
		// Hubtel documents no sandbox flag, so every send is real and billed.
		// Requiring "live" makes that an explicit choice rather than a default.
		if c.SMSEnvironment != "live" {
			add("SMS_ENVIRONMENT must be live when PRIMARY_SMS_PROVIDER=hubtel: Hubtel has no sandbox mode, so every message is sent and billed (got %q)", c.SMSEnvironment)
		}
		if _, err := checkURL(c.HubtelSMSBaseURL, strict); err != nil {
			add("HUBTEL_SMS_BASE_URL: %v", err)
		}
	}
	if c.PrimaryUSSDProvider == "hubtel" && len(c.HubtelUSSDTokens) == 0 {
		add("HUBTEL_USSD_TOKEN is required when PRIMARY_USSD_PROVIDER=hubtel: Hubtel does not sign USSD callbacks")
	}
	if c.PrimarySMSProvider == "hubtel" || c.PrimaryUSSDProvider == "hubtel" {
		for _, t := range [][]string{c.HubtelUSSDTokens, c.HubtelDLRTokens} {
			for _, v := range t {
				if len(v) < 24 {
					add("every HUBTEL_USSD_TOKEN and HUBTEL_DLR_TOKEN value must be at least 24 characters")
					return
				}
			}
		}
	}
}

func (c *Config) validateMTN(add func(format string, args ...any)) {
	sms, ussd := c.PrimarySMSProvider == "mtn", c.PrimaryUSSDProvider == "mtn"
	if !sms && !ussd {
		return
	}
	strict := !c.IsDevelopment() && c.Env != "test"
	if sms {
		if c.MTNClientID == "" || c.MTNClientSecret == "" {
			add("MTN_CLIENT_ID and MTN_CLIENT_SECRET are required when PRIMARY_SMS_PROVIDER=mtn")
		}
		if n := utf8.RuneCountInString(c.SMSOutboundSenderID); n < 1 || n > 11 {
			add("SMS_OUTBOUND_SENDER_ID must be 1-11 characters for MTN (got %d)", n)
		}
		// MTN has no sandbox flag; the environment is the host. Tying
		// SMS_ENVIRONMENT to it makes real, billed sending an explicit choice
		// and keeps a sandbox trial from silently using production.
		if c.SMSEnvironment != "sandbox" && c.SMSEnvironment != "live" {
			add("SMS_ENVIRONMENT must be sandbox or live when PRIMARY_SMS_PROVIDER=mtn (got %q)", c.SMSEnvironment)
		}
		switch kind := mtnHostKind(c.MTNBaseURL); {
		case c.SMSEnvironment == "live" && kind == "sandbox":
			add("SMS_ENVIRONMENT=live but MTN_BASE_URL is MTN's sandbox host: nothing would be delivered")
		case c.SMSEnvironment == "sandbox" && kind == "production":
			add("MTN_BASE_URL is MTN's production host, so messages would be real and billed: set SMS_ENVIRONMENT=live, or point MTN_BASE_URL at the sandbox host")
		}
	}
	if ussd && len(c.MTNUSSDTokens) == 0 {
		add("MTN_USSD_TOKEN is required when PRIMARY_USSD_PROVIDER=mtn: MTN does not authenticate its USSD callbacks")
	}
	if _, err := checkURL(c.MTNBaseURL, strict); err != nil {
		add("MTN_BASE_URL: %v", err)
	}
	if _, err := checkURL(c.MTNTokenURL, strict); err != nil {
		add("MTN_TOKEN_URL: %v", err)
	}
	for _, t := range [][]string{c.MTNUSSDTokens, c.MTNDLRTokens} {
		for _, v := range t {
			if len(v) < 24 {
				add("every MTN_USSD_TOKEN and MTN_DLR_TOKEN value must be at least 24 characters")
				return
			}
		}
	}
}

// validateMNotify checks the mNotify settings for the roles mNotify has been
// given. No message below repeats a configured value that could hold the API
// key: it travels in the query string of outbound calls, so a key pasted into
// the wrong variable must not come back out through an error.
func (c *Config) validateMNotify(add func(format string, args ...any)) {
	sms, ussd := c.PrimarySMSProvider == "mnotify", c.PrimaryUSSDProvider == "mnotify"
	if !sms && !ussd {
		return
	}
	strict := !c.IsDevelopment() && c.Env != "test"
	if sms {
		switch {
		case c.MNotifyAPIKey == "":
			add("MNOTIFY_API_KEY is required when PRIMARY_SMS_PROVIDER=mnotify")
		case strings.IndexFunc(c.MNotifyAPIKey, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '"' || r == '&' || r == '#' }) >= 0:
			add("MNOTIFY_API_KEY contains whitespace or one of \" & #: copy only the key itself from the mNotify dashboard")
		}
		if n := utf8.RuneCountInString(c.SMSOutboundSenderID); n < 1 || n > 11 {
			add("SMS_OUTBOUND_SENDER_ID must be 1-11 characters for mNotify (got %d)", n)
		}
		// mNotify documents no sandbox or test mode, so every send is real and
		// billed. Requiring "live" makes that an explicit choice.
		if c.SMSEnvironment != "live" {
			add("SMS_ENVIRONMENT must be live when PRIMARY_SMS_PROVIDER=mnotify: mNotify documents no sandbox mode, so every message is sent and billed (got %q)", c.SMSEnvironment)
		}
	}
	if ussd {
		switch c.MNotifyUSSDMode {
		case "auto", "shared", "dedicated":
		default:
			add("MNOTIFY_USSD_MODE must be auto, shared or dedicated (got %q)", c.MNotifyUSSDMode)
		}
		if len(c.MNotifyUSSDTokens) == 0 {
			add("MNOTIFY_USSD_TOKEN is required when PRIMARY_USSD_PROVIDER=mnotify: mNotify documents no authentication for its USSD callbacks")
		}
	}
	for _, v := range c.MNotifyUSSDTokens {
		if len(v) < 24 {
			add("every MNOTIFY_USSD_TOKEN value must be at least 24 characters")
			break
		}
	}
	if u, err := url.Parse(c.MNotifyBaseURL); err == nil && (u.RawQuery != "" || u.Fragment != "" || u.User != nil) {
		add("MNOTIFY_BASE_URL must not contain a query string, fragment or credentials: the client adds ?key= itself")
	} else if _, err := checkURL(c.MNotifyBaseURL, strict); err != nil {
		add("MNOTIFY_BASE_URL must be an absolute https URL (http only in development and test); check the value in your env file")
	}
}

// mtnHostKind classifies an MTN API base URL: "production" for api.mtn.com and
// its per-country hosts, "sandbox" for sandbox.api.mtn.com (the sandbox host
// MTN's developer notes name), "other" for anything else, such as a local fake.
func mtnHostKind(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "other"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "sandbox.api.mtn.com":
		return "sandbox"
	case host == "api.mtn.com" || strings.HasSuffix(host, ".api.mtn.com"):
		return "production"
	}
	return "other"
}

// ValidateMTNRegistration checks what `admin mtn-subscribe` needs for the
// callbacks it is asked to register, whichever providers are selected.
func (c *Config) ValidateMTNRegistration(ussd, sms bool) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	strict := !c.IsDevelopment() && c.Env != "test"
	if _, err := checkURL(c.MTNBaseURL, strict); err != nil {
		add("MTN_BASE_URL: %v", err)
	}
	if _, err := checkURL(c.MTNTokenURL, strict); err != nil {
		add("MTN_TOKEN_URL: %v", err)
	}
	if c.MTNCallbackBaseURL == "" {
		add("MTN_CALLBACK_BASE_URL is required: the public https origin MTN will call, e.g. https://sms.example.com")
	} else if u, err := checkURL(c.MTNCallbackBaseURL, strict); err != nil {
		add("MTN_CALLBACK_BASE_URL: %v", err)
	} else if u.RawQuery != "" || strings.Trim(u.Path, "/") != "" {
		add("MTN_CALLBACK_BASE_URL must be just the origin, without a path or query")
	}
	if c.MTNTargetSystem == "" {
		add("MTN_TARGET_SYSTEM must not be empty")
	}
	if ussd {
		if c.MTNAPIKey == "" && c.MTNClientID == "" {
			add("MTN_API_KEY (or MTN_CLIENT_ID, which MTN's FAQ names as the API key) is required to register a USSD callback")
		}
		if len(c.MTNUSSDTokens) == 0 {
			add("MTN_USSD_TOKEN is required: it becomes the ?token= of the USSD callback URL")
		}
		if c.MTNUSSDServiceCode == "" {
			add("MTN_USSD_SERVICE_CODE (or USSD_SERVICE_CODE) is required")
		}
		if c.MTNUSSDSendsBearer && (c.MTNClientID == "" || c.MTNClientSecret == "") {
			add("MTN_USSD_SEND_BEARER needs MTN_CLIENT_ID and MTN_CLIENT_SECRET")
		}
	}
	if sms {
		if c.MTNClientID == "" || c.MTNClientSecret == "" {
			add("MTN_CLIENT_ID and MTN_CLIENT_SECRET are required to register the SMS callbacks")
		}
		if c.MTNSMSSendsAPIKey && c.MTNAPIKey == "" && c.MTNClientID == "" {
			add("MTN_SMS_SEND_API_KEY needs MTN_API_KEY (or MTN_CLIENT_ID)")
		}
		if len(c.MTNDLRTokens) == 0 {
			add("MTN_DLR_TOKEN is required: it becomes the ?token= of the delivery-notification URL")
		}
		if c.SMSOutboundSenderID == "" {
			add("SMS_OUTBOUND_SENDER_ID is required: the delivery subscription belongs to a sender address")
		}
		if c.MTNCountryCode == "" {
			add("MTN_COUNTRY_CODE must not be empty: the SMS subscription calls need an x-country-code header")
		}
	}
	return errors.Join(errs...)
}

func checkURL(raw string, requireHTTPS bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return nil, fmt.Errorf("%q is not an absolute http(s) URL", raw)
	case requireHTTPS && u.Scheme != "https":
		return nil, fmt.Errorf("%q must use https", raw)
	}
	return u, nil
}

// KeyVersions returns configured key versions in ascending order.
func (c *Config) KeyVersions() []int {
	seen := map[int]bool{}
	for v := range c.EncryptionKeys {
		seen[v] = true
	}
	for v := range c.HMACKeys {
		seen[v] = true
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

type reader struct {
	env  map[string]string
	errs []error
}

func (r *reader) str(key, def string) string {
	if v, ok := r.env[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// secret returns "" for unset values and for obvious placeholders copied
// from .env.example, so a placeholder never masquerades as a real secret.
func (r *reader) secret(key string) string {
	v := r.str(key, "")
	if isPlaceholder(v) {
		return ""
	}
	return v
}

// secretList splits a comma-separated secret, dropping blanks and
// placeholders.
func (r *reader) secretList(key string) []string {
	var out []string
	for _, v := range strings.Split(r.str(key, ""), ",") {
		if v = strings.TrimSpace(v); v != "" && !isPlaceholder(v) {
			out = append(out, v)
		}
	}
	return out
}

// prefixes parses a comma-separated list of IP addresses and CIDR ranges.
func (r *reader) prefixes(key string) []netip.Prefix {
	var out []netip.Prefix
	for _, v := range strings.Split(r.str(key, ""), ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		p, err := netip.ParsePrefix(v)
		if err != nil {
			addr, aerr := netip.ParseAddr(v)
			if aerr != nil {
				r.errs = append(r.errs, fmt.Errorf("%s: %q is not an IP address or CIDR range", key, v))
				continue
			}
			p = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		out = append(out, p.Masked())
	}
	return out
}

func (r *reader) integer(key string, def int) int {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

func (r *reader) float(key string, def float64) float64 {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not a number", key, v))
		return def
	}
	return f
}

func (r *reader) boolean(key string, def bool) bool {
	v := strings.ToLower(r.str(key, ""))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	r.errs = append(r.errs, fmt.Errorf("%s: %q is not a boolean", key, v))
	return def
}

func (r *reader) millis(key string, def int) time.Duration {
	return time.Duration(r.integer(key, def)) * time.Millisecond
}

func isPlaceholder(v string) bool {
	u := strings.ToUpper(strings.TrimSpace(v))
	return strings.HasPrefix(u, "REPLACE_") || strings.HasPrefix(u, "CHANGE_ME") || strings.HasPrefix(u, "SET_AFTER")
}

func decodeKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("value is not valid base64")
}

func devKey(purpose string) []byte {
	sum := sha256.Sum256([]byte("smsai-ghana-INSECURE-development-only-" + purpose))
	return sum[:]
}

// parsePrices parses "model=in:out,model2=in:out" (USD per 1M tokens).
func parsePrices(s string) (map[string]ModelPrice, error) {
	out := map[string]ModelPrice{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		model, pair, ok := strings.Cut(item, "=")
		in, outp, ok2 := strings.Cut(pair, ":")
		if !ok || !ok2 {
			return nil, fmt.Errorf("LLM_PRICES: %q must look like model=in:out", item)
		}
		inF, err1 := strconv.ParseFloat(strings.TrimSpace(in), 64)
		outF, err2 := strconv.ParseFloat(strings.TrimSpace(outp), 64)
		if err1 != nil || err2 != nil || inF < 0 || outF < 0 {
			return nil, fmt.Errorf("LLM_PRICES: %q has invalid prices", item)
		}
		out[strings.TrimSpace(model)] = ModelPrice{InputPerM: inF, OutputPerM: outF}
	}
	return out, nil
}
