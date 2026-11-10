package assist

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/crypto"
	"icaro/internal/mcp"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

// turnScript answers one RunTurn: it writes a stream and returns a result.
type turnScript func(ctx context.Context, req session.TurnRequest, out io.Writer) (session.TurnResult, error)

// fakeSessions stands in for the session manager. Its state is owned by
// one goroutine, like the code under test.
type fakeSessions struct {
	ops chan func(*sessState)
}

type sessState struct {
	sessions  map[string]*session.Session
	n         int
	turns     []session.TurnRequest
	cancels   []string
	stops     []string
	removed   []string
	script    []turnScript
	createErr error
	startErr  error
	// queuedPolls makes a created session report queued for that many
	// Gets before it runs.
	queuedPolls int
	failStart   bool
	stopQueued  bool
	getErr      error
	stopErr     error
	removeErr   error
}

func newFakeSessions() *fakeSessions {
	f := &fakeSessions{ops: make(chan func(*sessState))}
	go func() {
		st := &sessState{sessions: map[string]*session.Session{}}
		for op := range f.ops {
			op(st)
		}
	}()
	return f
}

func (f *fakeSessions) do(fn func(*sessState)) {
	done := make(chan struct{})
	f.ops <- func(st *sessState) { fn(st); close(done) }
	<-done
}

func (f *fakeSessions) CreateAssist(_ context.Context, req session.AssistRequest) (*session.Session, error) {
	var out *session.Session
	var err error
	f.do(func(st *sessState) {
		if err = st.createErr; err != nil {
			return
		}
		st.n++
		s := &session.Session{ID: "sess" + strconv.Itoa(st.n), Kind: session.KindAssist, Profile: req.Profile, ProjectID: req.ProjectID, Status: session.StatusRunning}
		if st.queuedPolls > 0 {
			s.Status = session.StatusQueued
		}
		st.sessions[s.ID] = s
		cp := *s
		out = &cp
	})
	return out, err
}

