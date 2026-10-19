// Package policy holds pure policy rules: PII scrubbing and user-facing
// quota/limit texts. Stateful quota enforcement lives in the repository.
package policy

import (
	"regexp"
	"strings"
)

var (
	// Ghana Card PIN, e.g. GHA-123456789-0.
	reGhanaCard = regexp.MustCompile(`(?i)\bGHA[-\s]?\d{9}[-\s]?\d\b`)
	// 13–19 digit runs (optionally grouped by spaces/dashes): candidate card numbers.
	reCardCandidate = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	// A secret keyword followed by a value containing at least one digit.
	reSecret = regexp.MustCompile(`(?i)\b(pin|otp|password|passcode|passwd|cvv|cvc|verification code|security code)\b(\s*(?:is|was|=|:|-|no\.?|number)?\s*)([A-Za-z0-9]*\d[A-Za-z0-9]*)`)
)

const (
	redactedGhanaCard = "[ghana card removed]"
	redactedCard      = "[card number removed]"
	redactedSecret    = "[removed]"
)

// Scrub removes Ghana Card numbers, payment card numbers and PIN/OTP/password
// values before text is persisted or sent to the model (plan §10.2).
// Ordinary numbers (maths questions, phone numbers, years) are left alone.
func Scrub(s string) (string, bool) {
	out := reGhanaCard.ReplaceAllString(s, redactedGhanaCard)
	out = reCardCandidate.ReplaceAllStringFunc(out, func(m string) string {
		if luhnValid(m) {
			return redactedCard
		}
		return m
	})
	out = reSecret.ReplaceAllString(out, "${1}${2}"+redactedSecret)
	return out, out != s
}

func luhnValid(s string) bool {
	digits := make([]int, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if (len(digits)-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// TruncateQuestion bounds the user prompt length sent to the model.
func TruncateQuestion(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return string(r[:maxRunes])
}
