package msisdn

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"0244123456":       "+233244123456",
		"233244123456":     "+233244123456",
		"+233244123456":    "+233244123456",
		"+233 24 412 3456": "+233244123456",
		"00233244123456":   "+233244123456",
		"(024) 412-3456":   "+233244123456",
		"244123456":        "+233244123456",
		"+447700900123":    "+447700900123",
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "+23324412345", "02441234567", "12", "+0123456789", "024-41x-3456"} {
		if got, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) = %q; want error", bad, got)
		}
	}
}

func TestCarrier(t *testing.T) {
	cases := map[string]string{
		"+233244123456": "mtn",
		"+233554123456": "mtn",
		"+233204123456": "telecel",
		"+233504123456": "telecel",
		"+233264123456": "at",
		"+233574123456": "at",
		"+233304123456": "unknown",
		"+447700900123": "unknown",
	}
	for in, want := range cases {
		if got := Carrier(in); got != want {
			t.Errorf("Carrier(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestMask(t *testing.T) {
	if got := Mask("+233244123456"); got != "+233*******56" {
		t.Fatalf("Mask = %q", got)
	}
}
