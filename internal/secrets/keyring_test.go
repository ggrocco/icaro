package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"icaro/internal/crypto"
)

func init() { scryptN = 1 << 10 }

type memRing struct {
	avail bool
	val   string
	sets  int
}

func (m *memRing) Name() string                              { return "mem" }
func (m *memRing) Available(context.Context) bool            { return m.avail }
func (m *memRing) Get(context.Context) (string, bool, error) { return m.val, m.val != "", nil }
func (m *memRing) Set(_ context.Context, v string) error {
	m.val = v
	m.sets++
	return nil
}

func TestKeyringBackend(t *testing.T) {
	t.Setenv(crypto.MasterKeyEnv, "")
	t.Setenv(PasswordEnv, "")
	ctx := context.Background()
	ring := &memRing{avail: true}
	dir := t.TempDir()
	k1, info, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: ring, Create: true})
	if err != nil || info.Backend != "mem" || ring.sets != 1 {
		t.Fatalf("%v %+v", err, info)
	}
	k2, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: ring})
	if err != nil || *k1 != *k2 || ring.sets != 1 {
		t.Fatal("reload from keyring")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal("keyring backend must not write files")
	}
}

func TestPasswordFileFallback(t *testing.T) {
	t.Setenv(crypto.MasterKeyEnv, "")
	t.Setenv(PasswordEnv, "")
	ctx := context.Background()
	dir := t.TempDir()
	ring := &memRing{avail: false}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: ring, Create: true}); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("without a password the fallback must refuse: %v", err)
	}
	pw := func() (string, error) { return "correct horse", nil }
	k1, info, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: ring, Create: true, Password: pw})
	if err != nil || info.Backend != BackendFile {
		t.Fatalf("%v %+v", err, info)
	}
	fi, err := os.Stat(filepath.Join(dir, encFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	t.Setenv(PasswordEnv, "correct horse")
	k2, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir})
	if err != nil || *k1 != *k2 {
		t.Fatalf("reload: %v", err)
	}
	t.Setenv(PasswordEnv, "wrong")
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir}); err == nil {
		t.Fatal("wrong password accepted")
	}
	t.Setenv(PasswordEnv, "")
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir}); err == nil {
		t.Fatal("missing password accepted")
	}
}

func TestLegacyMigrationAndEnv(t *testing.T) {
	t.Setenv(PasswordEnv, "")
	t.Setenv(crypto.MasterKeyEnv, "")
	ctx := context.Background()
	dir := t.TempDir()
	k, _ := crypto.NewKey()
	enc := base64.StdEncoding.EncodeToString(k[:])
	_ = os.WriteFile(filepath.Join(dir, legacyFile), []byte(enc+"\n"), 0o600)
	// No backend, no password: keep working with a warning.
	got, info, err := LoadMasterKey(ctx, KeyOptions{Dir: dir})
	if err != nil || info.Backend != BackendLegacy || info.Warning == "" || *got != *k {
		t.Fatalf("%v %+v", err, info)
	}
	ring := &memRing{avail: true}
	got, info, err = LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: ring})
	if err != nil || info.Backend != "mem" || *got != *k {
		t.Fatalf("migrate: %v %+v", err, info)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFile)); !os.IsNotExist(err) {
		t.Fatal("plaintext key not removed after migration")
	}
	t.Setenv(crypto.MasterKeyEnv, enc)
	got, info, err = LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir()})
	if err != nil || info.Backend != BackendEnv || *got != *k {
		t.Fatal("env override")
	}
	t.Setenv(crypto.MasterKeyEnv, "short")
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir}); err == nil {
		t.Fatal("bad env key accepted")
	}
	t.Setenv(crypto.MasterKeyEnv, "")
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir()}); err == nil {
		t.Fatal("missing key without Create")
	}
}

func TestOSKeyringCommands(t *testing.T) {
	var calls [][]string
	var stdin []string
	run := func(_ context.Context, in []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		stdin = append(stdin, string(in))
		return []byte("dmFsdWU=\n"), nil
	}
	mk := macKeychain{run: run}
	if v, ok, err := mk.Get(context.Background()); err != nil || !ok || v != "dmFsdWU=" {
		t.Fatal(v, ok, err)
	}
	if err := mk.Set(context.Background(), "SECRETVALUE"); err != nil {
		t.Fatal(err)
	}
	for _, a := range calls[len(calls)-1] {
		if a == "SECRETVALUE" {
			t.Fatal("value leaked into argv")
		}
	}
	ss := secretService{run: run}
	if err := ss.Set(context.Background(), "SECRETVALUE"); err != nil || stdin[len(stdin)-1] != "SECRETVALUE" {
		t.Fatal("secret-tool reads the value from stdin")
	}
	if v, ok, _ := ss.Get(context.Background()); !ok || v == "" {
		t.Fatal("secret-tool get")
	}
}
