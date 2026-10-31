package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/serial"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// fakeClock keeps the time in a one-slot channel: whoever has received
// it owns it until sending it back.
type fakeClock struct {
	now chan time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{now: make(chan time.Time, 1)}
	c.now <- t
	return c
}

func (c *fakeClock) Now() time.Time {
	t := <-c.now
	c.now <- t
	return t
}

func (c *fakeClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time)
	return ch // never fires; tests drive Tick directly or use Wake
}

func (c *fakeClock) set(t time.Time) {
	<-c.now
	c.now <- t
}

type engine struct {
	ops  serial.Queue
	keys map[string]bool
	reqs []workflow.EnqueueRequest
}

func newEngine() *engine { return &engine{ops: serial.New(), keys: map[string]bool{}} }

func (e *engine) EnqueueRun(_ context.Context, r workflow.EnqueueRequest) (res workflow.EnqueueResult, err error) {
	e.ops.Do(func() {
		if e.keys[r.IdempotencyKey] {
			res = workflow.EnqueueResult{Deduplicated: true}
			return
		}
		e.keys[r.IdempotencyKey] = true
		e.reqs = append(e.reqs, r)
		res = workflow.EnqueueResult{RunID: fmt.Sprint(len(e.reqs))}
	})
	return res, err
}

func (e *engine) n() (n int) {
	e.ops.Do(func() { n = len(e.reqs) })
	return n
}

type specs []*workflow.Spec

func (s specs) ActiveSpecs(context.Context) ([]*workflow.Spec, error) { return s, nil }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func newSched(st *store.Store, clk *fakeClock, e *engine, sp specs) *Scheduler {
	s := New(Options{Store: st, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s.SetSpecSource(sp)
	s.SetEnqueuer(e)
	return s
}

var hourly = specs{{Version: 1, Name: "nightly", Triggers: []workflow.TriggerSpec{{Type: "manual"}, {Type: "cron", Cron: "0 * * * *"}}}}

func TestMissedFireDuringRestartRunsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC))
	e := newEngine()

	s := newSched(st, clk, e, hourly)
	next, err := s.Tick(ctx)
	if err != nil || !next.Equal(time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)) || e.n() != 0 {
		t.Fatalf("register: %v %v (runs %d)", next, err, e.n())
	}
	// On time.
	clk.set(time.Date(2026, 10, 20, 11, 0, 5, 0, time.UTC))
	if _, err := s.Tick(ctx); err != nil || e.n() != 1 {
		t.Fatalf("on-time fire: %v %d", err, e.n())
	}
	if e.reqs[0].IdempotencyKey != fmt.Sprintf("cron:nightly:1:%d", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC).Unix()) || e.reqs[0].TriggerKind != workflow.RunTriggerCron {
		t.Fatalf("%+v", e.reqs[0])
	}

	// The daemon is down from 11:00 to 14:30 (12:00, 13:00, 14:00 missed);
	// a fresh scheduler instance stands in for the restarted daemon.
	clk.set(time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC))
	restarted := newSched(st, clk, e, hourly)
	next, err = restarted.Tick(ctx)
	if err != nil || e.n() != 2 {
		t.Fatalf("missed fires must collapse into one: runs=%d err=%v", e.n(), err)
	}
	if late := e.reqs[1].Trigger["late"]; late != true || e.reqs[1].Trigger["scheduled_at"] != "2026-10-20T12:00:00Z" {
		t.Fatalf("%v", e.reqs[1].Trigger)
	}
	// The validator checks cron workflows against these fields.
	for _, k := range workflow.CronFields {
		if _, ok := e.reqs[1].Trigger[k]; !ok || len(e.reqs[1].Trigger) != len(workflow.CronFields) {
			t.Fatalf("cron payload %v does not match workflow.CronFields", e.reqs[1].Trigger)
		}
	}
	if !next.Equal(time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)) {
		t.Fatal(next)
	}
	if _, err := restarted.Tick(ctx); err != nil || e.n() != 2 {
		t.Fatal("second pass at the same time must not fire again")
	}
}

func TestChangedAndRemovedTriggers(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	e := newEngine()
	s := newSched(st, clk, e, hourly)
	_, _ = s.Tick(ctx)
	changed := specs{{Version: 1, Name: "nightly", Triggers: []workflow.TriggerSpec{{Type: "manual"}, {Type: "cron", Cron: "*/5 * * * *"}}}}
	s.SetSpecSource(changed)
	next, _ := s.Tick(ctx)
	if !next.Equal(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)) || e.n() != 0 {
		t.Fatalf("a changed expression reschedules without firing: %v", next)
	}
	bad := specs{{Version: 1, Name: "broken", Triggers: []workflow.TriggerSpec{{Type: "cron", Cron: "not cron"}}}}
	s.SetSpecSource(bad)
	if _, err := s.Tick(ctx); err == nil {
		t.Fatal("invalid cron must be reported")
	}
	cs, _ := st.ListCronStates(ctx)
	if len(cs) != 0 {
		t.Fatalf("removed triggers must be forgotten: %+v", cs)
	}
}

func TestRunLoopAndWake(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	e := newEngine()
	s := newSched(st, clk, e, hourly)
	if _, err := s.Tick(context.Background()); err != nil { // register before time moves
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	clk.set(time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC))
	deadline := time.Now().Add(3 * time.Second)
	for e.n() == 0 && time.Now().Before(deadline) {
		s.Wake()
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || e.n() != 1 {
		t.Fatalf("run loop: %v runs=%d", err, e.n())
	}
	if cs, _ := s.Next(context.Background()); len(cs) != 1 || cs[0].LastFiredAt == nil {
		t.Fatal("state")
	}
	if s.Name() != "scheduler" {
		t.Fatal("name")
	}
}

func TestNoEngineDoesNotAdvance(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	s := New(Options{Store: st, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if next, err := s.Tick(ctx); err != nil || !next.IsZero() {
		t.Fatal("no spec source: idle")
	}
	s.SetSpecSource(hourly)
	_, _ = s.Tick(ctx)
	clk.set(time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC))
	if _, err := s.Tick(ctx); err == nil {
		t.Fatal("must report the missing engine")
	}
	e := newEngine()
	s.SetEnqueuer(e)
	if _, err := s.Tick(ctx); err != nil || e.n() != 1 {
		t.Fatal("the pending fire must happen once the engine is wired")
	}
}
