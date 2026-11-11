package assist

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/session"
	"icaro/internal/store"
)

func TestStoreErrors(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{YAML: helloYAML}, "local")
	_ = e.st.Close()
	ctx := e.ctx
	if _, err := e.s.CreateChat(ctx, CreateRequest{}, "local"); err == nil {
		t.Error("create")
	}
	if _, err := e.s.ListChats(ctx); err == nil {
		t.Error("list")
	}
	if _, err := e.s.GetChat(ctx, d.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("get")
	}
	if _, err := e.s.Send(ctx, d.ID, "hi", "local"); err == nil {
		t.Error("send")
	}
	if _, err := e.s.Retry(ctx, d.ID, "m", "local"); err == nil {
		t.Error("retry")
	}
	if _, err := e.s.PutDraft(ctx, d.ID, "x"); err == nil {
		t.Error("draft")
	}
	if _, err := e.s.Decide(ctx, d.ID, "a", true, "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("decide")
	}
	if err := e.s.Delete(ctx, d.ID); err == nil {
		t.Error("delete")
	}
	if _, _, err := e.s.Entries(ctx, d.ID, 0); err == nil {
		t.Error("entries")
	}
}

func TestLogErrors(t *testing.T) {
	e := newEnv(t)
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	e.s.o.LogsDir = blocked
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0, `{"type":"result","is_error":false}`))
	m, err := e.s.Send(e.ctx, d.ID, "hi", "local")
	if err != nil {
		t.Fatal("a chat without a log must still work:", err)
	}
	e.waitFor("the turn without a log", func() bool {
		got, _ := e.st.GetChatMessage(e.ctx, m[1].ID)
		return got.Status == store.TurnDone
	})
	if _, _, err := e.s.Entries(e.ctx, d.ID, 0); err == nil {
		t.Error("entries under a file")
	}

	// A log the daemon cannot open (a directory in its place).
	e2 := newEnv(t)
	d2, _ := e2.s.CreateChat(e2.ctx, CreateRequest{}, "local")
	_ = os.MkdirAll(e2.s.logPath(d2.ID), 0o700)
	e2.s.actor(d2.ID).append(entry{Type: EntryStatus})
	if _, _, err := e2.s.Entries(e2.ctx, d2.ID, 0); err == nil {
		t.Error("entries of a directory")
	}

	// A partial last line is not returned until it is complete.
	e3 := newEnv(t)
	d3, _ := e3.s.CreateChat(e3.ctx, CreateRequest{}, "local")
	_ = os.MkdirAll(e3.s.o.LogsDir, 0o700)
	_ = os.WriteFile(e3.s.logPath(d3.ID), []byte("{\"type\":\"a\"}\nnot json\n{\"type\":"), 0o600)
	raw, next, err := e3.s.Entries(e3.ctx, d3.ID, 0)
	if err != nil || len(raw) != 1 || next != int64(len("{\"type\":\"a\"}\nnot json\n")) {
		t.Fatalf("%d entries, next %d, %v", len(raw), next, err)
	}
	if raw, _, err := e3.s.Entries(e3.ctx, d3.ID, 1<<20); err != nil || len(raw) != 0 {
		t.Fatal(raw, err)
	}
}

func TestExitGrace(t *testing.T) {
	saved := exitGrace
	exitGrace = 20 * time.Millisecond
	t.Cleanup(func() { exitGrace = saved })
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(func(ctx context.Context, _ session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		_, _ = io.WriteString(out, `{"type":"result","is_error":false}`+"\n")
		<-ctx.Done() // the CLI hangs after its last event
		return session.TurnResult{}, ctx.Err()
	})
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnDone {
		t.Fatalf("%+v", got)
	}
	e.waitFor("the hung process to be signalled", func() bool {
		n := 0
		e.sess.do(func(st *sessState) { n = len(st.cancels) })
		return n == 1
	})
}

func TestCancelWhileQueued(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.do(func(st *sessState) { st.queuedPolls = 1 << 30 })
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	e.waitFor("the queued note", func() bool {
		for _, en := range e.entries(d.ID) {
			if strings.Contains(en.Note, "free assistant slot") {
				return true
			}
		}
		return false
	})
	if err := e.s.Cancel(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnCancelled {
		t.Fatalf("%+v", got)
	}
}

func TestSessionErrors(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	e.sess.push(lines(0, `{"type":"result","is_error":false}`))
	m, _ := e.s.Send(e.ctx, d.ID, "hi", "local")
	e.waitMessage(d.ID, m[1].ID)

	e.sess.do(func(st *sessState) { st.getErr = errors.New("store busy") })
	m, _ = e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "store busy") {
		t.Fatalf("%+v", got)
	}
	// Get failing while the session is queued.
	e.sess.do(func(st *sessState) { st.getErr = nil })
	c, _ := e.st.GetChat(e.ctx, d.ID)
	e.sess.do(func(st *sessState) { st.sessions[c.SessionID].Status = session.StatusQueued; st.queuedPolls = 1 })
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.sess.do(func(st *sessState) { st.getErr = errors.New("lost") })
	}()
	m, _ = e.s.Send(e.ctx, d.ID, "hi", "local")
	if got := e.waitMessage(d.ID, m[1].ID); got.Status != store.TurnError {
		t.Fatalf("%+v", got)
	}
	e.sess.do(func(st *sessState) { st.getErr = nil })

	// Stopping an idle container can fail; the chat goes on.
	e.cfg.Assistant.IdleTimeout = 10 * time.Millisecond
	e.sess.do(func(st *sessState) { st.stopErr = errors.New("docker down") })
	e.sess.push(lines(0, `{"type":"result","is_error":false}`))
	m, _ = e.s.Send(e.ctx, d.ID, "hi", "local")
	e.waitMessage(d.ID, m[1].ID)
	time.Sleep(50 * time.Millisecond)

	e.sess.do(func(st *sessState) { st.removeErr = errors.New("busy") })
	if err := e.s.Delete(e.ctx, d.ID); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("delete: %v", err)
	}
}

