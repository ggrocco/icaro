package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// sseWriter hands each write to the test over a channel and fails every
// write after the first failAfter (never, when failAfter < 0). Only the
// handler goroutine touches its fields.
type sseWriter struct {
	hdr       http.Header
	writes    chan string
	n         int
	failAfter int
}

func newSSEWriter(failAfter int) *sseWriter {
	return &sseWriter{hdr: http.Header{}, writes: make(chan string, 64), failAfter: failAfter}
}

func (w *sseWriter) Header() http.Header { return w.hdr }
func (w *sseWriter) WriteHeader(int)     {}
func (w *sseWriter) FlushError() error   { return nil }

func (w *sseWriter) Write(p []byte) (int, error) {
	w.n++
	if w.failAfter >= 0 && w.n > w.failAfter {
		return 0, errors.New("client gone")
	}
	w.writes <- string(p)
	return len(p), nil
}

// next returns the next write, failing the test after a timeout.
func (w *sseWriter) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-w.writes:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no write")
		return ""
	}
}

type stream struct {
	w      *sseWriter
	cancel context.CancelFunc
	done   chan struct{}
}

// open calls the events handler directly, so a test controls the writer.
func openStream(s *Server, runID, query string, w *sseWriter) *stream {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/events"+query, nil).WithContext(ctx)
	r.SetPathValue("id", runID)
	st := &stream{w: w, cancel: cancel, done: make(chan struct{})}
	go func() { s.handleRunEvents(w, r); close(st.done) }()
	return st
}

func (st *stream) wait(t *testing.T) {
	t.Helper()
	select {
	case <-st.done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
}

// queuedRun applies the hello workflow and starts a run that stays queued
// (no engine runs in these tests).
func queuedRun(t *testing.T, f *full) string {
	t.Helper()
	if code, body := f.call("POST", "/api/v1/workflows", wfYAML); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := f.call("POST", "/api/v1/workflows/hello/runs", map[string]any{"trigger": map[string]any{"who": "x"}})
	var run service.RunInfo
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &run) != nil {
		t.Fatalf("run: %d %s", code, body)
	}
	return run.ID
}

func TestRunEventsLiveStream(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	id := queuedRun(t, f)
	st := openStream(f.srv, id, "", newSSEWriter(-1))
	defer st.cancel()
	if got := st.w.next(t); !strings.HasPrefix(got, "event: snapshot\n") {
		t.Fatalf("first event: %q", got)
	}
	if ct := st.w.hdr.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	// Without ?logs=1 log lines are not forwarded.
	f.bus.Publish(events.Event{Type: events.TypeLog, RunID: id, Step: "say", Attempt: 1, Line: "hidden", Offset: 7})
	f.bus.Publish(events.Event{Type: events.TypeStep, RunID: id, Step: "say", Status: "running"})
	if got := st.w.next(t); !strings.HasPrefix(got, "event: step\n") || strings.Contains(got, "hidden") {
		t.Fatalf("step event: %q", got)
	}
	// A non-terminal run event re-sends the snapshot.
	f.bus.Publish(events.Event{Type: events.TypeRun, RunID: id, Status: store.RunRunning})
	if got := st.w.next(t); !strings.HasPrefix(got, "event: snapshot\n") {
		t.Fatalf("snapshot: %q", got)
	}
	// The client going away ends the stream.
	st.cancel()
	st.wait(t)
}

func TestRunEventsStopOnWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		failAfter int
		publish   *events.Event
	}{
		{name: "snapshot", failAfter: 0},
		{name: "step", failAfter: 1, publish: &events.Event{Type: events.TypeStep, Step: "say", Status: "running"}},
		{name: "run snapshot", failAfter: 1, publish: &events.Event{Type: events.TypeRun, Status: store.RunRunning}},
		{name: "log", query: "?logs=1", failAfter: 1, publish: &events.Event{Type: events.TypeLog, Step: "say", Attempt: 1, Line: "l", Offset: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFull(t)
			id := queuedRun(t, f)
			st := openStream(f.srv, id, tc.query, newSSEWriter(tc.failAfter))
			defer st.cancel()
			if tc.publish != nil {
				st.w.next(t) // the snapshot: the subscription is live
				ev := *tc.publish
				ev.RunID = id
				f.bus.Publish(ev)
			}
			st.wait(t)
		})
	}
}

