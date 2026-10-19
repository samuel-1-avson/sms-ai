package formatting

import (
	"strings"
	"testing"
)

func TestSeptetCounting(t *testing.T) {
	cases := []struct {
		in    string
		enc   Encoding
		units int
	}{
		{"Hello", GSM7, 5},
		{"[a]", GSM7, 5},  // [ and ] are extension characters: 2 septets each
		{"€5", GSM7, 3},   // € is an extension character
		{"@£$¥", GSM7, 4}, // basic-table symbols cost one septet
		{"It’s", UCS2, 4}, // curly apostrophe forces UCS-2
		{"😀", UCS2, 2},    // surrogate pair counts as two UTF-16 units
	}
	for _, c := range cases {
		enc, n := Measure(c.in)
		if enc != c.enc || n != c.units {
			t.Errorf("Measure(%q) = %s/%d; want %s/%d", c.in, enc, n, c.enc, c.units)
		}
	}
}

func TestSegmentCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{strings.Repeat("a", 160), 1},
		{strings.Repeat("a", 161), 2},
		{strings.Repeat("a", 306), 2},
		{strings.Repeat("a", 307), 3},
		{strings.Repeat("ɛ", 70), 1},
		{strings.Repeat("ɛ", 71), 2},
		{strings.Repeat("ɛ", 134), 2},
		{strings.Repeat("ɛ", 135), 3},
		// 152 septets + € (2 septets) cannot share part one: the escape pair moves to part two.
		{strings.Repeat("a", 152) + "€" + strings.Repeat("a", 10), 2},
		{strings.Repeat("a", 159) + "€", 2},
	}
	for _, c := range cases {
		if got := SegmentCount(c.in); got != c.want {
			t.Errorf("SegmentCount(len=%d) = %d; want %d", len([]rune(c.in)), got, c.want)
		}
	}
}

func TestHomoglyphTrap(t *testing.T) {
	// A ~145-character answer: one GSM-7 segment, but three UCS-2 segments
	// if a single curly apostrophe slips through (plan §6.2).
	raw := "Photosynthesis is how plants make food from sunlight, water and carbon dioxide. It’s the reason we have oxygen in the air that we all breathe."
	if n := len([]rune(raw)); n < 135 || n > 160 {
		t.Fatalf("fixture length %d", n)
	}
	if SegmentCount(raw) != 3 {
		t.Fatalf("precondition: raw text with ’ should bill as 3 UCS-2 segments, got %d", SegmentCount(raw))
	}
	clean := Sanitize(raw, SanitizeOptions{})
	st := Analyze(clean)
	if st.Encoding != GSM7 || st.Segments != 1 {
		t.Fatalf("sanitized text should be 1 GSM-7 segment, got %+v: %q", st, clean)
	}
}

func TestSanitizeMappings(t *testing.T) {
	cases := map[string]string{
		"It’s “great” — really…":                    `It's "great" - really...`,
		"2 × 3 ÷ 6 ≤ 1 ≥ 0":                         "2 x 3 / 6 <= 1 >= 0",
		"H₂O and x² at 25°C":                        "H2O and x^2 at 25 deg C",
		"Earth's tilt (23.5°) causes seasons":       "Earth's tilt (23.5 deg) causes seasons",
		"• one\n• two":                              "one; two",
		"It costs GH¢5 or ₵5":                       "It costs GHS5 or GHS5",
		"Great job 👍!":                              "Great job!",
		"Café naïve façade":                         "Café naive facade", // é is GSM-7; ï and ç are not
		"a\u00A0b\u200Bc":                           "a bc",
		"Use `*929#` now":                           "Use *929# now",
		"π r²":                                      "pi r^2",
		"Visit [NHIA](https://www.nhis.gov.gh) now": "Visit NHIA (https://www.nhis.gov.gh) now",
	}
	for in, want := range cases {
		if got := Sanitize(in, SanitizeOptions{}); got != want {
			t.Errorf("Sanitize(%q)\n got %q\nwant %q", in, got, want)
		}
		if !IsGSM7Text(Sanitize(in, SanitizeOptions{})) {
			t.Errorf("Sanitize(%q) is not GSM-7", in)
		}
	}
}

