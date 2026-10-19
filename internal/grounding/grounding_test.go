package grounding

import (
	"strings"
	"testing"

	"smsaighana/data"
)

func seedMatcher(t *testing.T) *Matcher {
	t.Helper()
	snips, err := ParseSeed(data.GroundingSeed)
	if err != nil {
		t.Fatalf("embedded seed invalid: %v", err)
	}
	return NewMatcher(snips)
}

func TestEmbeddedSeedIsValid(t *testing.T) {
	if m := seedMatcher(t); m.Len() < 10 {
		t.Fatalf("expected at least 10 snippets, got %d", m.Len())
	}
}

func TestDirectHits(t *testing.T) {
	m := seedMatcher(t)
	cases := map[string]string{
		"How do I check my NHIS renewal?":      "nhis-check-renew",
		"How to link Ghana Card to MTN?":       "ghana-card-sim-link",
		"Explain Ohm's law":                    "ohms-law",
		"explain newtons first law":            "newtons-first-law",
		"what are the WASSCE grades":           "wassce-grading",
		"How do I calculate my BECE aggregate": "bece-aggregate",
		"Should I share my MoMo PIN?":          "momo-pin-safety",
	}
	for q, slug := range cases {
		r := m.Match(q)
		if r.Direct == nil || r.Direct.Slug != slug {
			t.Errorf("Match(%q) direct=%v context=%d; want direct %s", q, slugOf(r.Direct), len(r.Context), slug)
		}
	}
}

func TestMissesAndContext(t *testing.T) {
	m := seedMatcher(t)
	for _, q := range []string{"What is photosynthesis?", "Solve 2x + 5 = 15", "Who won the 2010 world cup", "newton second law"} {
		if r := m.Match(q); r.Direct != nil || len(r.Context) != 0 {
			t.Errorf("Match(%q) should miss, got direct=%v context=%d", q, slugOf(r.Direct), len(r.Context))
		}
	}
	// Two different full matches are ambiguous: inject both as context.
	r := m.Match("What is the difference between Ohm's law and Newton's first law?")
	if r.Direct != nil || len(r.Context) != 2 {
		t.Fatalf("ambiguous question: direct=%v context=%d", slugOf(r.Direct), len(r.Context))
	}
	// A long question that merely mentions a topic gets context, not a canned reply.
	r = m.Match("My NHIS status shows expired but I paid last month at the office in Kumasi, what went wrong")
	if r.Direct != nil || len(r.Context) == 0 || r.Context[0].Slug != "nhis-check-renew" {
		t.Fatalf("long question: direct=%v context=%d", slugOf(r.Direct), len(r.Context))
	}
}

func TestParseSeedRejectsBadAnswers(t *testing.T) {
	bad := []byte(`version: 1
snippets:
  - slug: curly
    category: x
    triggers: ["a b"]
    answer: "It’s curly"
  - slug: long
    category: x
    triggers: ["a b"]
    answer: "` + strings.Repeat("a", 161) + `"
  - slug: curly
    category: x
    triggers: []
    answer: "ok"
`)
	_, err := ParseSeed(bad)
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"curly: answer must be", "long: answer must be", "duplicate slug", "at least one trigger"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := strings.Join(Tokenize("How do I check my NHIS renewal?"), " ")
	if got != "check nhis renew" {
		t.Fatalf("Tokenize = %q", got)
	}
}

func slugOf(s *Snippet) string {
	if s == nil {
		return "<nil>"
	}
	return s.Slug
}
