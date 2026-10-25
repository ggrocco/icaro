package crypto

import (
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k1, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	box, err := k1.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := k1.Open(box)
	if err != nil || string(out) != "secret" {
		t.Fatalf("open: %v %q", err, out)
	}
	box[len(box)-1] ^= 1
	if _, err := k1.Open(box); err == nil {
		t.Fatal("tampered box should fail")
	}
	other, _ := NewKey()
	if _, err := other.Open(box); err == nil {
		t.Fatal("wrong key should fail")
	}
	if *k1 == *other {
		t.Fatal("NewKey repeated a key")
	}
}

func TestOpenRejectsShortBox(t *testing.T) {
	k, _ := NewKey()
	if _, err := k.Open(make([]byte, 23)); err == nil {
		t.Fatal("short ciphertext accepted")
	}
}

func TestTokens(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) || len(hash) != 64 || HashToken(plain) != hash {
		t.Fatalf("token %q hash %q", plain, hash)
	}
	if !ConstantTimeEqual(hash, HashToken(plain)) || ConstantTimeEqual(hash, "x") {
		t.Fatal("compare")
	}
}
