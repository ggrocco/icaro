package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"

	"icaro/internal/crypto"
)

// The master key seals every secret in the database. It lives in the OS
// secret store (macOS Keychain, Linux Secret Service); only when none is
// available does it fall back to a password-encrypted 0600 file, and only
// after the user gave a password explicitly (03-security).

// Key backends.
const (
	BackendEnv      = "env"            // ICARO_MASTER_KEY (headless/CI override)
	BackendKeychain = "keychain"       // macOS `security`
	BackendSecret   = "secret-service" // Linux `secret-tool`
	BackendFile     = "password-file"  // scrypt + secretbox file
	BackendLegacy   = "plaintext-file" // Phase 0 master.key, migrated away on load
)

// PasswordEnv supplies the fallback file password non-interactively.
const PasswordEnv = "ICARO_PASSWORD"

const (
	keyringService = "icaro"
	keyringAccount = "master-key"
	encFile        = "master.key.enc"
	legacyFile     = "master.key"
)

// ErrNoBackend reports that no OS secret store is reachable and no
// password was provided for the file fallback.
var ErrNoBackend = errors.New("no OS secret store is available: set " + PasswordEnv + " (encrypted-file fallback) or " + crypto.MasterKeyEnv)

// Runner runs a command with optional stdin and returns stdout.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name/args are the fixed security/secret-tool invocations above
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Keyring is an OS secret store holding one value per (service, account).
type Keyring interface {
	Name() string
	Available(ctx context.Context) bool
	Get(ctx context.Context) (string, bool, error)
	Set(ctx context.Context, value string) error
}

// macKeychain uses the `security` CLI.
type macKeychain struct{ run Runner }

func (k macKeychain) Name() string { return BackendKeychain }

func (k macKeychain) Available(ctx context.Context) bool {
	_, err := exec.LookPath("security")
	if err != nil {
		return false
	}
	_, err = k.run(ctx, nil, "security", "default-keychain")
	return err == nil
}

func (k macKeychain) Get(ctx context.Context) (string, bool, error) {
	out, err := k.run(ctx, nil, "security", "find-generic-password", "-s", keyringService, "-a", keyringAccount, "-w")
	if err != nil {
		if strings.Contains(err.Error(), "could not be found") || strings.Contains(err.Error(), "exit status 44") {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

func (k macKeychain) Set(ctx context.Context, value string) error {
	// `security -i` reads commands from stdin, so the value never appears
	// in the process list. -U updates an existing item.
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l icaro-master-key -w %s\n", keyringService, keyringAccount, value)
	_, err := k.run(ctx, []byte(line), "security", "-i")
	return err
}

// secretService uses libsecret's `secret-tool`.
type secretService struct{ run Runner }

func (k secretService) Name() string { return BackendSecret }

func (k secretService) Available(ctx context.Context) bool {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return false
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	// A lookup of a missing item exits 1 with no output when the service
	// works, and fails with a D-Bus error when it does not.
	_, err := k.run(ctx, nil, "secret-tool", "lookup", "service", keyringService, "account", "probe")
	return err == nil || !strings.Contains(err.Error(), "org.freedesktop")
}

func (k secretService) Get(ctx context.Context) (string, bool, error) {
	out, err := k.run(ctx, nil, "secret-tool", "lookup", "service", keyringService, "account", keyringAccount)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") && !strings.Contains(err.Error(), "org.freedesktop") {
			return "", false, nil
		}
		return "", false, err
	}
	v := strings.TrimSpace(string(out))
	return v, v != "", nil
}

func (k secretService) Set(ctx context.Context, value string) error {
	_, err := k.run(ctx, []byte(value), "secret-tool", "store", "--label=Ícaro master key", "service", keyringService, "account", keyringAccount)
	return err
}

// DefaultKeyring returns the platform's OS secret store, or nil.
func DefaultKeyring(run Runner) Keyring {
	if run == nil {
		run = execRunner
	}
	switch runtime.GOOS {
	case "darwin":
		return macKeychain{run: run}
	case "linux":
		return secretService{run: run}
	}
	return nil
}

// KeyOptions configure master key loading.
type KeyOptions struct {
	// Dir is the secrets directory (~/.icaro/secrets).
	Dir string
	// Keyring is the OS secret store; nil disables it.
	Keyring Keyring
	// Password supplies the fallback file password; nil means none was
	// given (ICARO_PASSWORD is consulted first).
	Password func() (string, error)
	// Create generates a key when none exists.
	Create bool
}

// KeyInfo reports where the key came from.
type KeyInfo struct {
	Backend string
	// Warning is set when the key is stored less safely than it should be.
	Warning string
}

// LoadMasterKey resolves the master key: ICARO_MASTER_KEY, then the OS
// secret store, then the password-encrypted file. A Phase 0 plaintext
// key file is moved into the best available backend and deleted.
func LoadMasterKey(ctx context.Context, o KeyOptions) (*crypto.Key, KeyInfo, error) {
	if v := os.Getenv(crypto.MasterKeyEnv); v != "" {
		k, err := decodeKey(v)
		return k, KeyInfo{Backend: BackendEnv}, err
	}
	password := func() (string, bool, error) {
		if v := os.Getenv(PasswordEnv); v != "" {
			return v, true, nil
		}
		if o.Password == nil {
			return "", false, nil
		}
		p, err := o.Password()
		if err != nil {
			return "", false, err
		}
		return p, p != "", nil
	}
	ring := o.Keyring
	if ring != nil && !ring.Available(ctx) {
		ring = nil
	}
	if ring != nil {
		if v, ok, err := ring.Get(ctx); err != nil {
			return nil, KeyInfo{}, fmt.Errorf("%s: %w", ring.Name(), err)
		} else if ok {
			k, err := decodeKey(v)
			return k, KeyInfo{Backend: ring.Name()}, err
		}
	}
	encPath := filepath.Join(o.Dir, encFile)
	if _, err := os.Stat(encPath); err == nil {
		pw, ok, err := password()
		if err != nil {
			return nil, KeyInfo{}, err
		}
		if !ok {
			return nil, KeyInfo{}, fmt.Errorf("master key is password-protected (%s): set %s", encPath, PasswordEnv)
		}
		k, err := readEncrypted(encPath, pw)
		return k, KeyInfo{Backend: BackendFile}, err
	}

	// Legacy plaintext key from Phase 0, or a brand-new key.
	var key *crypto.Key
	legacyPath := filepath.Join(o.Dir, legacyFile)
	if raw, err := os.ReadFile(legacyPath); err == nil {
		k, err := decodeKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, KeyInfo{}, err
		}
		key = k
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, KeyInfo{}, err
	} else if !o.Create {
		return nil, KeyInfo{}, errors.New("master key not found: run `icaro init`")
	} else {
		k, err := crypto.NewKey()
		if err != nil {
			return nil, KeyInfo{}, err
		}
		key = k
	}

	enc := base64.StdEncoding.EncodeToString(key[:])
	info := KeyInfo{}
	switch {
	case ring != nil:
		if err := ring.Set(ctx, enc); err != nil {
			return nil, KeyInfo{}, fmt.Errorf("%s: %w", ring.Name(), err)
		}
		info.Backend = ring.Name()
	default:
		pw, ok, err := password()
		if err != nil {
			return nil, KeyInfo{}, err
		}
		if !ok {
			if _, err := os.Stat(legacyPath); err == nil {
				// Keep working, loudly, rather than lock the user out.
				return key, KeyInfo{Backend: BackendLegacy, Warning: "master key is stored unencrypted at " + legacyPath + "; set " + PasswordEnv + " once to encrypt it"}, nil
			}
			return nil, KeyInfo{}, ErrNoBackend
		}
		if err := writeEncrypted(encPath, key, pw); err != nil {
			return nil, KeyInfo{}, err
		}
		info.Backend = BackendFile
	}
	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, KeyInfo{}, err
	}
	return key, info, nil
}

