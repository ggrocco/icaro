//go:build e2e

// Package quickstart runs the shell blocks of docs/quickstart.md that are
// tagged ```sh quickstart against a fresh daemon and a real Docker
// engine, so the quickstart cannot drift from the binary (Phase 10).
//
//	go test -tags e2e ./hack/quickstart
package quickstart

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// blocks returns the bodies of ```sh quickstart fences in order.
func blocks(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	var cur *strings.Builder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case cur == nil && strings.TrimSpace(line) == "```sh quickstart":
			cur = &strings.Builder{}
		case cur != nil && strings.TrimSpace(line) == "```":
			out = append(out, cur.String())
			cur = nil
		case cur != nil:
			cur.WriteString(line + "\n")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuickstart(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	bin := t.TempDir()
	args := []string{"build", "-o", filepath.Join(bin, "icaro")}
	if os.Getenv("GOCOVERDIR") != "" { // make cover-e2e
		args = append(args, "-cover", "-coverpkg=icaro/...")
	}
	build := exec.Command("go", append(args, "./cmd/icaro")...) //nolint:gosec // builds this repository
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// A short ICARO_HOME keeps the socket path under the Unix limit.
	home, err := os.MkdirTemp("/tmp", "icaro-qs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(filepath.Join(home, "run", "icaro.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				// A clean exit is when an instrumented daemon writes coverage.
				for i := 0; i < 150 && syscall.Kill(pid, 0) == nil; i++ {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		_ = os.RemoveAll(home)
	})
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ICARO_HOME="+home,
		"ICARO_PASSWORD=quickstart-test",
		"ICARO_CONFIG=",
	)
	bs := blocks(t, filepath.Join(root, "docs", "quickstart.md"))
	if len(bs) < 4 {
		t.Fatalf("found %d quickstart blocks; the doc lost its tags", len(bs))
	}
	for i, b := range bs {
		cmd := exec.Command("sh", "-e", "-c", b) //nolint:gosec // runs the documented quickstart commands
		cmd.Dir = root
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		t.Logf("block %d:\n%s\n%s", i+1, b, out)
		if err != nil {
			t.Fatalf("block %d failed: %v", i+1, err)
		}
	}
}
