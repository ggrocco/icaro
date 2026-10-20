package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func setupEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	t.Setenv("ICARO_MASTER_KEY", base64.StdEncoding.EncodeToString(k))
	return home
}

func TestSetupTwiceChangesNothingWithoutConsent(t *testing.T) {
	home := setupEnv(t)
	out, err := runCLI(t, "", "setup", "--yes", "--skip-images")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfgPath := filepath.Join(home, "config", "icaro.yaml")
	first, err := os.ReadFile(cfgPath)
	if err != nil || !strings.Contains(string(first), "claude") {
		t.Fatalf("%v %s", err, first)
	}
	fi1, _ := os.Stat(cfgPath)

	// Second run, no --yes, non-interactive stdin: nothing to change, so
	// it succeeds without asking and leaves the file alone.
	out, err = runCLI(t, "", "setup", "--skip-images")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "everything is up to date") {
		t.Fatalf("%s", out)
	}
	second, _ := os.ReadFile(cfgPath)
	fi2, _ := os.Stat(cfgPath)
	if !bytes.Equal(first, second) || !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("second setup rewrote the config")
	}

	// A hand-edited file missing a profile: setup proposes a diff but
	// refuses to write without consent.
	edited := "version: 1\n# my comment\nprofiles:\n  claude:\n    egress: [api.anthropic.com]\n"
	_ = os.WriteFile(cfgPath, []byte(edited), 0o600)
	out, err = runCLI(t, "", "setup", "--skip-images")
	if err == nil || !strings.Contains(out, "+ ") {
		t.Fatalf("expected a diff and a refusal: %v\n%s", err, out)
	}
	if now, _ := os.ReadFile(cfgPath); string(now) != edited {
		t.Fatal("config changed without consent")
	}
}

func TestDesiredConfigAndDiff(t *testing.T) {
	out, changed, err := DesiredConfig(nil)
	if err != nil || !changed || !strings.Contains(string(out), "version: 1") || !strings.Contains(string(out), "codex") {
		t.Fatalf("%v %s", err, out)
	}
	again, changed, err := DesiredConfig(out)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Fatal("not idempotent")
	}
	if _, _, err := DesiredConfig([]byte("a: [")); err == nil {
		t.Fatal("bad yaml")
	}
	d := LineDiff("a\nb\nc\n", "a\nc\nd\n")
	if d != "  a\n- b\n  c\n+ d\n" {
		t.Fatalf("%q", d)
	}
}

func TestLoginArgsAndExec(t *testing.T) {
	setupEnv(t)
	if out, err := runCLI(t, "", "egress-proxy", "--help"); err != nil || !strings.Contains(out, "allowlist") {
		t.Fatalf("%v %s", err, out)
	}
}
