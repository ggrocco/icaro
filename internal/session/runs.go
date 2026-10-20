package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"icaro/internal/store"
)

// The run-session half of the manager (ADR 0010): a workflow run owns one
// session whose workspace its image steps share.

// StartRun creates the run session, or returns it after a restart.
func (m *Manager) StartRun(ctx context.Context, runID, profile string) (RunWorkspace, error) {
	if row, err := m.o.Store.GetSessionByRun(ctx, runID); err == nil {
		if row.Status != StatusRunning {
			row.Status, row.Error = StatusRunning, ""
			if err := m.o.Store.UpdateSession(ctx, row); err != nil {
				return RunWorkspace{}, err
			}
		}
		if err := os.MkdirAll(row.WorktreePath, 0o700); err != nil {
			return RunWorkspace{}, err
		}
		return RunWorkspace{SessionID: row.ID, Dir: row.WorktreePath}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return RunWorkspace{}, err
	}
	if profile == "" {
		profile = m.o.Config.Workflows.Profile
	}
	if _, ok := m.o.Config.Profiles[profile]; !ok {
		return RunWorkspace{}, invalid("unknown profile %q", profile)
	}
	dir := filepath.Join(m.layout.Runs(), runID, "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return RunWorkspace{}, err
	}
	m.chownForContainer(dir)
	row := &store.SessionRow{Kind: KindRun, Profile: profile, RunID: runID, WorktreePath: dir, Status: StatusRunning}
	if err := m.o.Store.CreateSession(ctx, row); err != nil {
		return RunWorkspace{}, err
	}
	return RunWorkspace{SessionID: row.ID, Dir: dir}, nil
}

// RepoWorktree resolves ref@revision once per run: the repository is
// cloned into the cache (or fetched once for this run) and checked out as
// a detached worktree that the run's steps share.
func (m *Manager) RepoWorktree(ctx context.Context, sessionID, ref, revision string) (string, error) {
	if ref == "" {
		return "", invalid("repository ref is empty")
	}
	rev := revision
	if rev == "" {
		rev = "HEAD"
	}
	select {
	case m.gitLock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-m.gitLock }()

	if r, err := m.o.Store.GetSessionRepo(ctx, sessionID, ref, rev); err == nil {
		if _, err := os.Stat(r.Path); err == nil {
			return r.Path, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	cache := filepath.Join(m.layout.Repos(), repoKey(ref)+".git")
	known, err := m.o.Store.ListSessionRepos(ctx, sessionID)
	if err != nil {
		return "", err
	}
	fetched := false
	for _, k := range known {
		if k.Ref == ref {
			fetched = true
		}
	}
	if _, err := os.Stat(cache); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(m.layout.Repos(), 0o700); err != nil {
			return "", err
		}
		if _, err := git(ctx, m.layout.Repos(), "clone", "--mirror", "--quiet", ref, cache); err != nil {
			return "", err
		}
	} else if !fetched {
		if _, err := git(ctx, cache, "remote", "update", "--prune"); err != nil {
			return "", err
		}
	}
	if err := m.o.Store.TouchRepository(ctx, ref, cache); err != nil {
		return "", err
	}
	commit, err := git(ctx, cache, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", invalid("revision %q not found in %s", rev, ref)
	}
	target := filepath.Join(filepath.Dir(row.WorktreePath), "repos", repoKey(ref)+"-"+repoKey(rev)[len(repoKey(rev))-12:])
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	_ = os.RemoveAll(target)
	if _, err := git(ctx, cache, "worktree", "add", "--detach", target, commit); err != nil {
		return "", err
	}
	m.chownForContainer(target)
	if err := m.o.Store.AddSessionRepo(ctx, &store.SessionRepo{SessionID: sessionID, Ref: ref, Revision: rev, Path: target}); err != nil {
		return "", err
	}
	return target, nil
}

// SetContainer records the step container currently running.
func (m *Manager) SetContainer(ctx context.Context, sessionID, containerID string) error {
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	row.ContainerID = containerID
	return m.o.Store.UpdateSession(ctx, row)
}

// FinishRun stops the run session and, unless keep, removes its
// worktrees and scratch workspace.
func (m *Manager) FinishRun(ctx context.Context, sessionID string, keep bool) error {
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if row.Kind != KindRun {
		return fmt.Errorf("session %s is not a run session", sessionID)
	}
	if !keep {
		repos, err := m.o.Store.ListSessionRepos(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, r := range repos {
			cache := filepath.Join(m.layout.Repos(), repoKey(r.Ref)+".git")
			if err := removeWorktree(ctx, cache, r.Path); err != nil {
				m.o.Log.Warn("remove run worktree", "path", r.Path, "err", err)
			}
		}
		if err := os.RemoveAll(filepath.Join(m.layout.Runs(), row.RunID)); err != nil {
			return err
		}
	}
	row.Status, row.ContainerID = StatusStopped, ""
	return m.o.Store.UpdateSession(ctx, row)
}

var _ RunSessions = (*Manager)(nil)
