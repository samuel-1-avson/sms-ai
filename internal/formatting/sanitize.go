package formatting

import (
	"regexp"
	"strings"
	"unicode"
)

// SanitizeOptions controls how non-GSM characters are treated.
type SanitizeOptions struct {
	// AllowUCS2 keeps letters GSM-7 cannot encode (e.g. Twi ɛ/ɔ) instead of
	// transliterating them. Only enable it when the user wrote in a local
	// language that needs them: a single kept character makes the whole
	// message UCS-2 (70-character segments).
	AllowUCS2 bool
}

// homoglyphs maps typographic characters LLMs emit to GSM-7 equivalents.
// A single curly apostrophe would otherwise turn a 1-segment GSM-7 message
// into a 3-segment UCS-2 one (plan §6.2 "The Homoglyph Trap").
var homoglyphs = map[rune]string{
	// Quotes and apostrophes.
	'‘': "'", '’': "'", '‚': "'", '‛': "'", '′': "'", '´': "'",
	'`': "'", '‹': "'", '›': "'",
	'“': `"`, '”': `"`, '„': `"`, '‟': `"`, '«': `"`, '»': `"`, '″': `"`,
	// Dashes and hyphens.
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-", '−': "-",
	'\u00AD': "",
	// Ellipsis and bullets.
	'…': "...",
	'•': "-", '·': "-", '‣': "-", '⁃': "-", '●': "-", '▪': "-", '◦': "-",
	// Maths.
	'×': "x", '÷': "/", '≤': "<=", '≥': ">=", '≠': "!=", '±': "+/-",
	'≈': "~", '√': "sqrt", '∞': "infinity", '→': "->", '←': "<-", '⇒': "=>",
	'↔': "<->", '°': " deg ", '‰': " per mille",
	'¹': "^1", '²': "^2", '³': "^3", '⁰': "^0", '⁴': "^4", '⁵': "^5",
	'⁶': "^6", '⁷': "^7", '⁸': "^8", '⁹': "^9",
	'₀': "0", '₁': "1", '₂': "2", '₃': "3", '₄': "4",
	'₅': "5", '₆': "6", '₇': "7", '₈': "8", '₉': "9",
	'½': "1/2", '¼': "1/4", '¾': "3/4", '⅓': "1/3", '⅔': "2/3",
	// Greek letters missing from GSM-7 (Δ Φ Γ Λ Ω Π Ψ Σ Θ Ξ are present).
	'α': "alpha", 'β': "beta", 'γ': "gamma", 'δ': "delta", 'ε': "epsilon",
	'θ': "theta", 'λ': "lambda", 'μ': "u", 'µ': "u", 'π': "pi",
	'ρ': "rho", 'σ': "sigma", 'φ': "phi", 'ω': "omega",
	// Currency and marks.
	'₵': "GHS", '¢': "GHS", '₦': "NGN", '™': "TM", '©': "(c)", '®': "(R)",
	// Spaces and invisible characters.
	'\u00A0': " ", '\u2002': " ", '\u2003': " ", '\u2004': " ", '\u2005': " ", '\u2006': " ",
	'\u2007': " ", '\u2008': " ", '\u2009': " ", '\u200A': " ", '\u202F': " ", '\u205F': " ", '\u3000': " ",
	'\t': " ", '\u2028': "\n", '\u2029': "\n",
	'\u200B': "", '\u200C': "", '\u200D': "", '\u2060': "", '\uFEFF': "",
}

// localLetters are Ghanaian-language letters outside GSM-7. They are
// transliterated unless UCS-2 is allowed.
var localLetters = map[rune]string{
	'ɛ': "e", 'Ɛ': "E", // ɛ Ɛ
	'ɔ': "o", 'Ɔ': "O", // ɔ Ɔ
	'ŋ': "ng", 'Ŋ': "Ng", // ŋ Ŋ
}

// accents maps Latin letters missing from GSM-7 to their base letter.
// (è é ù ì ò Ç Ø ø Å å Æ æ ß É Ä Ö Ñ Ü ä ö ñ ü à are GSM-7 already.)
var accents = map[rune]string{
	'á': "a", 'â': "a", 'ã': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'À': "A", 'Á': "A", 'Â': "A", 'Ã': "A", 'Ā': "A",
	'ç': "c", 'ć': "c", 'č': "c", 'Ć': "C", 'Č': "C",
	'ê': "e", 'ë': "e", 'ē': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'È': "E", 'Ê': "E", 'Ë': "E", 'Ē': "E",
	'í': "i", 'î': "i", 'ï': "i", 'ī': "i", 'Ì': "I", 'Í': "I", 'Î': "I", 'Ï': "I",
	'ó': "o", 'ô': "o", 'õ': "o", 'ō': "o", 'ő': "o", 'Ò': "O", 'Ó': "O", 'Ô': "O", 'Õ': "O",
	'ú': "u", 'û': "u", 'ū': "u", 'ű': "u", 'ů': "u", 'Ù': "U", 'Ú': "U", 'Û': "U",
	'ý': "y", 'ÿ': "y", 'Ý': "Y",
	'ś': "s", 'š': "s", 'Š': "S", 'ž': "z", 'Ž': "Z", 'ł': "l", 'Ł': "L", 'ń': "n", 'ř': "r",
	'œ': "oe", 'Œ': "OE",
}

