package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	ErrMissingSignature = errors.New("security: missing webhook signature")
	ErrBadSignature     = errors.New("security: webhook signature mismatch")
	ErrStaleTimestamp   = errors.New("security: webhook timestamp outside tolerance")
)

// SignWebhook returns "sha256=<hex>" over "<timestamp>.<body>".
func SignWebhook(secret string, timestamp int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(timestamp, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifyWebhook checks a timestamped HMAC-SHA256 signature computed over the
// raw request body. It must run before the body is parsed (plan §10.1).
func VerifyWebhook(secret, signatureHeader, timestampHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	if signatureHeader == "" || timestampHeader == "" {
		return ErrMissingSignature
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return ErrStaleTimestamp
	}
	expected := SignWebhook(secret, ts, body)
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signatureHeader))) {
		return ErrBadSignature
	}
	return nil
}

// ConstantTimeEqual compares two secrets without leaking timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
