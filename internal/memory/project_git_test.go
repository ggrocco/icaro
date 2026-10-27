package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A repository's own config (written by an agent in its worktree) must
// not make the daemon's git run a program.
func TestGitIgnoresRepoFsmonitorAndHooks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "core.fsmonitor", hook},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test git
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), dir, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's core.fsmonitor ran")
	}
}
