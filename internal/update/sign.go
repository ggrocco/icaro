package update

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ErrBadSignature reports a checksums file whose signature does not verify.
var ErrBadSignature = errors.New("release signature does not verify")

// ErrChecksumMismatch reports a downloaded file whose digest is not the
// one listed in the signed checksums file.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ParsePublicKey accepts a base64 raw 32-byte key or a PEM "PUBLIC KEY"
// (SPKI), the form install.sh hands to openssl.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no release public key configured")
	}
	if strings.HasPrefix(s, "-----BEGIN") {
		blk, _ := pem.Decode([]byte(s))
		if blk == nil {
			return nil, errors.New("invalid PEM public key")
		}
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("public key is not ed25519")
		}
		return pub, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be base64 of 32 bytes or a PEM ed25519 key")
	}
	return ed25519.PublicKey(raw), nil
}

// PublicKeyPEM renders pub as a PEM SPKI block.
func PublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// VerifySignature checks a raw (64-byte) or base64 detached ed25519
// signature over data.
func VerifySignature(pub ed25519.PublicKey, data, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil || len(dec) != ed25519.SignatureSize {
			return ErrBadSignature
		}
		sig = dec
	}
	if !ed25519.Verify(pub, data, sig) {
		return ErrBadSignature
	}
	return nil
}

// ParseChecksums reads a sha256sum-style file ("<hex>  <name>").
func ParseChecksums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("malformed checksums line %q", line)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("malformed checksum %q", fields[0])
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out, sc.Err()
}

// VerifyChecksum compares data's sha256 with the entry for name.
func VerifyChecksum(sums map[string]string, name string, data []byte) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%w: %s is not listed in checksums.txt", ErrChecksumMismatch, name)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%w: %s", ErrChecksumMismatch, name)
	}
	return nil
}