func TestIdleWithoutSession(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	a := e.s.actor(d.ID)
	a.idleExpired() // no session yet: nothing to stop
	var stops int
	e.sess.do(func(st *sessState) { stops = len(st.stops) })
	if stops != 0 {
		t.Fatal(stops)
	}
	a.shutdown()
	a.idleExpired() // after shutdown: a no-op
	if a.do(func(*actorState) {}) {
		t.Fatal("a shut-down actor ran a closure")
	}
}

func TestMoreEdges(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.ApplyWorkflow(e.ctx, helloYAML, "local"); err != nil {
		t.Fatal(err)
	}
	// A chat about a saved workflow starts with its YAML as the draft.
	d, err := e.s.CreateChat(e.ctx, CreateRequest{Workflow: "hello"}, "local")
	if err != nil || d.Draft == nil || d.Draft.YAML != helloYAML || d.Workflow != "hello" {
		t.Fatal(d, err)
	}
	_, sid := e.chatWithSession("")
	if out, isErr := e.tool(sid, ToolRun, map[string]any{"name": 7}); !isErr {
		t.Fatalf("bad arguments accepted: %+v", out)
	}

	// A queued session that ends up stopped fails the turn and says so.
	e.sess.do(func(st *sessState) { st.queuedPolls, st.stopQueued = 1, true })
	d2, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	m, _ := e.s.Send(e.ctx, d2.ID, "hi", "local")
	if got := e.waitMessage(d2.ID, m[1].ID); got.Status != store.TurnError || !strings.Contains(got.Error, "it is stopped") {
		t.Fatalf("%+v", got)
	}

	// Recovery against a broken store logs and carries on.
	_ = e.st.Close()
	s2 := New(Options{Store: e.st, Config: e.cfg, Sessions: e.sess, Workflows: e.svc, LogsDir: t.TempDir(), Log: e.s.o.Log})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	s2.killLeftovers(context.Background())
}

// A queued turn whose message was settled meanwhile (cancelled by another
// writer) is skipped, both before and after its session is ready.
func TestSettledBeforeStart(t *testing.T) {
	e := newEnv(t)
	d, _ := e.s.CreateChat(e.ctx, CreateRequest{}, "local")
	gate := make(chan struct{})
	started := make(chan struct{})
	e.sess.push(func(ctx context.Context, req session.TurnRequest, out io.Writer) (session.TurnResult, error) {
		close(started)
		<-gate
		return lines(0, `{"type":"result","is_error":false}`)(ctx, req, out)
	})
	a, _ := e.s.Send(e.ctx, d.ID, "one", "local")
	b, _ := e.s.Send(e.ctx, d.ID, "two", "local")
	<-started
	settle := func(id string) {
		t.Helper()
		m, _ := e.st.GetChatMessage(e.ctx, id)
		m.Status = store.TurnCancelled
		if ok, err := e.st.UpdateChatMessageIf(e.ctx, m, store.TurnQueued); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	settle(b[1].ID)
	close(gate)
	e.waitMessage(d.ID, a[1].ID)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.sess.turns()); n != 1 {
		t.Fatalf("the settled turn ran: %d execs", n)
	}

	// Settled while its session was still starting.
	c, _ := e.st.GetChat(e.ctx, d.ID)
	e.sess.do(func(st *sessState) { st.sessions[c.SessionID].Status = session.StatusQueued; st.queuedPolls = 1 << 30 })
	m, _ := e.s.Send(e.ctx, d.ID, "three", "local")
	e.waitFor("the queued note", func() bool {
		for _, en := range e.entries(d.ID) {
			if en.MessageID == m[1].ID && strings.Contains(en.Note, "free assistant slot") {
				return true
			}
		}
		return false
	})
	settle(m[1].ID)
	e.sess.do(func(st *sessState) { st.queuedPolls = 0 })
	time.Sleep(50 * time.Millisecond)
	if n := len(e.sess.turns()); n != 1 {
		t.Fatalf("the settled turn ran: %d execs", n)
	}
}
