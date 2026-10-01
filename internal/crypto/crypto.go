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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
)

// MasterKeyEnv overrides the on-disk master key (base64, 32 bytes).
const MasterKeyEnv = "ICARO_MASTER_KEY"

const masterKeyFile = "master.key"

// Key is a 32-byte secretbox key.
type Key [32]byte

// LoadMasterKey reads the key from the environment or <dataDir>/master.key.
// With create=true a missing file is generated (mode 0600).
func LoadMasterKey(dataDir string, create bool) (*Key, error) {
	if v := os.Getenv(MasterKeyEnv); v != "" {
		return decodeKey(v)
	}
	path := filepath.Join(dataDir, masterKeyFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return decodeKey(strings.TrimSpace(string(raw)))
	case errors.Is(err, os.ErrNotExist) && create:
		k, err := NewKey()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(k[:]) + "\n"
		if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
			return nil, err
		}
		return k, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("master key not found: run `icaro init` or set %s", MasterKeyEnv)
	default:
		return nil, err
	}
}

// NewKey generates a random key.
func NewKey() (*Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return nil, err
	}
	return &k, nil
}

func decodeKey(b64 string) (*Key, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("master key must be 32 bytes, base64 encoded")
	}
	var k Key
	copy(k[:], raw)
	return &k, nil
}

// Seal encrypts plaintext; the random nonce is prefixed to the box.
func (k *Key) Seal(plaintext []byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
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
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
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