func (f *fakeSessions) Get(_ context.Context, id string) (*session.Session, error) {
	var out *session.Session
	var err error
	f.do(func(st *sessState) {
		if err = st.getErr; err != nil {
			return
		}
		s, ok := st.sessions[id]
		if !ok {
			return
		}
		if s.Status == session.StatusQueued {
			if st.queuedPolls > 0 {
				st.queuedPolls--
			} else if st.failStart {
				s.Status, s.Error = session.StatusError, "image pull failed"
			} else if st.stopQueued {
				s.Status = session.StatusStopped
			} else {
				s.Status = session.StatusRunning
			}
		}
		cp := *s
		out = &cp
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, session.ErrNotFound
	}
	return out, nil
}

func (f *fakeSessions) Start(_ context.Context, id string) (*session.Session, error) {
	var out *session.Session
	var err error
	f.do(func(st *sessState) {
		if err = st.startErr; err != nil {
			return
		}
		s := st.sessions[id]
		s.Status = session.StatusRunning
		cp := *s
		out = &cp
	})
	return out, err
}

func (f *fakeSessions) Stop(_ context.Context, id string) (*session.Session, error) {
	var out *session.Session
	var err error
	f.do(func(st *sessState) {
		if err = st.stopErr; err != nil {
			return
		}
		st.stops = append(st.stops, id)
		if s := st.sessions[id]; s != nil {
			s.Status = session.StatusStopped
			cp := *s
			out = &cp
		}
	})
	return out, err
}

func (f *fakeSessions) Remove(_ context.Context, id string, _ bool) error {
	var err error
	f.do(func(st *sessState) {
		if err = st.removeErr; err != nil {
			return
		}
		st.removed = append(st.removed, id)
		delete(st.sessions, id)
	})
	return err
}

func (f *fakeSessions) RunTurn(ctx context.Context, id string, req session.TurnRequest, out io.Writer) (session.TurnResult, error) {
	var script turnScript
	f.do(func(st *sessState) {
		st.turns = append(st.turns, req)
		if len(st.script) > 0 {
			script, st.script = st.script[0], st.script[1:]
		}
	})
	if script == nil {
		return session.TurnResult{}, errors.New("no script for this turn")
	}
	return script(ctx, req, out)
}

func (f *fakeSessions) CancelTurn(_ context.Context, id string) error {
	f.do(func(st *sessState) { st.cancels = append(st.cancels, id) })
	return nil
}

func (f *fakeSessions) push(s ...turnScript) {
	f.do(func(st *sessState) { st.script = append(st.script, s...) })
}

func (f *fakeSessions) turns() []session.TurnRequest {
	var out []session.TurnRequest
	f.do(func(st *sessState) { out = append(out, st.turns...) })
	return out
}

// lines streams fixed lines.
func lines(code int, ls ...string) turnScript {
	return func(_ context.Context, _ session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		for _, l := range ls {
			_, _ = io.WriteString(out, l+"\n")
		}
		return session.TurnResult{ExitCode: code}, nil
	}
}

// fixture streams a modeled fixture of the stream package.
func fixture(t *testing.T, agent, name string) turnScript {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("stream", "testdata", agent, "modeled", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return lines(0, strings.Split(strings.TrimSpace(string(b)), "\n")...)
}

// blockUntilCancel streams a start line, then waits for the turn to end.
func blockUntilCancel(started chan<- struct{}) turnScript {
	return func(ctx context.Context, _ session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		_, _ = io.WriteString(out, `{"type":"system","subtype":"init","session_id":"c1"}`+"\n")
		close(started)
		<-ctx.Done()
		return session.TurnResult{ExitCode: -1}, ctx.Err()
	}
}

type env struct {
	t    *testing.T
	ctx  context.Context
	s    *Service
	st   *store.Store
	sess *fakeSessions
	svc  *service.Service
	sec  *secrets.Store
	cfg  *config.Config
	mcp  *mcp.Server
	bus  *events.Bus
}

func newEnv(t *testing.T, mods ...func(*config.Config)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfgFile := filepath.Join(t.TempDir(), "icaro.yaml")
	if err := os.WriteFile(cfgFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = t.TempDir()
	cfg.Assistant.TurnTimeout = 10 * time.Second
	cfg.Assistant.ApprovalTimeout = 5 * time.Second
	for _, m := range mods {
		m(cfg)
	}
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	sec := secrets.New(st, key)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.New(ctx)
	fake := docker.NewFake()
	eng := engine.New(engine.Config{
		Concurrency: 2, StepTimeout: time.Minute, LogsDir: t.TempDir(), RunsDir: t.TempDir(),
		RetryBase: time.Millisecond, PollInterval: 10 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
	}, engine.Deps{Store: st, Docker: fake, Events: bus, Secrets: sec, Log: quiet})
	go func() { _ = eng.Run(ctx) }()
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet, Events: bus})
	svc.SetRunner(eng)
	svc.SetSecrets(sec)
	sess := newFakeSessions()
	s := New(Options{Store: st, Config: cfg, Sessions: sess, Workflows: svc, Secrets: sec, Events: bus, Log: quiet,
		LogsDir: filepath.Join(t.TempDir(), "assist"), Poll: 5 * time.Millisecond})
	go func() { _ = s.Run(ctx) }()
	<-s.started
	e := &env{t: t, ctx: ctx, s: s, st: st, sess: sess, svc: svc, sec: sec, cfg: cfg, bus: bus}
	e.mcp = s.Server("test", nil)
	return e
}

// waitMessage polls until the message reaches a final status.
func (e *env) waitMessage(chatID, msgID string) MessageInfo {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := e.s.GetChat(e.ctx, chatID)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, m := range d.Messages {
			if m.ID == msgID && m.Status != store.TurnQueued && m.Status != store.TurnRunning {
				// The store settles first; the log entry announcing it
				// follows. Wait for both, as a client would see them.
				for _, en := range e.entries(chatID) {
					if en.Type == EntryStatus && en.MessageID == msgID && en.Status == m.Status {
						return m
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("message %s never finished", msgID)
	return MessageInfo{}
}

// waitFor polls cond.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// entries returns the chat's log as decoded entries.
func (e *env) entries(chatID string) []entry {
	e.t.Helper()
	raw, _, err := e.s.Entries(e.ctx, chatID, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]entry, 0, len(raw))
	for _, r := range raw {
		var en entry
		if err := json.Unmarshal(r.Data, &en); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, en)
	}
	return out
}

// tool calls an assistant tool as the chat's session.
func (e *env) tool(sessionID, name string, args map[string]any) (map[string]any, bool) {
	e.t.Helper()
	c := mcp.InProcess(e.mcp, mcp.Caller{SessionID: sessionID, Principal: "assist:" + sessionID, Scope: "read"})
	res, err := c.CallTool(e.ctx, name, args)
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	out := map[string]any{}
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
			e.t.Fatalf("%s: %q: %v", name, res.Content[0].Text, err)
		}
	} else {
		out["error"] = res.Content[0].Text
	}
	return out, res.IsError
}

const helloYAML = `# The smallest useful workflow: one container step that reads its input
# and publishes an output for the next step.
#   icaro workflow apply examples/hello.yaml
#   icaro run hello --set who=icaro --wait
version: 1
name: hello
description: Greet whoever the trigger names
triggers:
  - type: manual
steps:
  - id: greet
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    input:
      who: ${{ trigger.who }}
    command: |
      echo "hello $ICARO_INPUT_WHO"
      echo "input.json: $(cat /icaro/input.json)"
      printf '{"greeting":"hello %s"}' "$ICARO_INPUT_WHO" > /icaro/output.json
    outputs:
      greeting: json:greeting
  - id: shout
    image: alpine:3.24@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
    needs: [greet]
    # Values that come from a trigger reach the shell through env, never
    # spliced into the command text.
    env:
      GREETING: ${{ steps.greet.outputs.greeting }}
    command: echo "$GREETING" | tr a-z A-Z
`
