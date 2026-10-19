// Package security provides versioned at-rest encryption (AES-256-GCM),
// versioned keyed lookup hashes (HMAC-SHA256) and webhook signature checks.
//
// Every ciphertext and lookup hash is stored next to the key_version that
// produced it, so a new CURRENT_KEY_VERSION can be introduced while older
// rows remain readable (plan ADR-008).
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// Purpose strings are bound into ciphertexts as GCM additional data so a
// ciphertext copied from one column cannot be decrypted as another.
const (
	PurposePhone      = "phone"
	PurposeMessage    = "message"
	PurposeOverflow   = "overflow"
	PurposeUSSDAnswer = "ussd_answer"
)

var ErrUnknownKeyVersion = errors.New("security: unknown key version")

type Keyring struct {
	current int
	aeads   map[int]cipher.AEAD
	hmacs   map[int][]byte
}

// NewKeyring builds a keyring. The current version must have both an
// encryption key (exactly 32 bytes) and an HMAC key (at least 32 bytes).
func NewKeyring(current int, encKeys, hmacKeys map[int][]byte) (*Keyring, error) {
	k := &Keyring{current: current, aeads: map[int]cipher.AEAD{}, hmacs: map[int][]byte{}}
	for v, key := range encKeys {
		if len(key) != 32 {
			return nil, fmt.Errorf("security: encryption key v%d must be 32 bytes", v)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.aeads[v] = aead
	}
	for v, key := range hmacKeys {
		if len(key) < 32 {
			return nil, fmt.Errorf("security: hmac key v%d must be at least 32 bytes", v)
		}
		k.hmacs[v] = append([]byte(nil), key...)
	}
	if _, ok := k.aeads[current]; !ok {
		return nil, fmt.Errorf("security: no encryption key for current version %d", current)
	}
	if _, ok := k.hmacs[current]; !ok {
		return nil, fmt.Errorf("security: no hmac key for current version %d", current)
	}
	return k, nil
}

func (k *Keyring) CurrentVersion() int { return k.current }

// Encrypt seals plaintext with the current key. Output is nonce||ciphertext.
func (k *Keyring) Encrypt(purpose string, plaintext []byte) ([]byte, int, error) {
	aead := k.aeads[k.current]
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, err
	}
	return aead.Seal(nonce, nonce, plaintext, []byte(purpose)), k.current, nil
}

func (k *Keyring) Decrypt(purpose string, sealed []byte, version int) ([]byte, error) {
	aead, ok := k.aeads[version]
	if !ok {
		return nil, fmt.Errorf("%w %d", ErrUnknownKeyVersion, version)
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("security: ciphertext too short")
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ct, []byte(purpose))
}

func (k *Keyring) EncryptString(purpose, s string) ([]byte, int, error) {
	return k.Encrypt(purpose, []byte(s))
}

func (k *Keyring) DecryptString(purpose string, sealed []byte, version int) (string, error) {
	b, err := k.Decrypt(purpose, sealed, version)
	return string(b), err
}

// VersionedHash is a lookup hash together with the key version that made it.
type VersionedHash struct {
	Version int
	Hash    string
}

// LookupHash returns hex(HMAC-SHA256(key[version], value)).
func (k *Keyring) LookupHash(version int, value string) (string, error) {
	key, ok := k.hmacs[version]
	if !ok {
		return "", fmt.Errorf("%w %d", ErrUnknownKeyVersion, version)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// LookupHashes returns the value's hash under every configured HMAC key,
// current version first. Used to find rows written before a key rotation.
func (k *Keyring) LookupHashes(value string) []VersionedHash {
	versions := make([]int, 0, len(k.hmacs))
	for v := range k.hmacs {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i] == k.current {
			return true
		}
		if versions[j] == k.current {
			return false
		}
		return versions[i] > versions[j]
	})
	out := make([]VersionedHash, 0, len(versions))
	for _, v := range versions {
		h, _ := k.LookupHash(v, value)
		out = append(out, VersionedHash{Version: v, Hash: h})
	}
	return out
}