func decodeKey(b64 string) (*crypto.Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("master key must be 32 bytes, base64 encoded")
	}
	var k crypto.Key
	copy(k[:], raw)
	return &k, nil
}

// encryptedKey is the fallback file format.
type encryptedKey struct {
	KDF  string `json:"kdf"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
	Box  string `json:"box"`
}

// scryptN is the scrypt cost; tests lower it.
var scryptN = 1 << 15

func deriveKey(password string, salt []byte, n, r, p int) (*[32]byte, error) {
	dk, err := scrypt.Key([]byte(password), salt, n, r, p, 32)
	if err != nil {
		return nil, err
	}
	var k [32]byte
	copy(k[:], dk)
	return &k, nil
}

func writeEncrypted(path string, key *crypto.Key, password string) error {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt) // never fails: crypto/rand.Read crashes the process instead
	ek := encryptedKey{KDF: "scrypt", N: scryptN, R: 8, P: 1, Salt: base64.StdEncoding.EncodeToString(salt)}
	dk, err := deriveKey(password, salt, ek.N, ek.R, ek.P)
	if err != nil {
		return err
	}
	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	ek.Box = base64.StdEncoding.EncodeToString(secretbox.Seal(nonce[:], key[:], &nonce, dk))
	b, err := json.MarshalIndent(ek, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func readEncrypted(path, password string) (*crypto.Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ek encryptedKey
	if err := json.Unmarshal(b, &ek); err != nil || ek.KDF != "scrypt" {
		return nil, fmt.Errorf("%s: unsupported key file", path)
	}
	salt, err := base64.StdEncoding.DecodeString(ek.Salt)
	if err != nil {
		return nil, err
	}
	box, err := base64.StdEncoding.DecodeString(ek.Box)
	if err != nil || len(box) < 24 {
		return nil, fmt.Errorf("%s: corrupt key file", path)
	}
	dk, err := deriveKey(password, salt, ek.N, ek.R, ek.P)
	if err != nil {
		return nil, err
	}
	var nonce [24]byte
	copy(nonce[:], box[:24])
	plain, ok := secretbox.Open(nil, box[24:], &nonce, dk)
	if !ok || len(plain) != 32 {
		return nil, errors.New("wrong password for the master key file")
	}
	var k crypto.Key
	copy(k[:], plain)
	return &k, nil
}
