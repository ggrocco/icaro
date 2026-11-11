package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/store"
)

// Execer runs a command inside a running container (docker exec).
type Execer interface {
	Exec(ctx context.Context, containerID string, o docker.ExecOptions, stdout, stderr io.Writer) (int, error)
}

// AssistRequest describes the assist session of one chat.
type AssistRequest struct {
	ChatID string
	// Profile defaults to assistant.profile (or the first profile with a
	// chat-capable agent).
	Profile string
	// ProjectID optionally scopes the assistant's memory reads.
	ProjectID string
}

// CreateAssist makes the assist session of a chat: a scratch workspace
// under ~/.icaro/assist/<chat>/workspace and an idle container. It starts
// at once below assistant.max_active, otherwise it waits as queued.
func (m *Manager) CreateAssist(ctx context.Context, req AssistRequest) (*Session, error) {
	if req.ChatID == "" || strings.ContainsAny(req.ChatID, `/\.`) {
		return nil, invalid("an assist session needs a chat id")
	}
	profile := req.Profile
	if profile == "" {
		profile = m.o.Config.AssistantProfile()
	}
	prof, ok := m.o.Config.Profiles[profile]
	if !ok {
		if profile == "" {
			return nil, invalid("no profile runs claude or codex; set assistant.profile")
		}
		return nil, invalid("unknown profile %q", profile)
	}
	if agent, err := creds.LookupAgent(prof.Agent); err != nil || !agent.CanChat() {
		return nil, invalid("profile %q cannot run the assistant: %v", profile, creds.ErrNoChat)
	}
	if req.ProjectID != "" {
		if _, err := m.o.Store.GetProject(ctx, req.ProjectID); err != nil {
			return nil, invalid("unknown project %q", req.ProjectID)
		}
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	err := m.o.Engine.Ping(pctx)
	cancel()
	if err != nil {
		return nil, &ErrUnavailable{Msg: "docker is not reachable, so the assistant cannot start: " + err.Error()}
	}
	dir := filepath.Join(m.layout.Assist(), req.ChatID, "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m.chownForContainer(filepath.Dir(dir))
	row := &store.SessionRow{ID: store.NewID(), Kind: KindAssist, ProjectID: req.ProjectID, Profile: profile, WorktreePath: dir, Status: StatusQueued}
	if err := m.o.Store.CreateSession(ctx, row); err != nil {
		return nil, err
	}
	if err := m.requestStart(ctx); err != nil {
		return nil, err
	}
	settled, err := m.waitSettled(ctx, row.ID, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	return toSession(settled), nil
}

// TurnRequest is one assistant turn: the agent CLI command, its extra
// environment, and the user's message on stdin.
type TurnRequest struct {
	Cmd   []string
	Env   []string
	Stdin []byte
}

// TurnResult is how a turn's process ended.
type TurnResult struct {
	ExitCode int
	// Stderr is the tail of the process's standard error.
	Stderr string
}

// turnPIDFile holds the running turn's pid inside the container, so
// CancelTurn can signal it (Docker cannot signal an exec).
const turnPIDFile = container.HomePath + "/.icaro-turn.pid"

// turnWrapper records the shell's pid, then execs the command in its
// place, so the pid is the agent CLI's.
var turnWrapper = []string{"sh", "-c", `echo $$ > ` + turnPIDFile + ` && exec "$@"`, "icaro-turn"}

// stderrTail is how much of a turn's stderr is kept.
const stderrTail = 4 << 10

// ErrNoExec reports a manager without an Execer.
var ErrNoExec = errors.New("this daemon cannot run commands in containers")

// RunTurn runs one turn in a running assist session, streaming the
// command's stdout to out. Cancelling ctx stops reading; call CancelTurn
// to stop the process.
func (m *Manager) RunTurn(ctx context.Context, sessionID string, req TurnRequest, out io.Writer) (TurnResult, error) {
	row, err := m.assistRow(ctx, sessionID)
	if err != nil {
		return TurnResult{}, err
	}
	if len(req.Cmd) == 0 {
		return TurnResult{}, invalid("a turn needs a command")
	}
	var errBuf tail
	code, err := m.o.Execer.Exec(ctx, row.ContainerID, docker.ExecOptions{
		Cmd: append(append([]string{}, turnWrapper...), req.Cmd...), User: m.User(), Workdir: container.WorkspacePath,
		Env: req.Env, Stdin: req.Stdin,
	}, out, &errBuf)
	_ = m.o.Store.HeartbeatSession(context.WithoutCancel(ctx), row.ID)
	return TurnResult{ExitCode: code, Stderr: errBuf.String()}, err
}

// CancelTurn signals the turn running in an assist session, if any.
func (m *Manager) CancelTurn(ctx context.Context, sessionID string) error {
	row, err := m.assistRow(ctx, sessionID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	script := `p=$(cat ` + turnPIDFile + ` 2>/dev/null) || exit 0; kill -TERM "$p" 2>/dev/null || exit 0; ` +
		`i=0; while kill -0 "$p" 2>/dev/null && [ $i -lt 5 ]; do sleep 1; i=$((i+1)); done; kill -KILL "$p" 2>/dev/null; exit 0`
	_, err = m.o.Execer.Exec(cctx, row.ContainerID, docker.ExecOptions{Cmd: []string{"sh", "-c", script}, User: m.User()}, io.Discard, io.Discard)
	return err
}

func (m *Manager) assistRow(ctx context.Context, sessionID string) (*store.SessionRow, error) {
	if m.o.Execer == nil {
		return nil, ErrNoExec
	}
	row, err := m.o.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Kind != KindAssist {
		return nil, invalid("session %s is not an assistant session", sessionID)
	}
	if row.Status != StatusRunning || row.ContainerID == "" {
		return nil, invalid("session %s is %s, not running", sessionID, row.Status)
	}
	return row, nil
}

// tail keeps the last stderrTail bytes written to it. One exec goroutine
// writes and the caller reads after the exec returns.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > stderrTail {
		t.b = t.b[len(t.b)-stderrTail:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.b)) }
