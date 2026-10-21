package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"icaro/internal/crypto"
)

var errBoom = errors.New("boom")

// scriptedRing is a Keyring whose Get/Set results are fixed.
type scriptedRing struct {
	val            string
	getErr, setErr error
}

func (r *scriptedRing) Name() string                   { return "scripted" }
func (r *scriptedRing) Available(context.Context) bool { return true }
func (r *scriptedRing) Get(context.Context) (string, bool, error) {
	return r.val, r.val != "", r.getErr
}
func (r *scriptedRing) Set(_ context.Context, v string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.val = v
	return nil
}

func noEnv(t *testing.T) {
	t.Setenv(crypto.MasterKeyEnv, "")
	t.Setenv(PasswordEnv, "")
}

func legacyKey(t *testing.T, dir string) *crypto.Key {
	t.Helper()
	k, _ := crypto.NewKey()
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(base64.StdEncoding.EncodeToString(k[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestLoadMasterKeyFailures(t *testing.T) {
	noEnv(t)
	ctx := context.Background()
	failPw := func() (string, error) { return "", errBoom }

	// The OS store answers with an error, or with garbage.
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir(), Keyring: &scriptedRing{getErr: errBoom}}); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "scripted") {
		t.Fatalf("keyring get: %v", err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir(), Keyring: &scriptedRing{val: "short"}}); err == nil {
		t.Fatal("garbage from the keyring accepted")
	}

	// Storing the key in the OS store fails: nothing is half-migrated.
	dir := t.TempDir()
	legacyKey(t, dir)
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Keyring: &scriptedRing{setErr: errBoom}}); !errors.Is(err, errBoom) {
		t.Fatalf("keyring set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFile)); err != nil {
		t.Fatal("legacy key deleted although the migration failed")
	}

	// The password prompt fails, for an existing file and for a new one.
	dir = t.TempDir()
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Create: true, Password: func() (string, error) { return "pw", nil }}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Password: failPw}); !errors.Is(err, errBoom) {
		t.Fatalf("prompt for an existing file: %v", err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir(), Create: true, Password: failPw}); !errors.Is(err, errBoom) {
		t.Fatalf("prompt for a new file: %v", err)
	}
	// An empty answer counts as no password.
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: t.TempDir(), Create: true, Password: func() (string, error) { return "", nil }}); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("empty password: %v", err)
	}

	// The legacy key file is unreadable or corrupt.
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, legacyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir, Create: true}); err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("unreadable legacy key: %v", err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir}); err == nil {
		t.Fatal("corrupt legacy key accepted")
	}

	// The secrets directory is not a directory: an I/O error, not "missing".
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswordEnv, "pw")
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: filepath.Join(parent, "file", "secrets"), Create: true}); err == nil {
		t.Fatal("secrets dir under a file")
	}
	// The encrypted file cannot be written (the directory is a dangling link).
	dangling := filepath.Join(parent, "dangling")
	if err := os.Symlink(filepath.Join(parent, "nowhere", "deeper"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dangling, Create: true}); err == nil {
		t.Fatal("unwritable secrets dir")
	}
}

// TestLegacyKeyNotRemovable: if the plaintext key cannot be deleted after
// the migration, loading fails loudly instead of leaving it behind.
func TestLegacyKeyNotRemovable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can delete from a read-only directory")
	}
	noEnv(t)
	dir := t.TempDir()
	legacyKey(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a read-only directory is the point
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore for TempDir cleanup
	if _, _, err := LoadMasterKey(context.Background(), KeyOptions{Dir: dir, Keyring: &scriptedRing{}}); err == nil {
		t.Fatal("undeletable plaintext key ignored")
	}
}

func TestLegacyMigrationToPasswordFile(t *testing.T) {
	noEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	k := legacyKey(t, dir)
	t.Setenv(PasswordEnv, "pw")
	got, info, err := LoadMasterKey(ctx, KeyOptions{Dir: dir})
	if err != nil || info.Backend != BackendFile || *got != *k {
		t.Fatalf("%v %+v", err, info)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFile)); !os.IsNotExist(err) {
		t.Fatal("plaintext key left behind")
	}
	again, _, err := LoadMasterKey(ctx, KeyOptions{Dir: dir})
	if err != nil || *again != *k {
		t.Fatalf("reload: %v", err)
	}
}

