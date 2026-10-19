package mtn_test

import (
	"strings"
	"testing"

	"smsaighana/internal/channel/mtn"
	"smsaighana/internal/config"
)

// TestConfigValidation pins the configuration guards that keep the MTN
// adapters from running unauthenticated, silently billing, or with values MTN
// would reject.
func TestConfigValidation(t *testing.T) {
	token := strings.Repeat("t", 24)
	base := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=mtn", "PRIMARY_USSD_PROVIDER=mtn", "SMS_ENVIRONMENT=live",
		"MTN_CLIENT_ID=id", "MTN_CLIENT_SECRET=secret", "MTN_USSD_TOKEN=" + token,
	}
	staging := []string{"APP_ENV=staging", "ADMIN_API_TOKEN=" + strings.Repeat("x", 24), "METRICS_TOKEN=" + strings.Repeat("y", 24)}
	cases := []struct {
		name  string
		extra []string
		want  string // substring of the error; empty means valid
	}{
		{"minimal valid", nil, ""},
		{"with a delivery-notification token", []string{"MTN_DLR_TOKEN=" + strings.Repeat("d", 24)}, ""},
		{"two USSD tokens while rotating", []string{"MTN_USSD_TOKEN=" + strings.Repeat("a", 24) + ", " + strings.Repeat("b", 24)}, ""},
		{"no USSD token", []string{"MTN_USSD_TOKEN="}, "MTN_USSD_TOKEN is required"},
		{"placeholder USSD token", []string{"MTN_USSD_TOKEN=REPLACE_VIA_SECRET_MANAGER"}, "MTN_USSD_TOKEN is required"},
		{"short USSD token", []string{"MTN_USSD_TOKEN=short"}, "at least 24 characters"},
		{"short delivery-notification token", []string{"MTN_DLR_TOKEN=short"}, "at least 24 characters"},
		{"no client id", []string{"MTN_CLIENT_ID=REPLACE_VIA_SECRET_MANAGER"}, "MTN_CLIENT_ID and MTN_CLIENT_SECRET are required"},
		{"no client secret", []string{"MTN_CLIENT_SECRET="}, "MTN_CLIENT_ID and MTN_CLIENT_SECRET are required"},
		{"sender ID too long", []string{"SMS_OUTBOUND_SENDER_ID=SMS-AI-GHANA"}, "1-11 characters"},
		{"unknown SMS environment", []string{"SMS_ENVIRONMENT=production"}, "must be sandbox or live"},
		{"relative base URL", []string{"MTN_BASE_URL=/v1"}, "not an absolute"},
		{"relative token URL", []string{"MTN_TOKEN_URL=/oauth"}, "not an absolute"},
		{"plain HTTP base URL in staging", append([]string{"MTN_BASE_URL=http://api.mtn.com"}, staging...), "must use https"},
		{"plain HTTP token URL in staging", append([]string{"MTN_TOKEN_URL=http://api.mtn.com/v1/oauth/access_token"}, staging...), "must use https"},
		{"plain HTTP base URL for a local fake in test", []string{"MTN_BASE_URL=http://127.0.0.1:9999", "SMS_ENVIRONMENT=sandbox"}, ""},

		// The environment must agree with the host, so a sandbox trial cannot
		// silently bill and a "live" system cannot silently send nowhere.
		{"sandbox environment on the production host", []string{"SMS_ENVIRONMENT=sandbox"}, "real and billed"},
		{"sandbox is the default environment", []string{"SMS_ENVIRONMENT="}, "real and billed"},
		{"sandbox environment on the sandbox host", []string{"SMS_ENVIRONMENT=sandbox", "MTN_BASE_URL=https://sandbox.api.mtn.com"}, ""},
		{"live environment on the sandbox host", []string{"MTN_BASE_URL=https://sandbox.api.mtn.com"}, "MTN's sandbox host"},
		{"live environment on a country host", []string{"MTN_BASE_URL=https://ghana.api.mtn.com"}, ""},
		{"sandbox environment on a country host", []string{"SMS_ENVIRONMENT=sandbox", "MTN_BASE_URL=https://ghana.api.mtn.com"}, "real and billed"},

		{"USSD only needs no SMS credentials or live mode", []string{"PRIMARY_SMS_PROVIDER=mock", "MTN_CLIENT_ID=", "MTN_CLIENT_SECRET=", "SMS_ENVIRONMENT=sandbox"}, ""},
		{"SMS only needs no USSD token", []string{"PRIMARY_USSD_PROVIDER=mock", "MTN_USSD_TOKEN="}, ""},
	}
	for _, c := range cases {
		cfg, err := config.LoadFrom(append(append([]string{}, base...), c.extra...))
		if err == nil {
			err = cfg.Validate()
		}
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}

	// MTN settings are not validated when MTN is not selected.
	idle := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=mock", "PRIMARY_USSD_PROVIDER=mock", "MTN_USSD_TOKEN=short", "MTN_BASE_URL=/nope",
	}
	if cfg, err := config.LoadFrom(idle); err != nil || cfg.Validate() != nil {
		t.Errorf("unselected MTN must not be validated: %v", err)
	}

	// Neither provider may be left unset.
	for _, env := range []string{"PRIMARY_SMS_PROVIDER=", "PRIMARY_USSD_PROVIDER="} {
		cfg, err := config.LoadFrom(append(append([]string{}, base...), env))
		if err == nil {
			err = cfg.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), "is required") {
			t.Errorf("%s: want a required-provider error, got %v", env, err)
		}
	}
}

