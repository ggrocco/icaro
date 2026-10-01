package crypto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMasterKeyLifecycle(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	dir := t.TempDir()
	if _, err := LoadMasterKey(dir, false); err == nil {
		t.Fatal("expected missing key error")
	}
	k1, err := LoadMasterKey(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, masterKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, st.Mode())
	}
	k2, err := LoadMasterKey(dir, false)
	if err != nil || *k1 != *k2 {
		t.Fatalf("reload mismatch: %v", err)
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
}

func TestMasterKeyFromEnv(t *testing.T) {
	t.Setenv(MasterKeyEnv, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	k, err := LoadMasterKey(t.TempDir(), false)
	if err != nil || k[0] != 0 {
		t.Fatal(err)
	}
	t.Setenv(MasterKeyEnv, "short")
	if _, err := LoadMasterKey(t.TempDir(), false); err == nil {
		t.Fatal("expected bad key error")
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