// TestEncryptedFileValidation: every way a key file can be damaged is a
// clear error, never a different key.
func TestEncryptedFileValidation(t *testing.T) {
	k, _ := crypto.NewKey()
	good := filepath.Join(t.TempDir(), encFile)
	if err := writeEncrypted(good, k, "pw"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	var ek encryptedKey
	if err := json.Unmarshal(raw, &ek); err != nil {
		t.Fatal(err)
	}
	write := func(mut func(e *encryptedKey)) string {
		e := ek
		mut(&e)
		b, _ := json.Marshal(e)
		p := filepath.Join(t.TempDir(), encFile)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"other kdf":   write(func(e *encryptedKey) { e.KDF = "argon2" }),
		"bad salt":    write(func(e *encryptedKey) { e.Salt = "%%%" }),
		"bad box":     write(func(e *encryptedKey) { e.Box = "%%%" }),
		"short box":   write(func(e *encryptedKey) { e.Box = base64.StdEncoding.EncodeToString([]byte("short")) }),
		"bad scrypt":  write(func(e *encryptedKey) { e.N = 3 }),
		"wrong salt":  write(func(e *encryptedKey) { e.Salt = base64.StdEncoding.EncodeToString(make([]byte, 16)) }),
		"is a dir":    t.TempDir(),
		"not json":    filepath.Join(t.TempDir(), "nojson"),
		"missing key": filepath.Join(t.TempDir(), "missing"),
	}
	_ = os.WriteFile(cases["not json"], []byte("{"), 0o600)
	for name, p := range cases {
		if got, err := readEncrypted(p, "pw"); err == nil {
			t.Errorf("%s: accepted, key %x", name, got[:4])
		}
	}
	if got, err := readEncrypted(good, "pw"); err != nil || *got != *k {
		t.Fatalf("intact file: %v", err)
	}

	// An unusable scrypt cost fails the write instead of writing a file
	// nobody can open.
	defer func(n int) { scryptN = n }(scryptN)
	scryptN = 3
	if err := writeEncrypted(filepath.Join(t.TempDir(), encFile), k, "pw"); err == nil {
		t.Fatal("invalid scrypt parameters accepted")
	}
}

func TestExecRunner(t *testing.T) {
	ctx := context.Background()
	out, err := execRunner(ctx, []byte("from stdin"), "cat")
	if err != nil || string(out) != "from stdin" {
		t.Fatalf("%q %v", out, err)
	}
	_, err = execRunner(ctx, nil, "sh", "-c", "echo 'item could not be found' >&2; exit 44")
	if err == nil || !strings.Contains(err.Error(), "exit status 44") || !strings.Contains(err.Error(), "could not be found") {
		t.Fatalf("stderr and status must be in the error: %v", err)
	}
}

// fakeTool puts an executable named name on PATH (Available checks it
// with LookPath before probing through the runner).
func fakeTool(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func runnerReturning(out string, err error) Runner {
	return func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(out), err }
}

func TestMacKeychain(t *testing.T) {
	ctx := context.Background()
	if (macKeychain{}).Name() != BackendKeychain {
		t.Fatal("name")
	}
	fakeTool(t, "")
	if (macKeychain{run: runnerReturning("", nil)}).Available(ctx) {
		t.Fatal("available without the security tool")
	}
	fakeTool(t, "security")
	if !(macKeychain{run: runnerReturning("", nil)}).Available(ctx) {
		t.Fatal("keychain with a default keychain")
	}
	if (macKeychain{run: runnerReturning("", errBoom)}).Available(ctx) {
		t.Fatal("keychain without a default keychain (e.g. over ssh)")
	}
	for _, msg := range []string{"The specified item could not be found in the keychain.", "exit status 44"} {
		if _, ok, err := (macKeychain{run: runnerReturning("", errors.New(msg))}).Get(ctx); ok || err != nil {
			t.Fatalf("%q must read as absent: %v %v", msg, ok, err)
		}
	}
	if _, _, err := (macKeychain{run: runnerReturning("", errBoom)}).Get(ctx); !errors.Is(err, errBoom) {
		t.Fatalf("other errors surface: %v", err)
	}
}

func TestSecretService(t *testing.T) {
	ctx := context.Background()
	if (secretService{}).Name() != BackendSecret {
		t.Fatal("name")
	}
	ok := runnerReturning("", nil)
	fakeTool(t, "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	if (secretService{run: ok}).Available(ctx) {
		t.Fatal("available without secret-tool")
	}
	fakeTool(t, "secret-tool")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if (secretService{run: ok}).Available(ctx) {
		t.Fatal("available without a session bus (headless server)")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	if !(secretService{run: ok}).Available(ctx) {
		t.Fatal("working service")
	}
	if !(secretService{run: runnerReturning("", errors.New("exit status 1"))}).Available(ctx) {
		t.Fatal("a missing probe item still means the service works")
	}
	if (secretService{run: runnerReturning("", errors.New("org.freedesktop.DBus.Error.ServiceUnknown"))}).Available(ctx) {
		t.Fatal("a D-Bus error means no service")
	}

	if _, found, err := (secretService{run: runnerReturning("", errors.New("exit status 1"))}).Get(ctx); found || err != nil {
		t.Fatalf("missing item: %v %v", found, err)
	}
	if _, _, err := (secretService{run: runnerReturning("", errors.New("exit status 1: org.freedesktop.Secret.Error.IsLocked"))}).Get(ctx); err == nil {
		t.Fatal("a locked collection is an error, not absence")
	}
	if _, found, err := (secretService{run: runnerReturning("  \n", nil)}).Get(ctx); found || err != nil {
		t.Fatalf("empty value: %v %v", found, err)
	}
}

func TestDefaultKeyring(t *testing.T) {
	ring := DefaultKeyring(nil)
	switch runtime.GOOS {
	case "darwin":
		if _, ok := ring.(macKeychain); !ok {
			t.Fatalf("%T", ring)
		}
	case "linux":
		if _, ok := ring.(secretService); !ok {
			t.Fatalf("%T", ring)
		}
	default:
		if ring != nil {
			t.Fatalf("%T", ring)
		}
	}
}