func TestSanitizeStripsFillerAndMarkdown(t *testing.T) {
	cases := map[string]string{
		"Sure! Here is the answer: photosynthesis makes food.":         "Photosynthesis makes food.",
		"Certainly. Here’s a short explanation. Ohm’s law is V=IR.":    "Ohm's law is V=IR.",
		"## Ohm's law\n**V = I x R** (voltage = current x resistance)": "Ohm's law V = I x R (voltage = current x resistance)",
		"Answer: 42":                       "42",
		"Steps:\n1. Mix flour\n2. Bake it": "Steps: Mix flour; Bake it",
		"Reply STOP to opt out":            "Reply STOP to opt out",
		"OK is short for okay.":            "OK is short for okay.",
		"2*3*4 = 24":                       "2*3*4 = 24",
		"Line one\n\n---\n\nLine two":      "Line one Line two",
	}
	for in, want := range cases {
		if got := Sanitize(in, SanitizeOptions{}); got != want {
			t.Errorf("Sanitize(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSanitizeLocalLanguage(t *testing.T) {
	in := "Ɛte sɛn? Me ho yɛ."
	if got := Sanitize(in, SanitizeOptions{}); got != "Ete sen? Me ho ye." {
		t.Fatalf("transliteration: %q", got)
	}
	kept := Sanitize(in, SanitizeOptions{AllowUCS2: true})
	if kept != in {
		t.Fatalf("AllowUCS2 should keep Twi letters: %q", kept)
	}
	if enc, _ := Measure(kept); enc != UCS2 {
		t.Fatal("Twi letters require UCS-2")
	}
	if !ContainsLocalLanguageLetters(in) || ContainsLocalLanguageLetters("hello") {
		t.Fatal("ContainsLocalLanguageLetters")
	}
	// Curly quotes are still mapped even when UCS-2 is allowed.
	if got := Sanitize("ɔ’s", SanitizeOptions{AllowUCS2: true}); got != "ɔ's" {
		t.Fatalf("homoglyphs must be mapped in UCS-2 mode: %q", got)
	}
}

const answer240 = "Photosynthesis is the process plants use to turn sunlight, water and carbon dioxide into glucose and oxygen. " +
	"It happens in the chloroplasts of leaf cells. Chlorophyll absorbs the light energy that powers the reaction for growth."

func TestSplitFirstMoreBuffer(t *testing.T) {
	if n := len(answer240); n < 220 || n > 260 {
		t.Fatalf("fixture length %d", n)
	}
	first, rest := SplitFirst(answer240, SMSLimits, SMSMoreSuffix)
	if !strings.HasSuffix(first, "..Txt MORE") {
		t.Fatalf("part 1 must end with ..Txt MORE: %q", first)
	}
	if _, n := Measure(first); n > 160 {
		t.Fatalf("part 1 is %d septets", n)
	}
	if SegmentCount(first) != 1 {
		t.Fatal("part 1 must be one segment")
	}
	if !strings.HasSuffix(strings.TrimSuffix(first, SMSMoreSuffix), ".") {
		t.Fatalf("part 1 should end at a sentence boundary: %q", first)
	}
	if rest == "" || !Fits(rest, SMSLimits) {
		t.Fatalf("part 2 should be a single page: %q", rest)
	}
	if strings.TrimSuffix(first, SMSMoreSuffix)+" "+rest != answer240 {
		t.Fatal("no text may be lost between part 1 and part 2")
	}
}

func TestSplitFirstShortText(t *testing.T) {
	first, rest := SplitFirst("Ohm's law: V = I x R.", SMSLimits, SMSMoreSuffix)
	if first != "Ohm's law: V = I x R." || rest != "" {
		t.Fatalf("short text must not be split: %q / %q", first, rest)
	}
}

func TestSplitFirstDropsDanglingPunctuation(t *testing.T) {
	text := strings.Repeat("plants, ", 30) // no sentence end to cut at
	first, rest := SplitFirst(text, SMSLimits, SMSMoreSuffix)
	head := strings.TrimSuffix(first, SMSMoreSuffix)
	if strings.HasSuffix(head, ",") || rest == "" {
		t.Fatalf("word-boundary page should not end with a comma: %q", first)
	}
}

func TestSplitFirstNoSpaces(t *testing.T) {
	long := strings.Repeat("x", 400)
	first, rest := SplitFirst(long, SMSLimits, SMSMoreSuffix)
	if _, n := Measure(first); n > 160 {
		t.Fatalf("hard cut overflowed: %d", n)
	}
	if len(rest) != 400-(len(first)-len(SMSMoreSuffix)) {
		t.Fatalf("hard cut lost characters")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := Truncate(long, SMSLimits)
	if _, n := Measure(got); n > 160 || !strings.HasSuffix(got, "...") {
		t.Fatalf("Truncate: %d %q", n, got)
	}
	sentences := "First sentence is here and it is long enough to matter for the cut. Second sentence keeps going. " +
		"Third sentence is far too long to fit on this page at all, so it has to go."
	if got := Truncate(sentences, SMSLimits); got != "First sentence is here and it is long enough to matter for the cut. Second sentence keeps going." {
		t.Fatalf("Truncate should end on a sentence: %q", got)
	}
}

func TestPaginate(t *testing.T) {
	text := strings.Repeat("Plants need light to grow well. ", 20)
	pages := Paginate(text, SMSLimits, SMSMoreSuffix, 3)
	if len(pages) != 3 {
		t.Fatalf("want 3 pages, got %d", len(pages))
	}
	for i, p := range pages {
		if SegmentCount(p) != 1 {
			t.Errorf("page %d is %d segments", i, SegmentCount(p))
		}
		hasMore := strings.HasSuffix(p, SMSMoreSuffix)
		if i < 2 && !hasMore || i == 2 && hasMore {
			t.Errorf("page %d suffix wrong: %q", i, p)
		}
	}
}

func TestUCS2Pagination(t *testing.T) {
	text := strings.Repeat("Ɛyɛ adeɛ pa. ", 12)
	first, rest := SplitFirst(text, SMSLimits, SMSMoreSuffix)
	if enc, n := Measure(first); enc != UCS2 || n > 70 {
		t.Fatalf("UCS-2 page: %s %d", enc, n)
	}
	if rest == "" {
		t.Fatal("expected remainder")
	}
}
