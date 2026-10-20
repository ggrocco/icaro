package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestKeygenSignVerify(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"keygen"}, &out); err != nil {
		t.Fatal(err)
	}
	seed := regexp.MustCompile(`ICARO_SIGNING_KEY=(\S+)`).FindStringSubmatch(out.String())[1]
	pem := out.String()[strings.Index(out.String(), "-----BEGIN"):]
	dir := t.TempDir()
	in, sig, pub := filepath.Join(dir, "checksums.txt"), filepath.Join(dir, "checksums.txt.sig"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(in, []byte("abc  file\n"), 0o600)
	_ = os.WriteFile(pub, []byte(pem), 0o600)
	t.Setenv("ICARO_SIGNING_KEY", seed)
	if err := run([]string{"sign", "-in", in, "-out", sig}, &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-pub", pub, "-in", in, "-sig", sig}, &out); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(in, []byte("tampered\n"), 0o600)
	if err := run([]string{"verify", "-pub", pub, "-in", in, "-sig", sig}, &out); err == nil {
		t.Fatal("tampered file verified")
	}
	t.Setenv("ICARO_SIGNING_KEY", "short")
	if err := run([]string{"sign", "-in", in, "-out", sig}, &out); err == nil {
		t.Fatal("bad key accepted")
	}
	tmpl := filepath.Join(dir, "install.sh")
	_ = os.WriteFile(tmpl, []byte("K='"+placeholder+"'\n"), 0o600)
	t.Setenv("ICARO_RELEASE_PUBKEY", strings.TrimSpace(pem))
	dst := filepath.Join(dir, "out.sh")
	if err := run([]string{"embed", "-in", tmpl, "-out", dst}, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !strings.Contains(string(b), "BEGIN PUBLIC KEY") || strings.Contains(string(b), placeholder) {
		t.Fatalf("embed: %s", b)
	}
	if err := run([]string{"embed", "-in", dst, "-out", dst + "2"}, &out); err == nil {
		t.Fatal("embed without placeholder must fail")
	}
	if err := run(nil, &out); err == nil {
		t.Fatal("usage")
	}
	if err := run([]string{"nope"}, &out); err == nil {
		t.Fatal("unknown")
	}
}
