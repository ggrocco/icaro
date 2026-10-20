package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture serves a mirror with one signed release.
type fixture struct {
	srv   *httptest.Server
	files map[string][]byte
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.data)
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func newFixture(t *testing.T, version string) *fixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fixture{files: map[string][]byte{}, pub: pub, priv: priv}
	archive := tarGz(t, "icaro", []byte("#!/bin/sh\necho "+version+"\n"))
	name := ArchiveName(version, "linux", "amd64")
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	f.files["/latest"] = []byte(version + "\n")
	f.files["/"+version+"/"+name] = archive
	f.files["/"+version+"/"+ChecksumsFile] = sums
	f.files["/"+version+"/"+SignatureFile] = ed25519.Sign(priv, sums)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) updater(t *testing.T, exe string) *Updater {
	return &Updater{
		Exe: exe, Current: "v0.1.0", Source: &Mirror{BaseURL: f.srv.URL}, PublicKey: f.pub, GOOS: "linux", GOARCH: "amd64",
		Smoke: func(_ context.Context, p string) (string, error) {
			b, err := os.ReadFile(p)
			if err != nil || !strings.HasPrefix(string(b), "#!/bin/sh") {
				return "", errors.New("not our binary")
			}
			return "ok", nil
		},
	}
}

func currentBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "icaro")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestUpdateSwapAndRollback(t *testing.T) {
	f := newFixture(t, "v0.2.0")
	exe := currentBinary(t)
	u := f.updater(t, exe)
	ctx := context.Background()
	plan, err := u.Check(ctx)
	if err != nil || !plan.Available || plan.Latest != "v0.2.0" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if err := u.Apply(ctx, plan.Release); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !strings.Contains(string(got), "v0.2.0") {
		t.Fatalf("binary not swapped: %q", got)
	}
	prev, _ := os.ReadFile(PrevPath(exe))
	if string(prev) != "old-binary" {
		t.Fatalf("previous not kept: %q", prev)
	}
	if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
		t.Fatal("staged binary left behind")
	}
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(exe)
	if string(got) != "old-binary" {
		t.Fatalf("rollback: %q", got)
	}
	if err := Rollback(exe); err == nil {
		t.Fatal("second rollback must fail")
	}
}

func TestUpdateRefusesTamperedArtifacts(t *testing.T) {
	ctx := context.Background()
	t.Run("checksum", func(t *testing.T) {
		f := newFixture(t, "v0.2.0")
		name := "/v0.2.0/" + ArchiveName("v0.2.0", "linux", "amd64")
		f.files[name] = tarGz(t, "icaro", []byte("#!/bin/sh\necho evil\n"))
		exe := currentBinary(t)
		err := f.updater(t, exe).Apply(ctx, (&Mirror{BaseURL: f.srv.URL}).At("v0.2.0"))
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("err = %v", err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old-binary" {
			t.Fatal("binary replaced despite mismatch")
		}
	})
	t.Run("signature", func(t *testing.T) {
		f := newFixture(t, "v0.2.0")
		f.files["/v0.2.0/"+ChecksumsFile] = append(f.files["/v0.2.0/"+ChecksumsFile], []byte("0000000000000000000000000000000000000000000000000000000000000000  x\n")...)
		err := f.updater(t, currentBinary(t)).Apply(ctx, (&Mirror{BaseURL: f.srv.URL}).At("v0.2.0"))
		if !errors.Is(err, ErrBadSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsigned", func(t *testing.T) {
		f := newFixture(t, "v0.2.0")
		u := f.updater(t, currentBinary(t))
		u.PublicKey = nil
		if err := u.Apply(ctx, (&Mirror{BaseURL: f.srv.URL}).At("v0.2.0")); err == nil {
			t.Fatal("unsigned update accepted")
		}
	})
	t.Run("smoke", func(t *testing.T) {
		f := newFixture(t, "v0.2.0")
		exe := currentBinary(t)
		u := f.updater(t, exe)
		u.Smoke = func(context.Context, string) (string, error) { return "boom", errors.New("exit 1") }
		if err := u.Apply(ctx, (&Mirror{BaseURL: f.srv.URL}).At("v0.2.0")); err == nil {
			t.Fatal("smoke failure accepted")
		}
		if _, err := os.Stat(StagedPath(exe)); !os.IsNotExist(err) {
			t.Fatal("failed staged binary left behind")
		}
	})
	t.Run("missing platform", func(t *testing.T) {
		f := newFixture(t, "v0.2.0")
		u := f.updater(t, currentBinary(t))
		u.GOARCH = "arm64"
		if err := u.Apply(ctx, (&Mirror{BaseURL: f.srv.URL}).At("v0.2.0")); err == nil {
			t.Fatal("missing archive accepted")
		}
	})
}

func TestRealSmokeTest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "icaro")
	_ = os.WriteFile(p, []byte("#!/bin/sh\necho v9\n"), 0o755)
	out, err := runVersion(context.Background(), p)
	if err != nil || strings.TrimSpace(out) != "v9" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestGitHubSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v1.4.0"}`)
	}))
	defer srv.Close()
	g := &GitHub{Repo: "o/r", APIURL: srv.URL, DownloadURL: "https://dl"}
	rel, err := g.Latest(context.Background())
	if err != nil || rel.Version != "v1.4.0" || rel.BaseURL != "https://dl/o/r/releases/download/v1.4.0" {
		t.Fatalf("%+v %v", rel, err)
	}
	if _, ok := NewSource("", nil).(*GitHub); !ok {
		t.Fatal("default source")
	}
	if _, ok := NewSource("https://m", nil).(*Mirror); !ok {
		t.Fatal("mirror source")
	}
	bad := &GitHub{Repo: "x/y", APIURL: srv.URL}
	if _, err := bad.Latest(context.Background()); err == nil {
		t.Fatal("404 must fail")
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.10.0", false},
		{"v2.0.0", "dev", true},
		{"v2.0.0", "d0aa983-dirty", true},
		{"v1.0.0", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.1", "v1.0.0", false},
		{"garbage", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Fatalf("Newer(%s, %s) = %v", c.latest, c.current, got)
		}
	}
}

func TestKeysAndChecksums(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pemKey, err := PublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{pemKey, base64.StdEncoding.EncodeToString(pub)} {
		k, err := ParsePublicKey(s)
		if err != nil || !k.Equal(pub) {
			t.Fatalf("parse %q: %v", s, err)
		}
	}
	for _, s := range []string{"", "-----BEGIN PUBLIC KEY-----\nxx\n-----END PUBLIC KEY-----", "c2hvcnQ="} {
		if _, err := ParsePublicKey(s); err == nil {
			t.Fatalf("%q accepted", s)
		}
	}
	data := []byte("x")
	sig := ed25519.Sign(priv, data)
	if VerifySignature(pub, data, []byte(base64.StdEncoding.EncodeToString(sig))) != nil {
		t.Fatal("base64 signature")
	}
	if VerifySignature(pub, data, []byte("nope")) == nil {
		t.Fatal("junk signature")
	}
	if _, err := ParseChecksums([]byte("abc  x\n")); err == nil {
		t.Fatal("short hash")
	}
	if _, err := ParseChecksums([]byte(strings.Repeat("z", 64) + "  x\n")); err == nil {
		t.Fatal("non-hex")
	}
	if err := VerifyChecksum(map[string]string{}, "x", data); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatal("unlisted file")
	}
	if _, err := extractBinary([]byte("not gzip")); err == nil {
		t.Fatal("bad archive")
	}
	if _, err := extractBinary(tarGz(t, "other", nil)); err == nil {
		t.Fatal("archive without binary")
	}
}
