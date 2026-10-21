package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourcesRejectBadResponses(t *testing.T) {
	ctx := context.Background()
	body := map[string]string{}
	status := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := status[r.URL.Path]; ok {
			w.WriteHeader(c)
			return
		}
		fmt.Fprint(w, body[r.URL.Path])
	}))
	defer srv.Close()

	for name, b := range map[string]string{"json": "{", "no tag": `{"tag_name":""}`} {
		body["/repos/o/r/releases/latest"] = b
		if _, err := (&GitHub{Repo: "o/r", APIURL: srv.URL}).Latest(ctx); err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	status["/repos/o/r/releases/latest"] = http.StatusBadGateway
	if _, err := (&GitHub{Repo: "o/r", APIURL: srv.URL}).Latest(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("5xx: %v", err)
	}
	g := &GitHub{Repo: "o/r"}
	if rel := g.At("1.2.3"); rel.Version != "v1.2.3" || rel.BaseURL != "https://github.com/o/r/releases/download/v1.2.3" {
		t.Fatalf("default download URL: %+v", rel)
	}
	if g.api() != "https://api.github.com" {
		t.Fatal(g.api())
	}

	for _, tag := range []string{"", "v1 2", "../v1"} {
		body["/m/latest"] = tag
		if _, err := (&Mirror{BaseURL: srv.URL + "/m/"}).Latest(ctx); err == nil {
			t.Fatalf("tag %q accepted", tag)
		}
	}
	status["/gone/latest"] = http.StatusNotFound
	u := &Updater{Source: &Mirror{BaseURL: srv.URL + "/gone"}}
	if _, err := u.Check(ctx); err == nil || !strings.Contains(err.Error(), "find latest release") {
		t.Fatalf("check: %v", err)
	}
}

func TestGetLimitsAndErrors(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "icaro-update" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "0123456789")
	}))
	defer srv.Close()
	// A nil client uses the default one.
	if b, err := get(ctx, nil, srv.URL, 10); err != nil || string(b) != "0123456789" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := get(ctx, srv.Client(), srv.URL, 9); err == nil || !strings.Contains(err.Error(), "exceeds 9 bytes") {
		t.Fatalf("limit: %v", err)
	}
	if _, err := get(ctx, nil, "http://bad\x7f", 1); err == nil {
		t.Fatal("invalid URL")
	}
	srv.Close()
	if _, err := get(ctx, nil, srv.URL, 1); err == nil {
		t.Fatal("connection refused")
	}
}

