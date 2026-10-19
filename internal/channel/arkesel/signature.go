package arkesel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Tolerance is Arkesel's documented replay window for signed callbacks.
const Tolerance = 5 * time.Minute

var (
	ErrMissingSignature  = errors.New("arkesel: missing webhook signature headers")
	ErrStaleTimestamp    = errors.New("arkesel: webhook timestamp outside the 5 minute window")
	ErrSignatureMismatch = errors.New("arkesel: webhook signature mismatch")
)

// Sign returns an X-Arkesel-Webhook-Signature value for payload, as Arkesel
// computes it (used by tests and local tooling).
func Sign(secret string, timestamp int64, payload any) (string, error) {
	c, err := CanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	return "v1=" + hex.EncodeToString(mac(secret, strconv.FormatInt(timestamp, 10)+"."+c)), nil
}

// Verify checks a signed callback and returns its signing time. The header
// may carry several comma-separated "v1=" values during secret rotation;
// any one matching is enough. The timestamp is used exactly as received.
func Verify(secret, signatureHeader, timestampHeader string, payload any, now time.Time) (time.Time, error) {
	if signatureHeader == "" || timestampHeader == "" {
		return time.Time{}, ErrMissingSignature
	}
	if !allDigits(timestampHeader) {
		return time.Time{}, ErrSignatureMismatch
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return time.Time{}, ErrSignatureMismatch
	}
	signedAt := time.Unix(ts, 0)
	if skew := now.Sub(signedAt); skew > Tolerance || skew < -Tolerance {
		return time.Time{}, ErrStaleTimestamp
	}
	c, err := CanonicalJSON(payload)
	if err != nil {
		return time.Time{}, err
	}
	want := mac(secret, timestampHeader+"."+c)
	for _, part := range strings.Split(signatureHeader, ",") {
		sig, ok := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		if !ok {
			continue
		}
		if got, err := hex.DecodeString(sig); err == nil && hmac.Equal(got, want) {
			return signedAt, nil
		}
	}
	return time.Time{}, ErrSignatureMismatch
}

func mac(secret, msg string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// CanonicalJSON encodes v the way Arkesel's PHP signer does:
// json_encode(recursively key-sorted v, JSON_UNESCAPED_SLASHES). Object keys
// are sorted, list order is kept, "/" is not escaped, and every non-ASCII
// character is written as \uXXXX (UTF-16, lower-case hex). Supported values
// are string, bool, nil, json.Number, map[string]any, map[string]string and
// []any.
func CanonicalJSON(v any) (string, error) {
	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		return writePHPString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		return writeCanonical(b, m)
	case map[string]any:
		if len(x) == 0 {
			b.WriteString("[]") // PHP cannot tell an empty map from an empty list
			return nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writePHPString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("arkesel: cannot canonicalize %T", v)
	}
	return nil
}

// writePHPString matches PHP json_encode string escaping without
// JSON_UNESCAPED_UNICODE (and with JSON_UNESCAPED_SLASHES).
func writePHPString(b *strings.Builder, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("arkesel: callback payload is not valid UTF-8")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case r < utf8.RuneSelf:
				b.WriteRune(r)
			case r <= 0xFFFF:
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
			}
		}
	}
	b.WriteByte('"')
	return nil
}
