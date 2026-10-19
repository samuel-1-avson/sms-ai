// Package formatting turns raw model output into SMS/USSD-safe text: a
// deterministic GSM-7 homoglyph sanitizer, an exact septet/segment counter
// and a MORE pager (plan §6.2, ADR-005). Nothing here calls the LLM.
package formatting

import (
	"unicode/utf16"
)

type Encoding string

const (
	GSM7 Encoding = "gsm7"
	UCS2 Encoding = "ucs2"
)

// GSM 03.38 default alphabet (the ESC code point is deliberately omitted).
const gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

// GSM 03.38 extension table: each costs two septets (ESC + char).
const gsmExtension = "^{}\\[~]|€"

var (
	basicSet = map[rune]bool{}
	extSet   = map[rune]bool{}
)

func init() {
	for _, r := range gsmBasic {
		basicSet[r] = true
	}
	for _, r := range gsmExtension {
		extSet[r] = true
	}
}

// SeptetCost is 1 for basic GSM-7 characters, 2 for extension characters
// and 0 for characters GSM-7 cannot represent.
func SeptetCost(r rune) int {
	switch {
	case basicSet[r]:
		return 1
	case extSet[r]:
		return 2
	}
	return 0
}

func IsGSM7(r rune) bool { return SeptetCost(r) > 0 }

// IsGSM7Text reports whether every character of s is GSM-7 encodable.
func IsGSM7Text(s string) bool {
	for _, r := range s {
		if !IsGSM7(r) {
			return false
		}
	}
	return true
}

// Measure returns the encoding a carrier would use for s and its length in
// that encoding's units (septets for GSM-7, UTF-16 code units for UCS-2).
func Measure(s string) (Encoding, int) {
	enc := GSM7
	if !IsGSM7Text(s) {
		enc = UCS2
	}
	n := 0
	for _, r := range s {
		n += unitCost(enc, r)
	}
	return enc, n
}

func unitCost(enc Encoding, r rune) int {
	if enc == GSM7 {
		return SeptetCost(r)
	}
	if utf16.IsSurrogate(r) || r > 0xFFFF {
		return 2
	}
	return 1
}

// Segment limits: single-part and per-part sizes once a User Data Header is
// needed for concatenation.
const (
	GSM7Single = 160
	GSM7Multi  = 153
	UCS2Single = 70
	UCS2Multi  = 67
)

// SegmentCount returns how many SMS parts s will be billed as. An escape
// pair or surrogate pair is never split across parts, matching handset and
// SMSC behaviour.
func SegmentCount(s string) int {
	enc, n := Measure(s)
	if n == 0 {
		return 0
	}
	single, multi := GSM7Single, GSM7Multi
	if enc == UCS2 {
		single, multi = UCS2Single, UCS2Multi
	}
	if n <= single {
		return 1
	}
	segs, used := 1, 0
	for _, r := range s {
		c := unitCost(enc, r)
		if used+c > multi {
			segs++
			used = 0
		}
		used += c
	}
	return segs
}

// Stats describes a message as the carrier will bill it.
type Stats struct {
	Encoding Encoding
	Units    int
	Segments int
}

func Analyze(s string) Stats {
	enc, n := Measure(s)
	return Stats{Encoding: enc, Units: n, Segments: SegmentCount(s)}
}
