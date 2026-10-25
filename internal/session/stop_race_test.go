package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/store"
)

// gate pauses one goroutine at a chosen point: wait signals entered and
// blocks until the test closes release; once released it no longer
// pauses.
type gate struct{ entered, release chan struct{} }

func newGate() *gate { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) wait() {
	select {
	case <-g.release:
		return
	default:
	}
	g.entered <- struct{}{}
	<-g.release
}

type gateImages struct{ g *gate }

func (i gateImages) Resolve(_ context.Context, ref string) (string, error) {
	i.g.wait()
	return ref + "@sha256:abc", nil
}

type gateMemory struct {
	g       *gate
	stopped chan struct{} // receives one value per stopped server
}

func (f gateMemory) ServeSession(_ context.Context, _ Identity, socket string) (func(), error) {
	f.g.wait()
	return func() { f.stopped <- struct{}{} }, os.WriteFile(socket, nil, 0o600)
}

type gatePreview struct{ g *gate }

func (p gatePreview) WorktreeAdded(context.Context, string, string, string) (string, error) {
	p.g.wait()
	return "", nil
}

func (gatePreview) WorktreeRemoved(context.Context, string, string) error { return nil }

type created struct {
	s   *Session
	err error
}

// createAsync starts Create and returns once the start is paused at g,
// with the id of the session being created.
func (e *env) createAsync(t *testing.T, g *gate) (string, <-chan created) {
	t.Helper()
	out := make(chan created, 1)
	go func() {
		s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: "racy"})
		out <- created{s, err}
	}()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the start never reached the pause point")
	}
	rows, err := e.st.ListSessions(context.Background(), store.SessionFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	return rows[0].ID, out
}

// waitRowStatus polls the store directly (Stop's claim is visible there
// before Stop returns).
func (e *env) waitRowStatus(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		row, err := e.st.GetSession(context.Background(), id)
		if err == nil && row.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s: %+v %v, want %s", id, row, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) containers() int {
	n := 0
	e.eng.do(func() { n = len(e.eng.specs) })
	return n
}

// TestStopWhileStarting: a stop that lands while the session is still
// being created must win; the start removes what it made instead of
// writing creating/running over the stop.
func TestStopWhileStarting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(e *env, g *gate)
	}{
		// The container exists but is not recorded on the session yet.
		{"before the container is recorded", func(e *env, g *gate) { e.eng.afterCreate = g.wait }},
		// The container runs and is recorded; memory is being attached.
		{"after the container started", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGate()
			mem := gateMemory{g: g, stopped: make(chan struct{}, 4)}
			e := newQuietEnv(t, 4, func(o *Options) {
				if tc.setup == nil {
					o.Memory = mem
				}
			})
			if tc.setup != nil {
				tc.setup(e, g)
			}
			ctx := context.Background()
			id, out := e.createAsync(t, g)

			stopped := make(chan created, 1)
			go func() {
				s, err := e.m.Stop(ctx, id)
				stopped <- created{s, err}
			}()
			e.waitRowStatus(t, id, StatusStopped) // Stop claimed the session
			close(g.release)

			res := <-stopped
			if res.err != nil || res.s.Status != StatusStopped || res.s.ContainerID != "" {
				t.Fatalf("stop: %+v %v", res.s, res.err)
			}
			if c := <-out; c.err != nil || c.s.Status != StatusStopped {
				t.Fatalf("create: %+v %v", c.s, c.err)
			}
			// Stop waited for the start to unwind, so this is final.
			if err := e.m.awaitStart(ctx, id); err != nil {
				t.Fatal(err)
			}
			got, _ := e.m.Get(ctx, id)
			if got.Status != StatusStopped || got.ContainerID != "" {
				t.Fatalf("the stop was undone: %+v", got)
			}
			if n := e.containers(); n != 0 {
				t.Fatalf("%d container(s) left after the stop", n)
			}
			if tc.setup == nil {
				select {
				case <-mem.stopped:
				default:
					t.Fatal("the memory server of the abandoned start still runs")
				}
			}
			// The session can be started again afterwards.
			if s, err := e.m.Start(ctx, id); err != nil || s.Status != StatusRunning {
				t.Fatalf("restart: %+v %v", s, err)
			}
		})
	}
}

