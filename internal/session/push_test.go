package session

import (
	"context"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// httpRemote serves a bare repository over git's smart HTTP protocol
// (git http-backend behind net/http/cgi) with anonymous push enabled, so
// pushes go through a real transport that the hardened options allow.
func httpRemote(t *testing.T) (url, bare string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	ctx := context.Background()
	if _, err := git(ctx, root, "init", "-q", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, bare, "config", "http.receivepack", "true"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv.URL + "/remote.git", bare
}

func remoteRef(t *testing.T, bare, ref string) string {
	t.Helper()
	out, _ := git(context.Background(), bare, "rev-parse", "--verify", "--quiet", ref)
	return out
}

func TestPushSessionBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	url, bare := httpRemote(t)
	if _, err := git(ctx, e.repo, "remote", "add", "origin", url); err != nil {
		t.Fatal(err)
	}
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "feature"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("%+v %v", s, err)
	}
	// The agent commits in its worktree and tags the commit; the user's
	// config asks git to push tags along.
	if _, err := git(ctx, s.WorktreePath, "commit", "-q", "--allow-empty", "-m", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, s.WorktreePath, "tag", "-a", "-m", "t", "agent-tag"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, e.repo, "config", "push.followTags", "true"); err != nil {
		t.Fatal(err)
	}
	head, _ := git(ctx, s.WorktreePath, "rev-parse", "HEAD")

	res, err := e.m.Push(ctx, s.ID, PushRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Remote != "origin" || res.Branch != "icaro/feature" || res.Commit != head || res.Forced {
		t.Fatalf("%+v", res)
	}
	if got := remoteRef(t, bare, "refs/heads/icaro/feature"); got != head {
		t.Fatalf("remote branch at %q, want %s", got, head)
	}
	if got := remoteRef(t, bare, "refs/tags/agent-tag"); got != "" {
		t.Fatal("a tag was pushed along with the branch")
	}
	if got := remoteRef(t, bare, "refs/heads/main"); got != "" {
		t.Fatal("another branch was pushed")
	}

	// History rewritten by the agent: a plain push is rejected, never forced.
	if _, err := git(ctx, s.WorktreePath, "commit", "-q", "--amend", "--allow-empty", "-m", "rewritten"); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := git(ctx, s.WorktreePath, "rev-parse", "HEAD")
	var pe *ErrPush
	if _, err := e.m.Push(ctx, s.ID, PushRequest{}); !errors.As(err, &pe) {
		t.Fatalf("non-fast-forward push must fail: %v", err)
	}
	// With a lease but no remote-tracking ref, the branch must not exist
	// on the remote yet, so this fails too.
	if _, err := git(ctx, e.repo, "update-ref", "-d", "refs/remotes/origin/icaro/feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Push(ctx, s.ID, PushRequest{ForceWithLease: true}); !errors.As(err, &pe) {
		t.Fatalf("a lease without a tracking ref must expect no remote branch: %v", err)
	}
	if got := remoteRef(t, bare, "refs/heads/icaro/feature"); got != head {
		t.Fatal("a refused push changed the remote")
	}
	// After a fetch the lease matches and the rewrite goes through.
	if _, err := git(ctx, e.repo, "fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	res, err = e.m.Push(ctx, s.ID, PushRequest{Remote: "origin", ForceWithLease: true})
	if err != nil || !res.Forced || res.Commit != rewritten {
		t.Fatalf("%+v %v", res, err)
	}
	if got := remoteRef(t, bare, "refs/heads/icaro/feature"); got != rewritten {
		t.Fatalf("remote branch at %q, want %s", got, rewritten)
	}
}

func TestPushRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	url, _ := httpRemote(t)
	local := t.TempDir()
	if _, err := git(ctx, local, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"remote", "add", "origin", url},
		{"remote", "add", "local", local},
		{"remote", "add", "--mirror=push", "mirror", url},
	} {
		if _, err := git(ctx, e.repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for name, remote := range map[string]string{
		"url":     "https://example.com/r.git",
		"path":    "/tmp/r.git",
		"option":  "--upload-pack=x",
		"unknown": "upstream",
		"mirror":  "mirror",
	} {
		if _, err := e.m.Push(ctx, s.ID, PushRequest{Remote: remote}); !isInvalid(err) {
			t.Fatalf("%s: remote %q must be refused: %v", name, remote, err)
		}
	}
	// A local-path remote is configured by the user, but host git never
	// uses the file transport on a session's behalf.
	var pe *ErrPush
	if _, err := e.m.Push(ctx, s.ID, PushRequest{Remote: "local"}); !errors.As(err, &pe) || !strings.Contains(pe.Msg, "file") {
		t.Fatalf("the file transport must be refused: %v", err)
	}
	if _, err := e.m.Push(ctx, "nope", PushRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ws, err := e.m.StartRun(ctx, "run-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Push(ctx, ws.SessionID, PushRequest{}); !isInvalid(err) {
		t.Fatalf("a run session has no branch: %v", err)
	}
	if _, err := git(ctx, e.repo, "update-ref", "-d", "refs/heads/icaro/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Push(ctx, s.ID, PushRequest{}); !isInvalid(err) {
		t.Fatalf("a missing branch must be refused: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.m.Push(cctx, s.ID, PushRequest{}); err == nil {
		t.Fatal("a cancelled push must fail")
	}
}