// signedRelease serves checksums signed with priv plus the given archive.
func signedRelease(t *testing.T, priv ed25519.PrivateKey, sums, archive []byte) *httptest.Server {
	t.Helper()
	files := map[string][]byte{
		"/v0.2.0/" + ChecksumsFile:                           sums,
		"/v0.2.0/" + SignatureFile:                           ed25519.Sign(priv, sums),
		"/v0.2.0/" + ArchiveName("v0.2.0", "linux", "amd64"): archive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sumLine(name string, data []byte) []byte {
	s := sha256.Sum256(data)
	return []byte(hex.EncodeToString(s[:]) + "  " + name + "\n")
}

func TestApplyFailureModes(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	name := ArchiveName("v0.2.0", "linux", "amd64")
	newUpdater := func(srv *httptest.Server, exe string) *Updater {
		return &Updater{Exe: exe, PublicKey: pub, GOOS: "linux", GOARCH: "amd64", HTTP: srv.Client(),
			Smoke: func(context.Context, string) (string, error) { return "ok", nil }}
	}
	rel := func(srv *httptest.Server) Release { return (&Mirror{BaseURL: srv.URL}).At("v0.2.0") }

	t.Run("signed but malformed checksums", func(t *testing.T) {
		srv := signedRelease(t, priv, []byte("not a checksum line\n"), nil)
		if err := newUpdater(srv, currentBinary(t)).Apply(ctx, rel(srv)); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing signature", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ChecksumsFile) {
				_, _ = w.Write(sumLine(name, nil))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		if err := newUpdater(srv, currentBinary(t)).Apply(ctx, rel(srv)); err == nil || !strings.Contains(err.Error(), SignatureFile) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing checksums", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		if err := newUpdater(srv, currentBinary(t)).Apply(ctx, rel(srv)); err == nil || !strings.Contains(err.Error(), ChecksumsFile) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("checksummed archive without binary", func(t *testing.T) {
		archive := tarGz(t, "other", []byte("x"))
		srv := signedRelease(t, priv, sumLine(name, archive), archive)
		if err := newUpdater(srv, currentBinary(t)).Apply(ctx, rel(srv)); err == nil || !strings.Contains(err.Error(), "no icaro binary") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stage directory missing", func(t *testing.T) {
		archive := tarGz(t, "icaro", []byte("#!/bin/sh\n"))
		srv := signedRelease(t, priv, sumLine(name, archive), archive)
		exe := filepath.Join(t.TempDir(), "absent", "icaro")
		if err := newUpdater(srv, exe).Apply(ctx, rel(srv)); err == nil || !strings.Contains(err.Error(), "stage") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("default smoke test runs the staged binary", func(t *testing.T) {
		archive := tarGz(t, "icaro", []byte("#!/bin/sh\necho v0.2.0\n"))
		srv := signedRelease(t, priv, sumLine(name, archive), archive)
		exe := currentBinary(t)
		u := newUpdater(srv, exe)
		u.Smoke = nil
		if err := u.Apply(ctx, rel(srv)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "v0.2.0") {
			t.Fatalf("not swapped: %q", b)
		}
	})
}

func TestSwapAndCopy(t *testing.T) {
	dir := t.TempDir()
	// Without a current binary there is nothing to keep.
	if err := swap(filepath.Join(dir, "absent"), filepath.Join(dir, "staged")); err == nil || !strings.Contains(err.Error(), "keep previous") {
		t.Fatalf("err = %v", err)
	}
	exe := currentBinary(t)
	if err := swap(exe, filepath.Join(dir, "missing-staged")); err == nil || !strings.Contains(err.Error(), "swap") {
		t.Fatalf("err = %v", err)
	}

	// copyFile is the fallback when a hard link is impossible.
	dst := filepath.Join(dir, "copy")
	if err := copyFile(exe, dst); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dst)
	if b, _ := os.ReadFile(dst); string(b) != "old-binary" || fi.Mode().Perm() != 0o700 {
		t.Fatalf("copy: %q %v", b, fi.Mode())
	}
	if err := copyFile(filepath.Join(dir, "absent"), dst); err == nil {
		t.Fatal("missing source")
	}
	if err := copyFile(exe, filepath.Join(dir, "absent", "x")); err == nil {
		t.Fatal("missing destination dir")
	}
	if err := copyFile(dir, filepath.Join(dir, "dircopy")); err == nil {
		t.Fatal("a directory cannot be copied")
	}
}

func TestExecutable(t *testing.T) {
	exe, err := Executable()
	if err != nil || !filepath.IsAbs(exe) {
		t.Fatalf("%q %v", exe, err)
	}
}

func TestParseKeysAndChecksumsEdges(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if _, err := ParsePublicKey(ecPEM); err == nil || !strings.Contains(err.Error(), "not ed25519") {
		t.Fatalf("ecdsa key: %v", err)
	}
	junk := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("junk")}))
	if _, err := ParsePublicKey(junk); err == nil {
		t.Fatal("undecodable DER accepted")
	}

	long := strings.Repeat("a", 70<<10)
	if _, err := ParseChecksums([]byte(long)); err == nil {
		t.Fatal("an over-long line must fail")
	}
	sums, err := ParseChecksums([]byte("\n" + strings.Repeat("AB", 32) + " *bin\n"))
	if err != nil || sums["bin"] != strings.Repeat("ab", 32) {
		t.Fatalf("binary-mode entry: %v %v", sums, err)
	}
	if err := VerifyChecksum(map[string]string{"x": strings.Repeat("0", 64)}, "x", []byte("x")); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("digest mismatch: %v", err)
	}
}

func TestExtractBinaryEdges(t *testing.T) {
	// A corrupt tar stream inside valid gzip.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(bytes.Repeat([]byte{0xff}, 1024))
	_ = zw.Close()
	if _, err := extractBinary(gz.Bytes()); err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("corrupt tar: %v", err)
	}

	// A header claiming more than the download limit.
	var big bytes.Buffer
	zw = gzip.NewWriter(&big)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "icaro", Mode: 0o755, Size: maxDownload + 1, Typeflag: tar.TypeReg})
	_ = tw.Flush()
	_ = zw.Close()
	if _, err := extractBinary(big.Bytes()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("too large: %v", err)
	}

	// Directories named icaro are not the binary.
	var dir bytes.Buffer
	zw = gzip.NewWriter(&dir)
	tw = tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "icaro/", Mode: 0o755, Typeflag: tar.TypeDir})
	_ = tw.Close()
	_ = zw.Close()
	if _, err := extractBinary(dir.Bytes()); err == nil || !strings.Contains(err.Error(), "no icaro binary") {
		t.Fatalf("dir: %v", err)
	}
}

func TestParseVersionEdges(t *testing.T) {
	for _, c := range []struct {
		s  string
		ok bool
		v  version
	}{
		{"v1.2.3", true, version{nums: [3]int{1, 2, 3}}},
		{" 1.2.3-rc.1+build ", true, version{nums: [3]int{1, 2, 3}, pre: "rc.1"}},
		{"1.2.3+meta", true, version{nums: [3]int{1, 2, 3}}},
		{"1.2", false, version{}},
		{"1.-2.3", false, version{}},
		{"1.x.3", false, version{}},
	} {
		v, ok := parseVersion(c.s)
		if ok != c.ok || v != c.v {
			t.Errorf("%q: %+v %v", c.s, v, ok)
		}
	}
	if !Newer("v1.0.0-rc.1", "dev") {
		t.Fatal("any release is newer than dev")
	}
	if Newer("v1.0.0-rc.1", "v1.0.0-rc.1") {
		t.Fatal("equal prereleases")
	}
}
