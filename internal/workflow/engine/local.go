package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"icaro/internal/session"
)

// localSessions is the fallback RunSessions used when the session
// manager is not wired: a scratch workspace per run, no repositories.
type localSessions struct {
	dir string
}

const localPrefix = "local-"

func (l *localSessions) root() string {
	if l.dir == "" {
		return filepath.Join(os.TempDir(), "icaro-runs")
	}
	return l.dir
}

func (l *localSessions) StartRun(_ context.Context, runID, _ string) (session.RunWorkspace, error) {
	dir := filepath.Join(l.root(), runID, "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return session.RunWorkspace{}, err
	}
	return session.RunWorkspace{SessionID: localPrefix + runID, Dir: dir}, nil
}

func (l *localSessions) RepoWorktree(context.Context, string, string, string) (string, error) {
	return "", errors.New("repository steps need the session manager, which is not configured")
}

func (l *localSessions) SetContainer(context.Context, string, string) error { return nil }

func (l *localSessions) FinishRun(_ context.Context, sessionID string, keep bool) error {
	runID, ok := strings.CutPrefix(sessionID, localPrefix)
	if !ok || keep || runID == "" || strings.ContainsAny(runID, `/\.`) {
		return nil
	}
	return os.RemoveAll(filepath.Join(l.root(), runID))
}

var _ session.RunSessions = (*localSessions)(nil)
