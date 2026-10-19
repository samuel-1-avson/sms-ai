package mnotify_test

import (
	"strings"
	"testing"

	"smsaighana/internal/channel/mnotify"
	"smsaighana/internal/config"
)

const cfgKey = "k3y+ABC/def=GHIJKL0123456789"

// TestConfigValidation pins the guards that keep the mNotify adapter from
// running unauthenticated, silently billing, or with values mNotify would
// reject.
func TestConfigValidation(t *testing.T) {
	token := strings.Repeat("t", 24)
	base := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=mnotify", "PRIMARY_USSD_PROVIDER=mnotify", "SMS_ENVIRONMENT=live",
		"MNOTIFY_API_KEY=" + cfgKey, "MNOTIFY_USSD_TOKEN=" + token,
	}
	staging := []string{
		"APP_ENV=staging", "ADMIN_API_TOKEN=" + strings.Repeat("x", 24), "METRICS_TOKEN=" + strings.Repeat("y", 24),
		"LLM_PROVIDER=deepinfra", "DEEPINFRA_API_KEY=not-a-real-key",
	}
	cases := []struct {
		name  string
		extra []string
		want  string // substring of the error; empty means valid
	}{
		{"minimal valid", nil, ""},
		{"two USSD tokens while rotating", []string{"MNOTIFY_USSD_TOKEN=" + strings.Repeat("a", 24) + ", " + strings.Repeat("b", 24)}, ""},
		{"fixed shared mode", []string{"MNOTIFY_USSD_MODE=shared"}, ""},
		{"fixed dedicated mode, upper case", []string{"MNOTIFY_USSD_MODE=DEDICATED"}, ""},
		{"valid in staging", staging, ""},
		{"no USSD token", []string{"MNOTIFY_USSD_TOKEN="}, "MNOTIFY_USSD_TOKEN is required"},
		{"placeholder USSD token", []string{"MNOTIFY_USSD_TOKEN=REPLACE_VIA_SECRET_MANAGER"}, "MNOTIFY_USSD_TOKEN is required"},
		{"short USSD token", []string{"MNOTIFY_USSD_TOKEN=short"}, "at least 24 characters"},
		{"unknown USSD mode", []string{"MNOTIFY_USSD_MODE=hybrid"}, "must be auto, shared or dedicated"},
		{"no API key", []string{"MNOTIFY_API_KEY="}, "MNOTIFY_API_KEY is required"},
		{"placeholder API key", []string{"MNOTIFY_API_KEY=REPLACE_VIA_SECRET_MANAGER"}, "MNOTIFY_API_KEY is required"},
		{"API key with a space", []string{"MNOTIFY_API_KEY=abc def"}, "whitespace"},
		{"API key with an ampersand", []string{"MNOTIFY_API_KEY=abc&def"}, "whitespace or one of"},
		{"sender ID too long", []string{"SMS_OUTBOUND_SENDER_ID=SMS-AI-GHANA"}, "1-11 characters"},
		// mNotify documents no sandbox: sending is real, so "live" must be chosen.
		{"sandbox environment", []string{"SMS_ENVIRONMENT=sandbox"}, "must be live"},
		{"sandbox is the default environment", []string{"SMS_ENVIRONMENT="}, "must be live"},
		{"relative base URL", []string{"MNOTIFY_BASE_URL=/api"}, "absolute https URL"},
		{"plain HTTP base URL in staging", append([]string{"MNOTIFY_BASE_URL=http://api.mnotify.com/api"}, staging...), "absolute https URL"},
		{"plain HTTP base URL for a local fake in test", []string{"MNOTIFY_BASE_URL=http://127.0.0.1:9999/api"}, ""},
		{"key pasted into the base URL", []string{"MNOTIFY_BASE_URL=https://api.mnotify.com/api?key=" + cfgKey}, "must not contain a query string"},

		{"USSD only needs no SMS key or live mode", []string{"PRIMARY_SMS_PROVIDER=mock", "MNOTIFY_API_KEY=", "SMS_ENVIRONMENT=sandbox"}, ""},
		{"USSD only still needs the token", []string{"PRIMARY_SMS_PROVIDER=mock", "MNOTIFY_USSD_TOKEN="}, "MNOTIFY_USSD_TOKEN is required"},
		{"SMS only needs no USSD token", []string{"PRIMARY_USSD_PROVIDER=mock", "MNOTIFY_USSD_TOKEN="}, ""},
		{"SMS only ignores the USSD mode", []string{"PRIMARY_USSD_PROVIDER=mock", "MNOTIFY_USSD_MODE=hybrid"}, ""},
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
		// No validation message may repeat a value that could hold the key.
		if err != nil && strings.Contains(err.Error(), cfgKey) {
			t.Errorf("%s: the error repeats the API key: %v", c.name, err)
		}
	}

	// mNotify settings are not validated when mNotify is not selected.
	idle := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=mock", "PRIMARY_USSD_PROVIDER=mock", "MNOTIFY_USSD_TOKEN=short", "MNOTIFY_BASE_URL=/nope", "MNOTIFY_USSD_MODE=hybrid",
	}
	if cfg, err := config.LoadFrom(idle); err != nil || cfg.Validate() != nil {
		t.Errorf("unselected mNotify must not be validated: %v", err)
	}

	// Neither provider may be left unset, and "mock" is refused outside tests.
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
	base := []string{"APP_ENV=test", "PRIMARY_SMS_PROVIDER=mnotify", "PRIMARY_USSD_PROVIDER=mnotify"}
	cfg, err := config.LoadFrom(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MNotifyBaseURL != "https://api.mnotify.com/api" || cfg.MNotifyUSSDMode != "auto" || cfg.MNotifyAPIKey != "" ||
		len(cfg.MNotifyUSSDTokens) != 0 || len(cfg.MNotifyUSSDAllowedIPs) != 0 || cfg.MNotifyClientIPHeader != "" {
		t.Errorf("defaults: %+v", cfg)
	}
	if mnotify.DefaultBaseURL != cfg.MNotifyBaseURL {
		t.Errorf("config default %q differs from the package default %q", cfg.MNotifyBaseURL, mnotify.DefaultBaseURL)
	}

	cfg, err = config.LoadFrom(append(base,
		"MNOTIFY_BASE_URL=https://api.mnotify.com/api/", "MNOTIFY_USSD_MODE=Shared",
		"MNOTIFY_USSD_ALLOWED_IPS=203.0.113.9/24, 198.51.100.7", "MNOTIFY_CLIENT_IP_HEADER=X-Real-IP"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MNotifyBaseURL != "https://api.mnotify.com/api" || cfg.MNotifyUSSDMode != "shared" || cfg.MNotifyClientIPHeader != "X-Real-IP" {
		t.Errorf("overrides: %+v", cfg)
	}
	var ips []string
	for _, p := range cfg.MNotifyUSSDAllowedIPs {
		ips = append(ips, p.String())
	}
	if strings.Join(ips, ",") != "203.0.113.0/24,198.51.100.7/32" {
		t.Errorf("allowlist parsed as %v", ips)
	}
	if _, err := config.LoadFrom(append(base, "MNOTIFY_USSD_ALLOWED_IPS=203.0.113.0/24, nope")); err == nil || !strings.Contains(err.Error(), `"nope" is not an IP address`) {
		t.Errorf("bad allowlist entry: %v", err)
	}
	// Placeholders copied from .env.example never count as credentials.
	cfg, _ = config.LoadFrom(append(base, "MNOTIFY_API_KEY=REPLACE_VIA_SECRET_MANAGER", "MNOTIFY_USSD_TOKEN=CHANGE_ME"))
	if cfg.MNotifyAPIKey != "" || len(cfg.MNotifyUSSDTokens) != 0 {
		t.Errorf("placeholders accepted: %+v", cfg)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg, err := config.LoadFrom([]string{
		"APP_ENV=test", "MNOTIFY_API_KEY=" + cfgKey, "MNOTIFY_BASE_URL=http://127.0.0.1:1/api", "MNOTIFY_USSD_MODE=dedicated",
		"MNOTIFY_USSD_TOKEN=" + strings.Repeat("u", 24), "MNOTIFY_USSD_ALLOWED_IPS=203.0.113.0/24", "MNOTIFY_CLIENT_IP_HEADER=X-Real-IP",
	})
	if err != nil {
		t.Fatal(err)
	}
	o := mnotify.OptionsFromConfig(cfg, nil)
	if o.APIKey != cfgKey || o.BaseURL != "http://127.0.0.1:1/api" || o.USSDMode != "dedicated" || len(o.USSDTokens) != 1 ||
		len(o.USSDAllowedIPs) != 1 || o.ClientIPHeader != "X-Real-IP" {
		t.Errorf("options: %+v", o)
	}
}
