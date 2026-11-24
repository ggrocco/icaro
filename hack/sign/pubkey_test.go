package main

import (
	"bytes"
	"strings"
	"testing"
)

// The release variable may hold the key as base64 or as the PEM block
// keygen prints (pasted through a web form, so with CRLF line ends); the
// linker needs the one-line base64 form either way.
func TestPubkeyNormalizesEveryForm(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"keygen"}, &out); err != nil {
		t.Fatal(err)
	}
	var b64 string
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "public key (base64): "); ok {
			b64 = v
		}
	}
	pem := out.String()[strings.Index(out.String(), "-----BEGIN"):]
	if b64 == "" || !strings.Contains(pem, "END PUBLIC KEY") {
		t.Fatalf("keygen output: %s", out.String())
	}
	for name, in := range map[string]string{
		"base64":      b64,
		"padded":      "  " + b64 + "\n",
		"pem":         pem,
		"pem as CRLF": strings.ReplaceAll(pem, "\n", "\r\n"),
	} {
		t.Setenv("ICARO_RELEASE_PUBKEY", in)
		var got bytes.Buffer
		if err := run([]string{"pubkey"}, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.String() != b64+"\n" {
			t.Errorf("%s: got %q, want %q", name, got.String(), b64)
		}
	}
	for name, in := range map[string]string{"unset": "", "not a key": "hello"} {
		t.Setenv("ICARO_RELEASE_PUBKEY", in)
		if err := run([]string{"pubkey"}, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "ICARO_RELEASE_PUBKEY") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
