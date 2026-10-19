package security

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestEncryptDecryptAcrossVersions(t *testing.T) {
	v1, err := NewKeyring(1, map[int][]byte{1: testKey(1)}, map[int][]byte{1: testKey(11)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, ver, err := v1.EncryptString(PurposePhone, "+233244123456")
	if err != nil || ver != 1 {
		t.Fatalf("encrypt: ver=%d err=%v", ver, err)
	}

	// Rotate: v2 becomes current but v1 stays configured for reads.
	v2, err := NewKeyring(2, map[int][]byte{1: testKey(1), 2: testKey(2)}, map[int][]byte{1: testKey(11), 2: testKey(12)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.DecryptString(PurposePhone, sealed, 1)
	if err != nil || got != "+233244123456" {
		t.Fatalf("decrypt old row: %q %v", got, err)
	}
	_, ver, _ = v2.EncryptString(PurposePhone, "x")
	if ver != 2 {
		t.Fatalf("new writes must use v2, got v%d", ver)
	}
}

func TestPurposeIsBound(t *testing.T) {
	k, _ := NewKeyring(1, map[int][]byte{1: testKey(1)}, map[int][]byte{1: testKey(11)})
	sealed, ver, _ := k.EncryptString(PurposePhone, "secret")
	if _, err := k.DecryptString(PurposeMessage, sealed, ver); err == nil {
		t.Fatal("ciphertext must not decrypt under a different purpose")
	}
}

func TestUnknownVersion(t *testing.T) {
	k, _ := NewKeyring(1, map[int][]byte{1: testKey(1)}, map[int][]byte{1: testKey(11)})
	if _, err := k.Decrypt(PurposePhone, []byte("0123456789abcdef"), 9); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("want ErrUnknownKeyVersion, got %v", err)
	}
}

func TestLookupHashesCurrentFirst(t *testing.T) {
	k, _ := NewKeyring(2, map[int][]byte{1: testKey(1), 2: testKey(2)}, map[int][]byte{1: testKey(11), 2: testKey(12)})
	hs := k.LookupHashes("+233244123456")
	if len(hs) != 2 || hs[0].Version != 2 || hs[1].Version != 1 {
		t.Fatalf("unexpected order: %+v", hs)
	}
	if hs[0].Hash == hs[1].Hash {
		t.Fatal("hashes under different keys must differ")
	}
	again, _ := k.LookupHash(1, "+233244123456")
	if again != hs[1].Hash {
		t.Fatal("lookup hash must be deterministic")
	}
}

func TestWebhookSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"text":"hi"}`)
	sig := SignWebhook("s3cret", now.Unix(), body)
	ts := "1800000000"

	if err := VerifyWebhook("s3cret", sig, ts, body, now, 5*time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifyWebhook("s3cret", sig, ts, []byte(`{"text":"HI"}`), now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body accepted: %v", err)
	}
	if err := VerifyWebhook("other", sig, ts, body, now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	if err := VerifyWebhook("s3cret", sig, ts, body, now.Add(10*time.Minute), 5*time.Minute); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("stale timestamp accepted: %v", err)
	}
	if err := VerifyWebhook("s3cret", "", ts, body, now, 5*time.Minute); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("missing signature accepted: %v", err)
	}
}