// TestRemoveWhileStarting: a removal racing the start must not leak the
// home layer the start prepares after the removal released it.
func TestRemoveWhileStarting(t *testing.T) {
	t.Parallel()
	g := newGate()
	e := newQuietEnv(t, 4, func(o *Options) { o.Images = gateImages{g: g} })
	ctx := context.Background()
	id, out := e.createAsync(t, g) // paused before the home layer exists

	removed := make(chan error, 1)
	go func() { removed <- e.m.Remove(ctx, id, false) }()
	e.waitRowStatus(t, id, StatusStopped)
	close(g.release)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	<-out
	if err := e.m.awaitStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(ctx, id); err != ErrNotFound {
		t.Fatalf("the session survived its removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Layout().Home("claude"), "sessions", id)); !os.IsNotExist(err) {
		t.Fatalf("home layer leaked: %v", err)
	}
	if _, err := os.Stat(e.m.icaroDir(id)); !os.IsNotExist(err) {
		t.Fatalf("session run dir leaked: %v", err)
	}
	if n := e.containers(); n != 0 {
		t.Fatalf("%d container(s) leaked", n)
	}
}

// TestStopBeforeQueued: a session stopped while Create is still setting
// it up is never queued, so it never starts.
func TestStopBeforeQueued(t *testing.T) {
	t.Parallel()
	g := newGate()
	e := newQuietEnv(t, 4, func(o *Options) { o.Preview = gatePreview{g: g} })
	ctx := context.Background()
	id, out := e.createAsync(t, g)
	if s, err := e.m.Stop(ctx, id); err != nil || s.Status != StatusStopped {
		t.Fatalf("%+v %v", s, err)
	}
	close(g.release)
	if c := <-out; c.err != nil || c.s.Status != StatusStopped {
		t.Fatalf("create: %+v %v", c.s, c.err)
	}
	if n := e.containers(); n != 0 {
		t.Fatalf("a stopped session started %d container(s)", n)
	}
}

// TestStartLosesToConcurrentChange: Start re-queues only from the state
// it read; a session changed meanwhile is reported as it now is.
func TestStartLosesToConcurrentChange(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	s := e.create(t, "s")
	if _, err := e.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	// Another client changes it between Start's read and its write.
	e.sql(t, `CREATE TRIGGER race BEFORE UPDATE ON sessions WHEN NEW.status = 'queued' AND OLD.status = 'stopped'
		BEGIN UPDATE sessions SET status = 'error' WHERE id = NEW.id; SELECT RAISE(IGNORE); END`)
	got, err := e.m.Start(ctx, s.ID)
	if err != nil || got.Status != StatusError {
		t.Fatalf("%+v %v", got, err)
	}
}

// TestStopFailureIsNotReportedAsStopped: when the engine cannot stop the
// container, the session says so instead of claiming to be stopped.
func TestStopFailureIsNotReportedAsStopped(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	s := e.create(t, "s")
	e.eng.fail("stop", errBoom)
	if _, err := e.m.Stop(ctx, s.ID); err == nil {
		t.Fatal("stop must fail")
	}
	got, _ := e.m.Get(ctx, s.ID)
	if got.Status != StatusError || got.ContainerID != s.ContainerID {
		t.Fatalf("%+v", got)
	}
	e.eng.fail("stop", nil)
	if got, err := e.m.Stop(ctx, s.ID); err != nil || got.Status != StatusStopped {
		t.Fatalf("retry: %+v %v", got, err)
	}
	if st, _ := e.eng.Inspect(ctx, s.ContainerID); st.Exists {
		t.Fatal("the retried stop removes the container")
	}
}
