package hubtel_test

import (
	"strings"
	"testing"

	"smsaighana/internal/config"
)

// TestConfigValidation pins the configuration guards that keep the Hubtel
// adapters from running unauthenticated, silently billing, or with values
// Hubtel would reject.
func TestConfigValidation(t *testing.T) {
	token := strings.Repeat("t", 24)
	base := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=hubtel", "PRIMARY_USSD_PROVIDER=hubtel", "SMS_ENVIRONMENT=live",
		"HUBTEL_CLIENT_ID=id", "HUBTEL_CLIENT_SECRET=secret", "HUBTEL_USSD_TOKEN=" + token,
	}
	cases := []struct {
		name  string
		extra []string
		want  string // substring of the error; empty means valid
	}{
		{"minimal valid", nil, ""},
		{"with a delivery-report token", []string{"HUBTEL_DLR_TOKEN=" + strings.Repeat("d", 24)}, ""},
		{"two USSD tokens while rotating", []string{"HUBTEL_USSD_TOKEN=" + strings.Repeat("a", 24) + ", " + strings.Repeat("b", 24)}, ""},
		{"no USSD token", []string{"HUBTEL_USSD_TOKEN="}, "HUBTEL_USSD_TOKEN is required"},
		{"placeholder USSD token", []string{"HUBTEL_USSD_TOKEN=REPLACE_VIA_SECRET_MANAGER"}, "HUBTEL_USSD_TOKEN is required"},
		{"short USSD token", []string{"HUBTEL_USSD_TOKEN=short"}, "at least 24 characters"},
		{"short delivery-report token", []string{"HUBTEL_DLR_TOKEN=short"}, "at least 24 characters"},
		{"no client id", []string{"HUBTEL_CLIENT_ID=REPLACE_VIA_SECRET_MANAGER"}, "HUBTEL_CLIENT_ID and HUBTEL_CLIENT_SECRET are required"},
		{"no client secret", []string{"HUBTEL_CLIENT_SECRET="}, "HUBTEL_CLIENT_ID and HUBTEL_CLIENT_SECRET are required"},
		{"sender ID too long", []string{"SMS_OUTBOUND_SENDER_ID=SMS-AI-GHANA"}, "1-11 characters"},
		{"sandbox is not available", []string{"SMS_ENVIRONMENT=sandbox"}, "Hubtel has no sandbox mode"},
		{"sandbox is the default", []string{"SMS_ENVIRONMENT="}, "Hubtel has no sandbox mode"},
		{"relative base URL", []string{"HUBTEL_SMS_BASE_URL=/v1"}, "not an absolute"},
		{"plain HTTP base URL in staging", []string{"APP_ENV=staging", "ADMIN_API_TOKEN=" + strings.Repeat("x", 24),
			"METRICS_TOKEN=" + strings.Repeat("y", 24), "HUBTEL_SMS_BASE_URL=http://sms.hubtel.com"}, "must use https"},
		{"USSD only needs no SMS credentials or live mode", []string{"PRIMARY_SMS_PROVIDER=mock", "HUBTEL_CLIENT_ID=", "HUBTEL_CLIENT_SECRET=", "SMS_ENVIRONMENT=sandbox"}, ""},
		{"SMS only needs no USSD token", []string{"PRIMARY_USSD_PROVIDER=mock", "HUBTEL_USSD_TOKEN="}, ""},
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

	// Hubtel settings are not validated when Hubtel is not selected.
	idle := []string{
		"APP_ENV=test", "LLM_PROVIDER=mock",
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
		"PRIMARY_SMS_PROVIDER=mock", "PRIMARY_USSD_PROVIDER=mock", "HUBTEL_USSD_TOKEN=short",
	}
	if cfg, err := config.LoadFrom(idle); err != nil || cfg.Validate() != nil {
		t.Errorf("unselected Hubtel must not be validated: %v", err)
	}

	if _, err := config.LoadFrom(append(append([]string{}, base...), "HUBTEL_DLR_ALLOWED_IPS=203.0.113.0/24, nope")); err == nil ||
		!strings.Contains(err.Error(), `"nope" is not an IP address`) {
		t.Errorf("bad allowlist entry: %v", err)
	}
	cfg, err := config.LoadFrom(append(append([]string{}, base...),
		"HUBTEL_USSD_ALLOWED_IPS=203.0.113.9/24, 198.51.100.7", "HUBTEL_DLR_ALLOWED_IPS=::1", "HUBTEL_CLIENT_IP_HEADER=X-Real-IP"))
	if err != nil {
		t.Fatal(err)
	}
	var ussd, dlr []string
	for _, p := range cfg.HubtelUSSDAllowedIPs {
		ussd = append(ussd, p.String())
	}
	for _, p := range cfg.HubtelDLRAllowedIPs {
		dlr = append(dlr, p.String())
	}
	if strings.Join(ussd, ",") != "203.0.113.0/24,198.51.100.7/32" || strings.Join(dlr, ",") != "::1/128" || cfg.HubtelClientIPHeader != "X-Real-IP" {
		t.Errorf("allowlists parsed as %v / %v, header %q", ussd, dlr, cfg.HubtelClientIPHeader)
	}
	if cfg.HubtelSMSBaseURL != "https://sms.hubtel.com" {
		t.Errorf("default base URL %q", cfg.HubtelSMSBaseURL)
	}
}
