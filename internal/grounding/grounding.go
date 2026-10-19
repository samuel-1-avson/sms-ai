// Package grounding is the Local Ghana Grounding Store (plan §7.5): curated,
// verified snippets matched in memory before any model call. A direct hit
// answers in microseconds at zero token cost; a partial hit is injected into
// the prompt as a verified fact.
package grounding

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"smsaighana/internal/formatting"
)

type Snippet struct {
	Slug        string   `yaml:"slug"`
	Category    string   `yaml:"category"`
	Triggers    []string `yaml:"triggers"`
	Answer      string   `yaml:"answer"`
	Context     string   `yaml:"context"`
	DirectReply bool     `yaml:"direct_reply"`
	Reviewed    bool     `yaml:"reviewed"`
	Source      string   `yaml:"source"`
	Enabled     *bool    `yaml:"enabled"`
}

// IsEnabled defaults to true when the field is omitted.
func (s Snippet) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Fact is the text injected into the prompt for partial matches.
func (s Snippet) Fact() string {
	if s.Context != "" {
		return s.Context
	}
	return s.Answer
}

type seedFile struct {
	Version  int       `yaml:"version"`
	Snippets []Snippet `yaml:"snippets"`
}

// ParseSeed parses and validates a YAML seed document.
func ParseSeed(b []byte) ([]Snippet, error) {
	var f seedFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("grounding: parse seed: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("grounding: unsupported seed version %d", f.Version)
	}
	var errs []error
	seen := map[string]bool{}
	for i := range f.Snippets {
		s := &f.Snippets[i]
		s.Answer = strings.TrimSpace(s.Answer)
		if s.Slug == "" {
			errs = append(errs, fmt.Errorf("snippet #%d: slug is required", i+1))
			continue
		}
		if seen[s.Slug] {
			errs = append(errs, fmt.Errorf("%s: duplicate slug", s.Slug))
		}
		seen[s.Slug] = true
		if len(s.Triggers) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one trigger is required", s.Slug))
		}
		if st := formatting.Analyze(s.Answer); st.Encoding != formatting.GSM7 || st.Units > formatting.GSM7Single || st.Units == 0 {
			errs = append(errs, fmt.Errorf("%s: answer must be 1-160 GSM-7 septets (got %s/%d)", s.Slug, st.Encoding, st.Units))
		}
		if s.Category == "" {
			errs = append(errs, fmt.Errorf("%s: category is required", s.Slug))
		}
	}
	return f.Snippets, errors.Join(errs...)
}

// LoadSeedFile reads a seed from disk.
func LoadSeedFile(path string) ([]Snippet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSeed(b)
}

// Result of matching a question against the store.
type Result struct {
	// Direct is set when the snippet answer should be sent as-is.
	Direct *Snippet
	// Context holds verified facts to inject into the model prompt.
	Context []*Snippet
}

type entry struct {
	snippet  *Snippet
	triggers [][]string
}

// Matcher is a concurrency-safe in-memory index over snippets.
type Matcher struct {
	mu      sync.RWMutex
	entries []entry
}

func NewMatcher(snippets []Snippet) *Matcher {
	m := &Matcher{}
	m.Replace(snippets)
	return m
}

// Replace swaps the indexed snippets (used on reload).
func (m *Matcher) Replace(snippets []Snippet) {
	entries := make([]entry, 0, len(snippets))
	for i := range snippets {
		s := snippets[i]
		if !s.IsEnabled() {
			continue
		}
		e := entry{snippet: &s}
		for _, t := range s.Triggers {
			if toks := Tokenize(t); len(toks) > 0 {
				e.triggers = append(e.triggers, toks)
			}
		}
		if len(e.triggers) > 0 {
			entries = append(entries, e)
		}
	}
	m.mu.Lock()
	m.entries = entries
	m.mu.Unlock()
}

func (m *Matcher) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

const (
	// A trigger counts as matched when at least this share of its words
	// appear in the question (all words for 2–3 word triggers).
	matchThreshold = 0.75
	// A direct reply is skipped when the question carries more than this many
	// content words beyond the trigger: it is probably asking something else.
	maxExtraWordsForDirect = 4
)

type hit struct {
	snippet     *Snippet
	score       float64
	specificity int
	extra       int
}

// Match classifies a question as a direct hit, context-only, or a miss.
func (m *Matcher) Match(question string) Result {
	q := Tokenize(question)
	if len(q) == 0 {
		return Result{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var hits []hit
	for _, e := range m.entries {
		best := hit{snippet: e.snippet}
		for _, trig := range e.triggers {
			matched, usedQ := 0, map[int]bool{}
			for _, t := range trig {
				for qi, qt := range q {
					if !usedQ[qi] && tokenMatch(t, qt) {
						matched++
						usedQ[qi] = true
						break
					}
				}
			}
			score := float64(matched) / float64(len(trig))
			if score > best.score || score == best.score && len(trig) > best.specificity {
				best.score, best.specificity, best.extra = score, len(trig), len(q)-len(usedQ)
			}
		}
		if best.score >= matchThreshold {
			hits = append(hits, best)
		}
	}
	if len(hits) == 0 {
		return Result{}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].specificity > hits[j].specificity
	})

	res := Result{}
	full := 0
	for _, h := range hits {
		if h.score == 1 {
			full++
		}
	}
	top := hits[0]
	if top.score == 1 && full == 1 && top.snippet.DirectReply && top.extra <= maxExtraWordsForDirect {
		res.Direct = top.snippet
		return res
	}
	for i, h := range hits {
		if i == 2 {
			break
		}
		res.Context = append(res.Context, h.snippet)
	}
	return res
}

func tokenMatch(trigger, query string) bool {
	if trigger == query {
		return true
	}
	if len(trigger) >= 4 && len(query) >= 4 {
		return strings.HasPrefix(query, trigger) || strings.HasPrefix(trigger, query)
	}
	return false
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the i me my mine we our you your do does did how what whats when where
		which who why is are was were be been am can could should would will shall may might must to of for on in
		at by with from about into and or but if then so it its this that these those there please pls plz tell
		explain know want need help give show get let us any some way ways much many im ive dont cant`) {
		stopwords[w] = true
	}
}

// Tokenize lowercases, strips punctuation and possessives, removes stopwords
// and applies a light stemmer. Triggers and questions share it.
func Tokenize(s string) []string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("'s", "", "’s", "", "'", "", "’", "").Replace(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if stopwords[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

func stem(t string) string {
	switch {
	case len(t) > 5 && strings.HasSuffix(t, "ing"):
		return t[:len(t)-3]
	case len(t) > 5 && strings.HasSuffix(t, "al"):
		return t[:len(t)-2]
	case len(t) > 4 && strings.HasSuffix(t, "ed"):
		return t[:len(t)-2]
	case len(t) > 3 && strings.HasSuffix(t, "s") &&
		!strings.HasSuffix(t, "ss") && !strings.HasSuffix(t, "us") && !strings.HasSuffix(t, "is"):
		return t[:len(t)-1]
	}
	return t
}
