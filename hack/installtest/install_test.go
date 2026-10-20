// Package installtest exercises install.sh against a local release mirror.
package installtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"icaro/internal/update"
)

type mirror struct {
	files map[string][]byte
	srv   *httptest.Server
	pem   string
}

func newMirror(t *testing.T, version string, tamper ...func(files map[string][]byte)) *mirror {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "openssl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pem, err := update.PublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	requireRawinEd25519(t, pem, priv)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/sh\necho " + version + "\n")
	_ = tw.WriteHeader(&tar.Header{Name: "icaro", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	archive := buf.Bytes()
	name := update.ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	m := &mirror{pem: pem, files: map[string][]byte{
		"/latest":                            []byte(version + "\n"),
		"/" + version + "/" + name:           archive,
		"/" + version + "/checksums.txt":     sums,
		"/" + version + "/checksums.txt.sig": ed25519.Sign(priv, sums),
	}}
	for _, fn := range tamper {
		fn(m.files)
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := m.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// requireRawinEd25519 skips when the local openssl cannot verify ed25519
// with -rawin (macOS ships LibreSSL, which cannot; install.sh then tells
// the user to install OpenSSL 3).
func requireRawinEd25519(t *testing.T, pem string, priv ed25519.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	data, key, sig := filepath.Join(dir, "d"), filepath.Join(dir, "k.pem"), filepath.Join(dir, "s")
	_ = os.WriteFile(data, []byte("probe"), 0o600)
	_ = os.WriteFile(key, []byte(pem), 0o600)
	_ = os.WriteFile(sig, ed25519.Sign(priv, []byte("probe")), 0o600)
	if err := exec.Command("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", key, "-rawin", "-in", data, "-sigfile", sig).Run(); err != nil { //nolint:gosec // fixed probe command
		t.Skipf("openssl cannot verify ed25519 with -rawin (needs OpenSSL 3.0+): %v", err)
	}
}

func (m *mirror) install(t *testing.T, binDir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script, "--base-url", m.srv.URL, "--bin-dir", binDir}, args...)...) //nolint:gosec // test runs the repo's install.sh
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallVerifiesAndInstalls(t *testing.T) {
	m := newMirror(t, "v0.4.0")
	bin := filepath.Join(t.TempDir(), "bin")
	out, err := m.install(t, bin, []string{"ICARO_INSTALL_PUBKEY=" + m.pem})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "signature OK") || !strings.Contains(out, "checksum OK") {
		t.Fatalf("checks not reported:\n%s", out)
	}
	got, err := exec.Command(filepath.Join(bin, "icaro")).Output() //nolint:gosec // the binary this test just installed
	if err != nil || strings.TrimSpace(string(got)) != "v0.4.0" {
		t.Fatalf("installed binary: %q %v", got, err)
	}
}

func TestInstallBlocksTamperedArchive(t *testing.T) {
	name := "/v0.4.0/" + update.ArchiveName("v0.4.0", runtime.GOOS, runtime.GOARCH)
	m := newMirror(t, "v0.4.0", func(f map[string][]byte) { f[name] = append(append([]byte(nil), f[name]...), 0) })
	bin := filepath.Join(t.TempDir(), "bin")
	out, err := m.install(t, bin, []string{"ICARO_INSTALL_PUBKEY=" + m.pem}, "--version", "v0.4.0")
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered archive accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "icaro")); !os.IsNotExist(err) {
		t.Fatal("binary installed despite checksum mismatch")
	}
}

func TestInstallBlocksForgedChecksums(t *testing.T) {
	m := newMirror(t, "v0.4.0", func(f map[string][]byte) {
		f["/v0.4.0/checksums.txt"] = append(append([]byte(nil), f["/v0.4.0/checksums.txt"]...), []byte("# forged\n")...)
	})
	bin := filepath.Join(t.TempDir(), "bin")
	out, err := m.install(t, bin, []string{"ICARO_INSTALL_PUBKEY=" + m.pem})
	if err == nil || !strings.Contains(out, "signature verification FAILED") {
		t.Fatalf("forged checksums accepted: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "icaro")); !os.IsNotExist(err) {
		t.Fatal("binary installed despite bad signature")
	}
}

func TestInstallRefusesWithoutKey(t *testing.T) {
	m := newMirror(t, "v0.4.0")
	bin := filepath.Join(t.TempDir(), "bin")
	out, err := m.install(t, bin, []string{"ICARO_INSTALL_PUBKEY="})
	if err == nil || !strings.Contains(out, "no release public key") {
		t.Fatalf("installed without a key: %v\n%s", err, out)
	}
	out, err = m.install(t, bin, nil, "--insecure-skip-signature")
	if err != nil || !strings.Contains(out, "WARNING") {
		t.Fatalf("skip-signature: %v\n%s", err, out)
	}
}
