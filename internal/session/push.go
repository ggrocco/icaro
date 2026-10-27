package session

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"icaro/internal/logs"
	"icaro/internal/store"
)

// PushRequest asks the daemon to push a session's branch from the host,
// so agent containers never hold git credentials.
type PushRequest struct {
	// Remote is the name of a remote configured in the repository
	// (default origin); URLs and paths are refused.
	Remote string `json:"remote,omitempty"`
	// ForceWithLease overwrites the remote branch only while it still
	// points where the repository's remote-tracking ref says it does. A
	// plain force push is never done.
	ForceWithLease bool `json:"force_with_lease,omitempty"`
}

// PushResult reports a push.
type PushResult struct {
	SessionID string `json:"session_id"`
	Remote    string `json:"remote"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`
	Forced    bool   `json:"forced"`
	Output    string `json:"output,omitempty"`
}

// ErrPush reports a push that git (or the remote) rejected.
type ErrPush struct{ Msg string }

func (e *ErrPush) Error() string { return e.Msg }

var remoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// pushOptions keep a push to exactly the refspec given: no tags that
// happen to point into it (the agent can create tags in the shared .git)
// and no submodules, whatever the user's git config says.
var pushOptions = []string{"-c", "push.followTags=false", "-c", "push.recurseSubmodules=no", "-c", "push.gpgSign=false"}

// Push pushes the session's own branch (icaro/<name>) to a configured
// remote with the host user's git credentials. It runs in the main
// checkout under the hardened git options, pushes the commit the branch
// points at to refs/heads/<branch> and nothing else, and never forces
// without a lease.
func (m *Manager) Push(ctx context.Context, id string, req PushRequest) (*PushResult, error) {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Kind != KindAgent || !strings.HasPrefix(row.Branch, "icaro/") {
		return nil, invalid("session %s has no branch to push (only agent sessions own an icaro/ branch)", id)
	}
	proj, err := m.o.Store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	root := proj.RootPath
	remote := req.Remote
	if remote == "" {
		remote = "origin"
	}
	if !remoteNameRe.MatchString(remote) {
		return nil, invalid("remote %q must be the name of a remote configured in %s, not a URL or path", remote, root)
	}
	names, err := git(ctx, root, "remote")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(strings.Fields(names), remote) {
		return nil, invalid("%s has no remote named %q (git remote add it on the host first)", root, remote)
	}
	if mirror, _ := git(ctx, root, "config", "--type=bool", "--get", "remote."+remote+".mirror"); mirror == "true" {
		return nil, invalid("remote %q is a mirror remote, which pushes every ref; refused", remote)
	}
	ref := "refs/heads/" + row.Branch
	commit, err := git(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || commit == "" {
		return nil, invalid("branch %s does not exist in %s", row.Branch, root)
	}
	args := []string{"push", "--porcelain", "--no-follow-tags", "--no-recurse-submodules", "--no-verify"}
	if req.ForceWithLease {
		// The expected value is the remote-tracking ref; with none, the
		// branch must not exist on the remote yet. The agent can write
		// that ref in the shared .git, so the lease guards against
		// clobbering work nobody fetched, not against the agent; either
		// way only the session's own branch can be overwritten.
		expect, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+row.Branch)
		if err != nil {
			expect = ""
		}
		args = append(args, "--force-with-lease="+ref+":"+expect)
	}
	args = append(args, remote, commit+":"+ref)
	out, err := runGitCombined(ctx, root, append(append([]string{}, hardening...), pushOptions...), args, true)
	out = strings.TrimSpace(logs.RedactString(out, nil))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ErrPush{Msg: fmt.Sprintf("push %s to %s failed: %s", row.Branch, remote, out)}
	}
	m.o.Log.Info("session branch pushed", "session", id, "remote", remote, "branch", row.Branch, "commit", commit, "forced", req.ForceWithLease)
	return &PushResult{SessionID: id, Remote: remote, Branch: row.Branch, Commit: commit, Forced: req.ForceWithLease, Output: out}, nil
}