// Not parallel: it changes the package-level keep-alive interval.
func TestRunEventsKeepAlive(t *testing.T) {
	old := sseKeepAlive
	sseKeepAlive = 10 * time.Millisecond
	t.Cleanup(func() { sseKeepAlive = old })
	f := newFull(t)
	id := queuedRun(t, f)
	// The first keep-alive goes out; the second write fails and ends it.
	st := openStream(f.srv, id, "", newSSEWriter(2))
	defer st.cancel()
	st.w.next(t)
	if got := st.w.next(t); got != ": keep-alive\n\n" {
		t.Fatalf("keep-alive: %q", got)
	}
	st.wait(t)
}

func TestRunEventsRunLookupFailsMidStream(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	id := queuedRun(t, f)
	st := openStream(f.srv, id, "", newSSEWriter(-1))
	defer st.cancel()
	st.w.next(t)
	_ = f.st.Close()
	f.bus.Publish(events.Event{Type: events.TypeRun, RunID: id, Status: store.RunRunning})
	st.wait(t)
}

func TestRunEventsResyncWhenTheBusDrops(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	id := queuedRun(t, f)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	bus := events.New(dead)
	key, _ := crypto.NewKey()
	svc := service.New(service.Options{Store: f.st, Key: key, LogsDir: t.TempDir(), Log: quietLog(), Events: bus})
	st := openStream(New(svc, quietLog()), id, "", newSSEWriter(-1))
	defer st.cancel()
	st.w.next(t)
	if got := st.w.next(t); !strings.HasPrefix(got, "event: resync\n") {
		t.Fatalf("resync: %q", got)
	}
	st.wait(t)
}

func TestRunEventsWithoutABus(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	id := queuedRun(t, f)
	key, _ := crypto.NewKey()
	svc := service.New(service.Options{Store: f.st, Key: key, LogsDir: t.TempDir(), Log: quietLog()})
	c := serveUnix(t, New(svc, quietLog()).Handler())
	code, body := doReq(t, c, "GET", "http://icaro/api/v1/runs/"+id+"/events", nil, nil)
	if code != 500 || errBody(t, body).Error != "internal error" {
		t.Fatalf("%d %s", code, body)
	}
}

// TestRunEventsBacklog covers the ?logs=1 backlog: unreadable logs are
// skipped, and a client that goes away mid-backlog ends the stream.
func TestRunEventsBacklog(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	ctx := context.Background()
	id := queuedRun(t, f)
	row, err := f.st.GetStepRun(ctx, id, "say")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "say.1.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	row.Status, row.Attempt, row.LogPath = store.StepRunning, 1, logPath
	if err := f.st.UpdateStepRun(ctx, row); err != nil {
		t.Fatal(err)
	}

	st := openStream(f.srv, id, "?logs=1", newSSEWriter(2))
	defer st.cancel()
	st.w.next(t)
	if got := st.w.next(t); !strings.Contains(got, `"line":"one"`) {
		t.Fatalf("backlog: %q", got)
	}
	st.wait(t) // the second backlog line fails to write

	// A log path that cannot be read as a file is skipped.
	row.LogPath = t.TempDir()
	if err := f.st.UpdateStepRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	st = openStream(f.srv, id, "?logs=1", newSSEWriter(-1))
	defer st.cancel()
	st.w.next(t)
	f.bus.Publish(events.Event{Type: events.TypeStep, RunID: id, Step: "say", Status: "running"})
	if got := st.w.next(t); !strings.HasPrefix(got, "event: step\n") {
		t.Fatalf("after skipped backlog: %q", got)
	}
}
