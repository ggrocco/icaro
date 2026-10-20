package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// git runs git with args in dir and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	// Worktrees handed to the container user (root daemon) are owned by
	// another uid; the daemon manages them, so ownership checks don't apply.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*"}, args...)...) //nolint:gosec // argv assembled by this package
	cmd.Dir = dir
	// Never prompt for credentials from the daemon.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RepoInfo identifies the git repository containing a path.
type RepoInfo struct {
	Root   string // worktree root of the main checkout
	GitDir string // common .git directory
	Remote string // origin URL, "" when none
	Head   string // current branch or commit
}

// InspectRepo resolves the repository at path. A path inside one of our
// own worktrees resolves to the main repository.
func InspectRepo(ctx context.Context, path string) (RepoInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return RepoInfo{}, err
	}
	common, err := git(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return RepoInfo{}, fmt.Errorf("%s is not inside a git repository: %w", abs, err)
	}
	common, _ = filepath.EvalSymlinks(common)
	root := filepath.Dir(common)
	if filepath.Base(common) != ".git" {
		// A bare repository: sessions need a checkout to branch from.
		return RepoInfo{}, fmt.Errorf("%s is a bare repository", common)
	}
	info := RepoInfo{Root: root, GitDir: common}
	info.Remote, _ = git(ctx, root, "remote", "get-url", "origin")
	if head, err := git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && head != "HEAD" {
		info.Head = head
	} else {
		info.Head, _ = git(ctx, root, "rev-parse", "HEAD")
	}
	return info, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug makes a branch/directory-safe name.
func Slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// branchExists reports whether a local branch exists.
func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// addWorktree creates path on branch, creating the branch from base when
// it does not exist yet.
func addWorktree(ctx context.Context, repo, path, branch, base string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if branchExists(ctx, repo, branch) {
		_, err := git(ctx, repo, "worktree", "add", path, branch)
		return err
	}
	args := []string{"worktree", "add", "-b", branch, path}
	if base != "" {
		args = append(args, base)
	}
	_, err := git(ctx, repo, args...)
	return err
}

// removeWorktree removes a worktree (forcing past local changes, since the
// caller confirmed) and prunes stale entries. The branch is kept: it holds
// the session's commits.
func removeWorktree(ctx context.Context, repo, path string) error {
	if _, err := os.Stat(path); err == nil {
		if _, err := git(ctx, repo, "worktree", "remove", "--force", path); err != nil {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				return err
			}
		}
	}
	_, err := git(ctx, repo, "worktree", "prune")
	return err
}

// repoKey names a cached repository directory.
func repoKey(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	base := Slug(filepath.Base(strings.TrimSuffix(ref, ".git")))
	if base == "" {
		base = "repo"
	}
	return base + "-" + hex.EncodeToString(sum[:6])
}
