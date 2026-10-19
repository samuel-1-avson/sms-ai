package config

import (
	"strings"
	"testing"
)

func TestNoSimulatorOrGatewayByDefault(t *testing.T) {
	cfg, err := LoadFrom([]string{"APP_ENV=development"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMProvider != "deepinfra" || cfg.PrimarySMSProvider != "" || cfg.PrimaryUSSDProvider != "" {
		t.Fatalf("want deepinfra and no default gateway, got llm=%s sms=%q ussd=%q",
			cfg.LLMProvider, cfg.PrimarySMSProvider, cfg.PrimaryUSSDProvider)
	}
	// An unconfigured system fails validation instead of silently falling
	// back to a simulator or to a provider nobody chose.
	err = cfg.Validate()
	if err == nil {
		t.Fatal("a configuration with no provider settings must not validate")
	}
	for _, want := range []string{"DEEPINFRA_API_KEY", "PRIMARY_SMS_PROVIDER is required", "PRIMARY_USSD_PROVIDER is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error should mention %s: %v", want, err)
		}
	}
}

func TestMockOnlyInDevelopmentAndTest(t *testing.T) {
	mock := []string{"LLM_PROVIDER=mock", "PRIMARY_SMS_PROVIDER=mock", "PRIMARY_USSD_PROVIDER=mock"}
	keys := []string{
		"ENCRYPTION_KEYS_V1=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		"HMAC_KEYS_V1=ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8=",
	}
	for _, env := range []string{"development", "test"} {
		cfg, err := LoadFrom(append(append([]string{"APP_ENV=" + env}, keys...), mock...))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.AllowsMock() {
			t.Errorf("%s should allow mock", env)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s with mock providers should validate: %v", env, err)
		}
	}
	for _, env := range []string{"staging", "production"} {
		cfg, err := LoadFrom(append(append([]string{
			"APP_ENV=" + env,
			"ADMIN_API_TOKEN=0123456789abcdef01234567", "METRICS_TOKEN=0123456789abcdef01234567",
		}, keys...), mock...))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AllowsMock() {
			t.Errorf("%s must not allow mock", env)
		}
		err = cfg.Validate()
		if err == nil || strings.Count(err.Error(), "only allowed when APP_ENV is development or test") != 2 {
			t.Errorf("%s: want both mock-provider errors, got %v", env, err)
		}
	}
}
