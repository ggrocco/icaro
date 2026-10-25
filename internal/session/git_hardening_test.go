package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// plantRepo returns a repository whose config and hooks run a program
// that appends to a marker file: what an agent would plant if it could
// write .git/config or .git/hooks (ADR 0011 mounts them read-only; the
// host git options are the second line).
func plantRepo(t *testing.T) (repo, marker string) {
	t.Helper()
	repo = t.TempDir()
	gitInit(t, repo)
	marker = filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$0 $*\" >> "+marker+"\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test hook
		t.Fatal(err)
	}
	hooks := filepath.Join(repo, ".git", "hooks")
	_ = os.MkdirAll(hooks, 0o700)
	for _, h := range []string{"post-checkout", "pre-push", "reference-transaction", "post-index-change"} {
		if err := os.Symlink(script, filepath.Join(hooks, h)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "config", "core.fsmonitor", script) //nolint:gosec // test fixture
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return repo, marker
}

func TestHostGitRunsNoPlantedHooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, marker := plantRepo(t)

	// Control: plain git runs the planted programs, so this test can fail.
	cmd := exec.Command("git", "checkout", "-q", "-b", "control")
	cmd.Dir = repo
	_ = cmd.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("plain git did not run the planted hook, so the check below proves nothing: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	if err := addWorktree(ctx, repo, wt, "icaro/x", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRepo(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, wt, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, wt, "commit", "-q", "--allow-empty", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktree(ctx, repo, wt); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("host git ran a planted program: %s", b)
	}
}

func TestHostGitRefusesTheFileTransport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := t.TempDir()
	gitInit(t, src)
	repo := t.TempDir()
	gitInit(t, repo)
	if _, err := git(ctx, repo, "ls-remote", src); err == nil || !strings.Contains(err.Error(), "file") {
		t.Fatalf("a local-path remote must be refused: %v", err)
	}
	if _, err := git(ctx, repo, "ls-remote", "file://"+src); err == nil {
		t.Fatal("a file:// remote must be refused")
	}
	// A workflow's configured source may still be a local path.
	dst := filepath.Join(t.TempDir(), "cache.git")
	if _, err := gitSource(ctx, filepath.Dir(dst), "clone", "--mirror", "--quiet", src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := gitSource(ctx, dst, "remote", "update", "--prune"); err != nil {
		t.Fatal(err)
	}
}

func TestHardeningOptions(t *testing.T) {
	t.Parallel()
	joined := strings.Join(hardening, " ")
	for _, want := range []string{"core.fsmonitor=false", "core.hooksPath=/dev/null", "protocol.file.allow=never", "submodule.recurse=false"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
}