func TestConfigDefaultsAndAllowlists(t *testing.T) {
	base := []string{
		"APP_ENV=test", "PRIMARY_SMS_PROVIDER=mtn", "PRIMARY_USSD_PROVIDER=mtn",
	}
	cfg, err := config.LoadFrom(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTNBaseURL != "https://api.mtn.com" || cfg.MTNTokenURL != "https://api.mtn.com/v1/oauth/access_token" ||
		cfg.MTNTargetSystem != "sms-ai-ghana" || cfg.MTNUSSDServiceCode != cfg.USSDServiceCode || cfg.MTNCountryCode != "GH" || cfg.MTNSMSSendsAPIKey || cfg.MTNUSSDSendsBearer || cfg.MTNSMSLookupByClientRef ||
		cfg.MTNAPIKey != "" || cfg.MTNUSSDInputIsHistory || cfg.MTNCallbackBaseURL != "" {
		t.Errorf("defaults: %+v", cfg)
	}

	cfg, err = config.LoadFrom(append(base,
		"MTN_BASE_URL=https://sandbox.api.mtn.com/", "MTN_CALLBACK_BASE_URL=https://sms.example.com/",
		"MTN_USSD_SERVICE_CODE=*123#", "MTN_COUNTRY_CODE=GHA", "MTN_SMS_SEND_API_KEY=true", "MTN_USSD_SEND_BEARER=true", "MTN_SMS_LOOKUP_BY_CLIENT_REF=true", "MTN_USSD_INPUT_IS_HISTORY=true",
		"MTN_USSD_ALLOWED_IPS=203.0.113.9/24, 198.51.100.7", "MTN_DLR_ALLOWED_IPS=::1", "MTN_CLIENT_IP_HEADER=X-Real-IP",
		"MTN_USSD_SUBSCRIPTION_ID=u1", "MTN_SMS_SUBSCRIPTION_ID=s1"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTNBaseURL != "https://sandbox.api.mtn.com" || cfg.MTNTokenURL != "https://sandbox.api.mtn.com/v1/oauth/access_token" {
		t.Errorf("base %q token %q (the token URL follows the base URL)", cfg.MTNBaseURL, cfg.MTNTokenURL)
	}
	if cfg.MTNCallbackBaseURL != "https://sms.example.com" || cfg.MTNUSSDServiceCode != "*123#" || cfg.MTNCountryCode != "GHA" || !cfg.MTNSMSSendsAPIKey || !cfg.MTNUSSDSendsBearer || !cfg.MTNSMSLookupByClientRef ||
		!cfg.MTNUSSDInputIsHistory || cfg.MTNUSSDSubscriptionID != "u1" || cfg.MTNSMSSubscriptionID != "s1" {
		t.Errorf("overrides: %+v", cfg)
	}
	var ussd, dlr []string
	for _, p := range cfg.MTNUSSDAllowedIPs {
		ussd = append(ussd, p.String())
	}
	for _, p := range cfg.MTNDLRAllowedIPs {
		dlr = append(dlr, p.String())
	}
	if strings.Join(ussd, ",") != "203.0.113.0/24,198.51.100.7/32" || strings.Join(dlr, ",") != "::1/128" || cfg.MTNClientIPHeader != "X-Real-IP" {
		t.Errorf("allowlists parsed as %v / %v, header %q", ussd, dlr, cfg.MTNClientIPHeader)
	}
	if cfg, err := config.LoadFrom(append(base, "MTN_TOKEN_URL=https://api.mtn.com/v1/oauth/access_token/accesstoken")); err != nil ||
		cfg.MTNTokenURL != "https://api.mtn.com/v1/oauth/access_token/accesstoken" {
		t.Errorf("explicit token URL: %v", err)
	}
	if _, err := config.LoadFrom(append(base, "MTN_DLR_ALLOWED_IPS=203.0.113.0/24, nope")); err == nil || !strings.Contains(err.Error(), `"nope" is not an IP address`) {
		t.Errorf("bad allowlist entry: %v", err)
	}
	if _, err := config.LoadFrom(append(base, "MTN_USSD_INPUT_IS_HISTORY=maybe")); err == nil {
		t.Error("bad boolean")
	}
	for _, name := range []string{"MTN_SMS_SEND_API_KEY", "MTN_USSD_SEND_BEARER", "MTN_SMS_LOOKUP_BY_CLIENT_REF"} {
		if _, err := config.LoadFrom(append(base, name+"=maybe")); err == nil {
			t.Errorf("%s: bad boolean accepted", name)
		}
	}
	// Placeholders copied from .env.example never count as credentials.
	cfg, _ = config.LoadFrom(append(base, "MTN_CLIENT_ID=REPLACE_VIA_SECRET_MANAGER", "MTN_API_KEY=CHANGE_ME", "MTN_DLR_TOKEN=SET_AFTER_REGISTERING"))
	if cfg.MTNClientID != "" || cfg.MTNAPIKey != "" || len(cfg.MTNDLRTokens) != 0 {
		t.Errorf("placeholders accepted: %+v", cfg)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg, err := config.LoadFrom([]string{
		"APP_ENV=test", "MTN_CLIENT_ID=id", "MTN_CLIENT_SECRET=secret", "MTN_API_KEY=key", "MTN_BASE_URL=http://127.0.0.1:1",
		"MTN_USSD_TOKEN=" + strings.Repeat("u", 24), "MTN_DLR_TOKEN=" + strings.Repeat("d", 24),
		"USSD_SERVICE_CODE=*713*1#", "MTN_COUNTRY_CODE=GHA", "MTN_SMS_SEND_API_KEY=true", "MTN_USSD_SEND_BEARER=true", "MTN_SMS_LOOKUP_BY_CLIENT_REF=true", "MTN_USSD_INPUT_IS_HISTORY=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	o := mtn.OptionsFromConfig(cfg, nil)
	if o.ClientID != "id" || o.ClientSecret != "secret" || o.APIKey != "key" || o.BaseURL != "http://127.0.0.1:1" ||
		o.TokenURL != "http://127.0.0.1:1/v1/oauth/access_token" || o.USSDServiceCode != "*713*1#" || o.CountryCode != "GHA" || !o.SMSSendsAPIKey || !o.USSDSendsBearer || !o.LookupByClientRef || o.SenderID != cfg.SMSOutboundSenderID ||
		!o.USSDInputIsHistory || len(o.USSDTokens) != 1 || len(o.DLRTokens) != 1 {
		t.Errorf("options: %+v", o)
	}
}

func TestValidateMTNRegistration(t *testing.T) {
	good := []string{
		"APP_ENV=test", "MTN_CLIENT_ID=id", "MTN_CLIENT_SECRET=secret", "MTN_CALLBACK_BASE_URL=https://sms.example.com",
		"MTN_USSD_TOKEN=" + strings.Repeat("u", 24), "MTN_DLR_TOKEN=" + strings.Repeat("d", 24),
	}
	cfg, err := config.LoadFrom(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]bool{{true, true}, {true, false}, {false, true}} {
		if err := cfg.ValidateMTNRegistration(c[0], c[1]); err != nil {
			t.Errorf("ussd=%v sms=%v: %v", c[0], c[1], err)
		}
	}
	// A USSD-only registration does not need SMS credentials, and vice versa.
	noSMS, _ := config.LoadFrom(append(good, "MTN_CLIENT_SECRET=", "MTN_API_KEY=key"))
	if err := noSMS.ValidateMTNRegistration(true, false); err != nil {
		t.Errorf("USSD only: %v", err)
	}
	if err := noSMS.ValidateMTNRegistration(false, true); err == nil {
		t.Error("SMS registration without a client secret should fail")
	}
	noSMS.MTNUSSDSendsBearer = true
	if err := noSMS.ValidateMTNRegistration(true, false); err == nil || !strings.Contains(err.Error(), "MTN_USSD_SEND_BEARER") {
		t.Errorf("USSD bearer credentials were not required: %v", err)
	}
	for _, c := range []struct{ sender, country string }{{"", "GH"}, {"SMS-AI", ""}} {
		copy := *cfg
		copy.SMSOutboundSenderID, copy.MTNCountryCode = c.sender, c.country
		if err := copy.ValidateMTNRegistration(false, true); err == nil {
			t.Errorf("SMS subscription accepted empty sender or country: %+v", c)
		}
	}
	noDLR, _ := config.LoadFrom(append(good, "MTN_DLR_TOKEN="))
	if err := noDLR.ValidateMTNRegistration(true, false); err != nil {
		t.Errorf("USSD only without a delivery token: %v", err)
	}
	if err := noDLR.ValidateMTNRegistration(false, true); err == nil {
		t.Error("SMS registration without a delivery token should fail")
	}
}
