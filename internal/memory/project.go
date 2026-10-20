package memory

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"icaro/internal/session"
)

// ProjectIDForPath returns the stable project id for a directory: the id
// of the git repository that contains it (its main worktree, so every
// worktree of one repository shares a project), or of the directory
// itself when it is not in a repository. The id is "p_" plus 16 hex chars
// of the SHA-256 of the cleaned absolute, symlink-resolved path.
//
// Session code must derive project ids with this function so memory scope
// lines up with sessions (see docs/memory.md).
func ProjectIDForPath(path string) (string, error) {
	root, err := ProjectRoot(path)
	if err != nil {
		return "", err
	}
	return ProjectIDForRoot(root), nil
}

// ProjectIDForRoot hashes an already-resolved project root.
func ProjectIDForRoot(root string) string { return session.ProjectIDForRoot(root) }

// ProjectRoot resolves the directory a project id is derived from.
func ProjectRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// --git-common-dir is shared by all worktrees of a repository.
	out, err := git(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return abs, nil
	}
	common := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	// A bare repository or a separate git dir: the git dir names the project.
	return common, nil
}

// GitTopLevel returns the worktree root containing path, or path itself.
func GitTopLevel(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		abs, _ := filepath.Abs(path)
		return abs
	}
	return strings.TrimSpace(string(out))
}

// GitBranch returns the current branch of the worktree at path, or "".
func GitBranch(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return ""
	}
	return b
}

// git runs the git binary in dir. Arguments are passed as argv (no shell),
// and dir is only ever a directory the caller asked to index.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // fixed binary, argv without a shell
}
