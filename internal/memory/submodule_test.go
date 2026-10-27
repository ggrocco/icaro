package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always", "-C", dir}
	if out, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test git
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// Files tracked in a checked-out submodule are indexed like the project's
// own; untracked ones inside it are not.
func TestGitListedIncludesSubmodules(t *testing.T) {
	lib := t.TempDir()
	runGit(t, lib, "init", "-q")
	if err := os.WriteFile(filepath.Join(lib, "lib.go"), []byte("package lib\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, lib, "add", ".")
	runGit(t, lib, "commit", "-q", "-m", "lib")

	app := t.TempDir()
	runGit(t, app, "init", "-q")
	if err := os.WriteFile(filepath.Join(app, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, app, "submodule", "add", "-q", lib, "vendor/lib")
	runGit(t, app, "add", ".")
	runGit(t, app, "commit", "-q", "-m", "app")
	if err := os.WriteFile(filepath.Join(app, "vendor", "lib", "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, dirs, ok := gitListed(context.Background(), app)
	if !ok {
		t.Fatal("a git work tree")
	}
	if !files["main.go"] || !files["vendor/lib/lib.go"] || !dirs["vendor/lib"] {
		t.Fatalf("project and submodule files: %v %v", files, dirs)
	}
	if files["vendor/lib/scratch.txt"] {
		t.Fatal("untracked files inside a submodule are not listed")
	}
}
