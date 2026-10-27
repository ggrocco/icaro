package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/mount"
)

// gitRun runs git in dir for test setup.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...) //nolint:gosec // test setup with fixed arguments
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// mountModes maps each mount target to "ro" or "rw".
func mountModes(ms []mount.Mount) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		out[m.Target] = map[bool]string{true: "ro", false: "rw"}[m.ReadOnly]
	}
	return out
}

// TestGitMounts: in a linked worktree with a submodule, everything the
// host's git reads to decide what to run is read-only, missing hooks are
// created empty, each protected path's parents are mount points, and what
// a commit writes stays writable.
func TestGitMounts(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo, ws := filepath.Join(root, "repo"), filepath.Join(root, "wt", "s1")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "icaro/s1", ws)
	gitDir := filepath.Join(repo, ".git")
	if err := os.RemoveAll(filepath.Join(gitDir, "hooks")); err != nil {
		t.Fatal(err)
	}
	// A submodule whose name has a slash, without hooks/.
	sub := filepath.Join(gitDir, "modules", "lib", "x")
	for _, d := range []string{filepath.Join(sub, "objects"), filepath.Join(sub, "refs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"HEAD", "config"} {
		if err := os.WriteFile(filepath.Join(sub, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	admin := filepath.Join(gitDir, "worktrees", "s1")

	spec, _ := AgentSpec{Name: "s", Image: "i", Workspace: ws, GitDir: gitDir}.Build()
	got := mountModes(spec.HostConfig.Mounts)
	want := map[string]string{
		WorkspacePath: "rw", gitDir: "rw",
		gitDir + "/config": "ro", gitDir + "/hooks": "ro",
		gitDir + "/worktrees": "ro", admin: "rw",
		admin + "/commondir": "ro", admin + "/gitdir": "ro", admin + "/config.worktree": "ro",
		WorkspacePath + "/.git": "ro",
		gitDir + "/modules":     "rw", gitDir + "/modules/lib": "rw", sub: "rw",
		sub + "/config": "ro", sub + "/hooks": "ro",
	}
	for target, mode := range want {
		if got[target] != mode {
			t.Errorf("%s: %q, want %q", target, got[target], mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}
	for _, d := range []string{filepath.Join(gitDir, "hooks"), filepath.Join(sub, "hooks")} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s must be created empty: %v", d, err)
		}
	}
	if fi, err := os.Stat(admin + "/config.worktree"); err != nil || fi.Size() != 0 {
		t.Errorf("config.worktree must be created empty: %v", err)
	}
	// Building again changes nothing on the host and yields the same mounts.
	again, _ := AgentSpec{Name: "s", Image: "i", Workspace: ws, GitDir: gitDir}.Build()
	if len(again.HostConfig.Mounts) != len(spec.HostConfig.Mounts) {
		t.Fatal("Build must be repeatable")
	}
}

// TestGitMountsWithoutWorktree: without a linked worktree every admin
// directory's files are protected one by one, and a hooks entry that is
// not a directory is covered by a read-only tmpfs.
func TestGitMountsWithoutWorktree(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	other := filepath.Join(gitDir, "worktrees", "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(gitDir, "config"), filepath.Join(gitDir, "hooks"), filepath.Join(gitDir, "worktrees", "stray"), filepath.Join(other, "commondir")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ms := gitMounts("", gitDir)
	got := mountModes(ms)
	want := map[string]string{
		gitDir: "rw", gitDir + "/config": "ro", gitDir + "/hooks": "ro",
		gitDir + "/worktrees": "rw", other: "rw", other + "/commondir": "ro",
	}
	for target, mode := range want {
		if got[target] != mode {
			t.Errorf("%s: %q, want %q", target, got[target], mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}
	for _, m := range ms {
		if m.Target == gitDir+"/hooks" && m.Type != mount.TypeTmpfs {
			t.Errorf("a file named hooks is covered by a tmpfs: %+v", m)
		}
	}
}

// TestWorktreeAdmin: only a .git file that points into the common
// directory's worktrees/ names an admin directory.
func TestWorktreeAdmin(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	gitDir := filepath.Join(root, ".git")
	admin := filepath.Join(gitDir, "worktrees", "a")
	ws := filepath.Join(root, "ws")
	for _, d := range []string{admin, ws} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"gitdir: " + admin + "\n":        admin,
		"gitdir: ../.git/worktrees/a":    admin, // relative to the worktree
		"gitdir: " + root + "/elsewhere": "",
		"not a gitdir line":              "",
	}
	for content, want := range cases {
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := worktreeAdmin(ws, gitDir); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
	if worktreeAdmin("", gitDir) != "" || worktreeAdmin(filepath.Join(root, "missing"), gitDir) != "" {
		t.Error("no worktree, no admin directory")
	}
}