var (
	reCedi        = regexp.MustCompile(`(?i)gh\s?[¢₵]`)
	reMdLink      = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	reMdBold      = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	reMdCode      = regexp.MustCompile("`([^`\n]+)`")
	reMdHeading   = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	reMdQuote     = regexp.MustCompile(`^\s*>\s?`)
	reMdRule      = regexp.MustCompile(`^\s*([-*_]\s*){3,}$`)
	reMdList      = regexp.MustCompile(`^\s*(?:[-*+•▪●‣◦]|\d{1,2}[.)])\s+`)
	reFillerShort = regexp.MustCompile(`(?i)^(?:sure|certainly|of course|absolutely|great question|good question|okay|ok|alright|hello|hi there|hi)\s*[!.,:]+\s*`)
	reFillerHere  = regexp.MustCompile(`(?i)^here(?:'s| is| are)\s+(?:the\s+|an?\s+|your\s+)?(?:short\s+|brief\s+|quick\s+|concise\s+|simple\s+)?(?:answer|explanation|summary|response|reply)s?(?:\s+to\s+your\s+question)?\s*[:.!-]*\s*`)
	reFillerLabel = regexp.MustCompile(`(?i)^(?:answer|response|reply)\s*:\s*`)
	reSpaceBefore = regexp.MustCompile(` +([,.!?;:)])`)
	reSpaces      = regexp.MustCompile(`\s+`)
)

// Sanitize converts raw model output to compact single-line text that stays
// in GSM-7 (unless opts.AllowUCS2 keeps local-language letters).
func Sanitize(raw string, opts SanitizeOptions) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = reCedi.ReplaceAllString(s, "GHS")
	s = reMdCode.ReplaceAllString(s, "$1")
	s = mapRunes(s, opts, true)
	s = stripMarkdown(s)
	stripped := false
	for i := 0; i < 3; i++ {
		next := s
		for _, re := range []*regexp.Regexp{reFillerShort, reFillerHere, reFillerLabel} {
			next = re.ReplaceAllString(next, "")
		}
		if next == s {
			break
		}
		s, stripped = next, true
	}
	s = mapRunes(s, opts, false)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reSpaceBefore.ReplaceAllString(s, "$1")
	s = strings.TrimSpace(s)
	if stripped {
		s = capitalizeFirst(s)
	}
	return s
}

func capitalizeFirst(s string) string {
	for i, r := range s {
		if unicode.IsLower(r) {
			if up := unicode.ToUpper(r); IsGSM7(up) {
				return s[:i] + string(up) + s[i+len(string(r)):]
			}
		}
		return s
	}
	return s
}

// mapRunes applies the homoglyph table. With keepNewlines the line structure
// survives for markdown handling; the final pass also drops anything that is
// still not encodable (emoji, symbols, control characters).
func mapRunes(s string, opts SanitizeOptions, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' {
			if keepNewlines {
				b.WriteRune('\n')
			} else {
				b.WriteRune(' ')
			}
			continue
		}
		if r == '\r' {
			b.WriteRune(' ')
			continue
		}
		if rep, ok := homoglyphs[r]; ok {
			b.WriteString(rep)
			continue
		}
		if IsGSM7(r) {
			b.WriteRune(r)
			continue
		}
		if rep, ok := localLetters[r]; ok {
			if opts.AllowUCS2 {
				b.WriteRune(r)
			} else {
				b.WriteString(rep)
			}
			continue
		}
		if rep, ok := accents[r]; ok {
			b.WriteString(rep)
			continue
		}
		if keepNewlines {
			// First pass: leave unknown characters for the final pass.
			b.WriteRune(r)
			continue
		}
		if opts.AllowUCS2 && (unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsPunct(r)) {
			b.WriteRune(r)
		}
		// Everything else (emoji, pictographs, combining marks, controls) is dropped.
	}
	return b.String()
}

func stripMarkdown(s string) string {
	s = reMdLink.ReplaceAllString(s, "$1 ($2)")
	s = reMdBold.ReplaceAllString(s, "$1$2")
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if reMdRule.MatchString(line) {
			continue
		}
		line = reMdHeading.ReplaceAllString(line, "")
		line = reMdQuote.ReplaceAllString(line, "")
		isItem := reMdList.MatchString(line)
		line = strings.TrimSpace(reMdList.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if out.Len() > 0 {
			prev := out.String()
			last := prev[len(prev)-1]
			switch {
			case strings.ContainsRune(".!?:;,", rune(last)):
				out.WriteString(" ")
			case isItem:
				out.WriteString("; ")
			default:
				out.WriteString(" ")
			}
		}
		out.WriteString(line)
	}
	return out.String()
}

// ContainsLocalLanguageLetters reports whether the user wrote Ghanaian
// letters (ɛ, ɔ, ŋ) that GSM-7 cannot carry.
func ContainsLocalLanguageLetters(s string) bool {
	for _, r := range s {
		if _, ok := localLetters[r]; ok {
			return true
		}
	}
	return false
}
