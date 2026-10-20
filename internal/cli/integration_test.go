//go:build integration

package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCLISessionTTY drives the real binary: the daemon starts on demand,
// `icaro sh` creates a session for the current repository, the command
// gets a TTY, runs as the session user in /workspace, and its exit code
// becomes icaro's.
func TestCLISessionTTY(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("needs util-linux script for a pseudo-terminal")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker: %s", out)
	}
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(t.TempDir(), "icaro")
	build := exec.Command("go", "build", "-o", bin, "./cmd/icaro")
	build.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	home, err := os.MkdirTemp("/tmp", "icaro-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	profile := "cli" + time.Now().Format("150405")
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o700)
	cfg := "version: 1\nsessions:\n  default_profile: " + profile + "\nprofiles:\n  " + profile +
		":\n    image: mirror.gcr.io/library/busybox:1.36\n    egress: []\n"
	if err := os.WriteFile(filepath.Join(home, "config", "icaro.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	env := append(os.Environ(), "ICARO_HOME="+home, "ICARO_CONFIG=", "ICARO_MASTER_KEY="+base64.StdEncoding.EncodeToString(k))
	repo := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, b)
		}
	}
	icaro := func(args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Dir, c.Env = repo, env
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		err := c.Run()
		return b.String(), err
	}
	t.Cleanup(func() {
		out, _ := icaro("session", "list", "--json")
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `"id"`) {
				id := strings.Trim(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), `",`)
				_, _ = icaro("session", "rm", "--yes", id)
			}
		}
		if pid, err := os.ReadFile(filepath.Join(home, "run", "icaro.pid")); err == nil {
			_ = exec.Command("kill", strings.TrimSpace(string(pid))).Run()
		}
		_, _ = exec.Command("docker", "network", "rm", "icaro-egress-"+profile).CombinedOutput()
	})

	// script(1) gives icaro a real pseudo-terminal; -e returns the child's exit code.
	c := exec.Command("script", "-qec", bin+" sh -- sh -c 'tty; id -u; pwd; exit 3'", "/dev/null")
	c.Dir, c.Env = repo, env
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err = c.Run()
	out := strings.ReplaceAll(b.String(), "\r", "")
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit code must propagate (want 3): %v\n%s", err, out)
	}
	if !strings.Contains(out, "/dev/pts/") || !strings.Contains(out, "/workspace") {
		t.Fatalf("no TTY or wrong workdir:\n%s", out)
	}
	if uid := os.Getuid(); uid != 0 && !strings.Contains(out, "\n"+strconv.Itoa(uid)+"\n") {
		t.Fatalf("UID not preserved:\n%s", out)
	}
	// Second call reuses the session.
	out2, err := icaro("session", "list")
	if err != nil || strings.Count(out2, "running") != 1 {
		t.Fatalf("%v\n%s", err, out2)
	}
	if out, err := icaro("doctor"); err != nil && !strings.Contains(out, "orphaned sessions: none") {
		t.Logf("doctor: %s", out)
	}
}
