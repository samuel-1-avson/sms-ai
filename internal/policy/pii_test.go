package policy

import "testing"

func TestScrub(t *testing.T) {
	cases := map[string]string{
		"My Ghana Card GHA-123456789-0 will not link":    "My Ghana Card [ghana card removed] will not link",
		"card 4111 1111 1111 1111 was charged twice":     "card [card number removed] was charged twice",
		"My MoMo PIN is 1234, what do I do?":             "My MoMo PIN is [removed], what do I do?",
		"the otp: 583920 came in":                        "the otp: [removed] came in",
		"my password=abc123x":                            "my password=[removed]",
		"Solve 2x + 5 = 15":                              "Solve 2x + 5 = 15",
		"What is a PIN code?":                            "What is a PIN code?",
		"Call 0244123456 tomorrow":                       "Call 0244123456 tomorrow",
		"The year 2026 had 1234567890123 grains of rice": "The year 2026 had 1234567890123 grains of rice", // fails Luhn
	}
	for in, want := range cases {
		got, changed := Scrub(in)
		if got != want {
			t.Errorf("Scrub(%q)\n got %q\nwant %q", in, got, want)
		}
		if changed != (in != want) {
			t.Errorf("Scrub(%q) changed=%v", in, changed)
		}
	}
}

func TestTruncateQuestion(t *testing.T) {
	if got := TruncateQuestion("  ɛɛɛɛ  ", 2); got != "ɛɛ" {
		t.Fatalf("got %q", got)
	}
}
