package formatting

import (
	"strings"
	"unicode/utf8"
)

// Limits are per-page capacities in encoding units.
type Limits struct {
	GSM7 int
	UCS2 int
}

// SMSLimits keep every page to exactly one billable SMS segment.
var SMSLimits = Limits{GSM7: GSM7Single, UCS2: UCS2Single}

func (l Limits) For(enc Encoding) int {
	if enc == UCS2 {
		return l.UCS2
	}
	return l.GSM7
}

// SMSMoreSuffix is appended to a page that has a continuation (plan §6.2).
const SMSMoreSuffix = " ..Txt MORE"

type cutKind int

const (
	cutAll cutKind = iota
	cutSentence
	cutWord
	cutHard
)

// cutIndex finds the byte index at which to cut text so the head fits in
// budget units. It prefers the end of a sentence, then a word boundary,
// and only then a hard cut.
func cutIndex(text string, enc Encoding, budget int) (int, cutKind) {
	if budget <= 0 {
		return 0, cutHard
	}
	used, lastFit := 0, 0
	lastSpace, spaceUnits := -1, 0
	lastSentence, sentenceUnits := -1, 0
	prevTerminal := false
	for i, r := range text {
		c := unitCost(enc, r)
		if used+c > budget {
			break
		}
		if r == ' ' {
			lastSpace, spaceUnits = i, used
			if prevTerminal {
				lastSentence, sentenceUnits = i, used
			}
		}
		used += c
		lastFit = i + utf8.RuneLen(r)
		prevTerminal = r == '.' || r == '!' || r == '?'
	}
	if lastFit >= len(text) {
		return len(text), cutAll
	}
	if text[lastFit] == ' ' {
		lastSpace, spaceUnits = lastFit, used
		if prevTerminal {
			lastSentence, sentenceUnits = lastFit, used
		}
	}
	if lastSentence > 0 && sentenceUnits*2 >= budget {
		return lastSentence, cutSentence
	}
	if lastSpace > 0 && spaceUnits*10 >= budget*3 {
		return lastSpace, cutWord
	}
	return lastFit, cutHard
}

func unitsOf(enc Encoding, s string) int {
	n := 0
	for _, r := range s {
		n += unitCost(enc, r)
	}
	return n
}

// Fits reports whether text fits on one page.
func Fits(text string, lim Limits) bool {
	enc, n := Measure(text)
	return n <= lim.For(enc)
}

// SplitFirst returns the first page (with suffix appended when there is a
// continuation) and the remaining text. The first page always fits lim.
func SplitFirst(text string, lim Limits, suffix string) (first, rest string) {
	text = strings.TrimSpace(text)
	enc, n := Measure(text)
	limit := lim.For(enc)
	if n <= limit {
		return text, ""
	}
	budget := limit - unitsOf(enc, suffix)
	if budget < limit/3 {
		return Truncate(text, lim), ""
	}
	idx, kind := cutIndex(text, enc, budget)
	head := strings.TrimSpace(text[:idx])
	if kind == cutWord {
		// Mid-sentence: drop dangling punctuation so the page reads cleanly.
		head = strings.TrimRight(head, " ,;:-")
	}
	if head == "" {
		return Truncate(text, lim), ""
	}
	return head + suffix, strings.TrimSpace(text[idx:])
}

// Truncate shortens text to one page, ending on a full sentence when one
// is long enough, otherwise on a word boundary followed by "...".
func Truncate(text string, lim Limits) string {
	text = strings.TrimSpace(text)
	enc, n := Measure(text)
	limit := lim.For(enc)
	if n <= limit {
		return text
	}
	if idx, kind := cutIndex(text, enc, limit); kind == cutSentence {
		return strings.TrimSpace(text[:idx])
	}
	idx, _ := cutIndex(text, enc, limit-3)
	return strings.TrimRight(text[:idx], " ,;:-") + "..."
}

// Paginate splits text into at most maxPages pages. Every page but the
// last ends with suffix; the last page is truncated if text is too long.
func Paginate(text string, lim Limits, suffix string, maxPages int) []string {
	if maxPages < 1 {
		maxPages = 1
	}
	var pages []string
	rest := strings.TrimSpace(text)
	for len(pages) < maxPages-1 {
		first, r := SplitFirst(rest, lim, suffix)
		pages = append(pages, first)
		if r == "" {
			return pages
		}
		rest = r
	}
	return append(pages, Truncate(rest, lim))
}
