package container

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/mount"
)

// gitMounts mounts the repository's common .git directory at its host
// path, so the worktree's .git file resolves, and keeps everything the
// host's git reads to decide what to execute out of the agent's reach
// (ADR 0011):
//
//   - config, config.worktree and hooks/ of the repository and of every
//     submodule under modules/ are read-only; a missing hooks/ is created
//     empty first so the agent cannot add one;
//   - worktrees/ is read-only except the session's own admin directory,
//     whose commondir, gitdir and config.worktree are read-only too;
//   - the worktree's .git file at /workspace/.git is read-only.
//
// Objects, refs, logs and the session's index and HEAD stay writable, so
// the agent can commit. Every directory between .git and a read-only path
// is a mount point of its own: a mount point cannot be renamed, so the
// agent cannot move a protected path's parent aside and recreate it.
func gitMounts(workspace, gitDir string) []mount.Mount {
	g := &gitGuard{seen: map[string]bool{}}
	g.bind(gitDir, gitDir, false)
	admin := worktreeAdmin(workspace, gitDir)
	if admin != "" {
		g.bind(filepath.Join(gitDir, "worktrees"), filepath.Join(gitDir, "worktrees"), true)
		g.bind(admin, admin, false)
		g.file(filepath.Join(admin, "commondir"), false)
		g.file(filepath.Join(admin, "gitdir"), false)
		g.file(filepath.Join(admin, "config.worktree"), true)
		dotgit := filepath.Join(workspace, ".git")
		if fi, err := os.Lstat(dotgit); err == nil && fi.Mode().IsRegular() {
			g.bind(dotgit, WorkspacePath+"/.git", true)
		}
	}
	g.repo(gitDir, admin == "")
	return g.mounts
}

// worktreeAdmin returns the session worktree's admin directory (the
// target of its .git file), or "" when workspace is not a linked worktree
// of gitDir.
func worktreeAdmin(workspace, gitDir string) string {
	if workspace == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(workspace, ".git")) //nolint:gosec // the session's own worktree
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workspace, dir)
	}
	dir = resolve(dir)
	if filepath.Dir(dir) != filepath.Join(resolve(gitDir), "worktrees") {
		return ""
	}
	return filepath.Join(gitDir, "worktrees", filepath.Base(dir))
}

// gitGuard collects the mounts of gitMounts without duplicates.
type gitGuard struct {
	mounts []mount.Mount
	seen   map[string]bool
}

func (g *gitGuard) bind(src, dst string, ro bool) {
	if g.seen[dst] {
		return
	}
	g.seen[dst] = true
	g.mounts = append(g.mounts, mount.Mount{Type: mount.TypeBind, Source: src, Target: dst, ReadOnly: ro})
}

// pin makes dir a mount point of its own (read-write), so it cannot be
// renamed while a read-only path below it is protected.
func (g *gitGuard) pin(dir string) { g.bind(dir, dir, false) }

// file mounts the regular file p read-only; with create, a missing p is
// created empty first.
func (g *gitGuard) file(p string, create bool) {
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) && create {
		if f, cerr := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); cerr == nil { //nolint:gosec // an empty git config file
			_ = f.Close()
			fi, err = os.Lstat(p)
		}
	}
	if err == nil && fi.Mode().IsRegular() {
		g.bind(p, p, true)
	}
}

// hooks mounts dir/hooks read-only, creating it empty when missing. If it
// cannot be created, an empty read-only tmpfs takes its place.
func (g *gitGuard) hooks(dir string) {
	p := filepath.Join(dir, "hooks")
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		_ = os.Mkdir(p, 0o755) //nolint:gosec // git's own default mode for hooks/
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		g.bind(p, p, true)
		return
	}
	if !g.seen[p] {
		g.seen[p] = true
		g.mounts = append(g.mounts, mount.Mount{Type: mount.TypeTmpfs, Target: p, ReadOnly: true, TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 1 << 16}})
	}
}

// repo protects the git directory dir: its config, config.worktree and
// hooks, its submodules' git directories and, with worktrees, the admin
// files of its linked worktrees.
func (g *gitGuard) repo(dir string, worktrees bool) {
	g.file(filepath.Join(dir, "config"), false)
	g.file(filepath.Join(dir, "config.worktree"), false)
	g.hooks(dir)
	if worktrees {
		wt := filepath.Join(dir, "worktrees")
		if entries, err := os.ReadDir(wt); err == nil {
			g.pin(wt)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				admin := filepath.Join(wt, e.Name())
				g.pin(admin)
				for _, f := range []string{"commondir", "gitdir", "config.worktree"} {
					g.file(filepath.Join(admin, f), false)
				}
			}
		}
	}
	g.modules(filepath.Join(dir, "modules"))
}

// modules walks a modules/ tree. Submodule names may contain slashes, so
// git directories can sit several levels down, and nested submodules live
// in their own modules/.
func (g *gitGuard) modules(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	g.pin(dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if isGitDir(sub) {
			g.pin(sub)
			g.repo(sub, true)
			continue
		}
		g.modules(sub)
	}
}

// isGitDir reports whether dir looks like a git directory.
func isGitDir(dir string) bool {
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && objects.IsDir()
}
