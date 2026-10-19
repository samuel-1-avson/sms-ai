package arkesel_test

import (
	"strings"
	"testing"

	"smsaighana/internal/config"
)

// TestConfigValidation pins the configuration guards that keep the Arkesel
// adapters from running unauthenticated or with values Arkesel rejects.
func TestConfigValidation(t *testing.T) {
	base := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=arkesel", "PRIMARY_USSD_PROVIDER=arkesel",
		"ARKESEL_API_KEY=key", "ARKESEL_USSD_TOKEN=" + strings.Repeat("t", 24),
	}
	cases := []struct {
		name  string
		extra []string
		want  string // substring of the error; empty means valid
	}{
		{"minimal valid", nil, ""},
		{"two tokens while rotating", []string{"ARKESEL_USSD_TOKEN=" + strings.Repeat("a", 24) + ", " + strings.Repeat("b", 24)}, ""},
		{"no USSD token", []string{"ARKESEL_USSD_TOKEN="}, "ARKESEL_USSD_TOKEN is required"},
		{"placeholder USSD token", []string{"ARKESEL_USSD_TOKEN=REPLACE_VIA_SECRET_MANAGER"}, "ARKESEL_USSD_TOKEN is required"},
		{"short USSD token", []string{"ARKESEL_USSD_TOKEN=short"}, "at least 24 characters"},
		{"no API key", []string{"ARKESEL_API_KEY=REPLACE_VIA_SECRET_MANAGER"}, "ARKESEL_API_KEY is required"},
		{"sender ID too long", []string{"SMS_OUTBOUND_SENDER_ID=SMS-AI-GHANA"}, "1-11 characters"},
		{"unknown SMS environment", []string{"SMS_ENVIRONMENT=prod"}, "SMS_ENVIRONMENT must be sandbox or live"},
		{"receipts without secret", []string{"ARKESEL_DLR_CALLBACK_URL=https://api.example.test/webhooks/sms/arkesel/status"}, "ARKESEL_WEBHOOK_SECRET is required"},
		{"receipt URL with query", []string{"ARKESEL_WEBHOOK_SECRET=s", "ARKESEL_DLR_CALLBACK_URL=https://api.example.test/x?a=1"}, "must not have a query string"},
		{"relative receipt URL", []string{"ARKESEL_WEBHOOK_SECRET=s", "ARKESEL_DLR_CALLBACK_URL=/webhooks/sms/arkesel/status"}, "not an absolute"},
		{"receipts with secret", []string{"ARKESEL_WEBHOOK_SECRET=s", "ARKESEL_DLR_CALLBACK_URL=https://api.example.test/webhooks/sms/arkesel/status"}, ""},
		{"plain HTTP API in staging", []string{"APP_ENV=staging", "ADMIN_API_TOKEN=" + strings.Repeat("x", 24),
			"METRICS_TOKEN=" + strings.Repeat("y", 24), "ARKESEL_BASE_URL=http://sms.arkesel.com"}, "must use https"},
		{"USSD only needs no API key", []string{"PRIMARY_SMS_PROVIDER=mock", "ARKESEL_API_KEY="}, ""},
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

	if _, err := config.LoadFrom(append(append([]string{}, base...), "ARKESEL_USSD_ALLOWED_IPS=203.0.113.0/24, 198.51.100.7, nope")); err == nil ||
		!strings.Contains(err.Error(), `"nope" is not an IP address`) {
		t.Errorf("bad allowlist entry: %v", err)
	}
	cfg, err := config.LoadFrom(append(append([]string{}, base...), "ARKESEL_USSD_ALLOWED_IPS=203.0.113.9/24, 198.51.100.7, ::1"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.ArkeselUSSDAllowedIPs {
		got = append(got, p.String())
	}
	if strings.Join(got, ",") != "203.0.113.0/24,198.51.100.7/32,::1/128" {
		t.Errorf("allowlist parsed as %v", got)
	}
}
