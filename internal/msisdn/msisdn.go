// Package msisdn normalizes phone numbers to E.164 and infers the Ghanaian
// network from the number prefix (for metrics only: ported numbers make the
// prefix an approximation).
package msisdn

import (
	"errors"
	"strings"
)

var ErrInvalid = errors.New("msisdn: invalid phone number")

const ghanaCC = "233"

// Normalize converts common Ghanaian and international formats to E.164:
// "0244123456", "233244123456", "+233 24 412 3456", "00233244123456".
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	international := strings.HasPrefix(raw, "+")
	var b strings.Builder
	for i, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", ErrInvalid
		}
	}
	d := b.String()
	switch {
	case international:
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	case len(d) == 10 && d[0] == '0':
		d = ghanaCC + d[1:]
	case len(d) == 9:
		d = ghanaCC + d
	}
	if strings.HasPrefix(d, ghanaCC) {
		if len(d) != 12 {
			return "", ErrInvalid
		}
	} else if len(d) < 8 || len(d) > 15 || d[0] == '0' {
		return "", ErrInvalid
	}
	return "+" + d, nil
}

// Carrier returns "mtn", "telecel", "at" or "unknown" for a normalized number.
func Carrier(e164 string) string {
	if !strings.HasPrefix(e164, "+"+ghanaCC) || len(e164) != 13 {
		return "unknown"
	}
	switch e164[4:6] {
	case "24", "25", "53", "54", "55", "59":
		return "mtn"
	case "20", "50":
		return "telecel"
	case "26", "27", "56", "57":
		return "at"
	}
	return "unknown"
}

// Mask hides all but the country code and last two digits, for logs.
func Mask(e164 string) string {
	if len(e164) < 7 {
		return "***"
	}
	return e164[:4] + strings.Repeat("*", len(e164)-6) + e164[len(e164)-2:]
}
