package crypto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMasterKeyFileErrors(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	tmp := t.TempDir()

	// A directory where the key file should be is an I/O error, not "missing".
	dir := filepath.Join(tmp, "isdir")
	if err := os.MkdirAll(filepath.Join(dir, masterKeyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(dir, true); err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("unreadable key file: %v", err)
	}

	// The data directory cannot be created: a dangling symlink is in the way.
	link := filepath.Join(tmp, "dangling")
	if err := os.Symlink(filepath.Join(tmp, "nowhere", "deeper"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(link, true); err == nil {
		t.Fatal("data dir creation error swallowed")
	}

	// The key file cannot be written: it is a dangling symlink.
	wdir := filepath.Join(tmp, "w")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "nowhere", masterKeyFile), filepath.Join(wdir, masterKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(wdir, true); err == nil {
		t.Fatal("key write error swallowed")
	}

	// A corrupt key file is rejected rather than used.
	bad := filepath.Join(tmp, "bad")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, masterKeyFile), []byte("not base64!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(bad, false); err == nil {
		t.Fatal("corrupt key accepted")
	}
}

func TestOpenRejectsShortBox(t *testing.T) {
	k, _ := NewKey()
	if _, err := k.Open(make([]byte, 23)); err == nil {
		t.Fatal("short ciphertext accepted")
	}
}
