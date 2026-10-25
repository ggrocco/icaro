// Package crypto provides the master key, secret sealing and API token
// generation used by the engine.
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/nacl/secretbox"
)

// MasterKeyEnv supplies the master key (base64, 32 bytes) instead of the
// OS key store; internal/secrets resolves the key (headless/CI override).
const MasterKeyEnv = "ICARO_MASTER_KEY"

// Key is a 32-byte secretbox key.
type Key [32]byte

// NewKey generates a random key. The error is always nil: crypto/rand.Read
// never fails (it crashes the process instead).
func NewKey() (*Key, error) {
	var k Key
	_, _ = rand.Read(k[:])
	return &k, nil
}

// Seal encrypts plaintext; the random nonce is prefixed to the box.
func (k *Key) Seal(plaintext []byte) ([]byte, error) {
	var nonce [24]byte
	_, _ = rand.Read(nonce[:]) // never fails, see NewKey
	return secretbox.Seal(nonce[:], plaintext, &nonce, (*[32]byte)(k)), nil
}

// Open decrypts a box produced by Seal.
func (k *Key) Open(box []byte) ([]byte, error) {
	if len(box) < 24 {
		return nil, errors.New("ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], box[:24])
	out, ok := secretbox.Open(nil, box[24:], &nonce, (*[32]byte)(k))
	if !ok {
		return nil, errors.New("decryption failed")
	}
	return out, nil
}

// TokenPrefix marks icaro API tokens.
const TokenPrefix = "icaro_"

// NewToken returns a random bearer token and its hash.
func NewToken() (plain, hash string, err error) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails, see NewKey
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return plain, HashToken(plain), nil
}

// HashToken returns the hex SHA-256 of a token; the store keeps only this.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqual compares two strings without leaking length-independent timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
