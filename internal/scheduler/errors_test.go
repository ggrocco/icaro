package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type failingSpecs struct{ calls atomic.Int32 }

func (f *failingSpecs) ActiveSpecs(context.Context) ([]*workflow.Spec, error) {
	f.calls.Add(1)
	return nil, errors.New("specs unavailable")
}

func TestDefaultsAndRunLoopSurvivesFailedPasses(t *testing.T) {
	s := New(Options{Store: openStore(t), MaxSleep: time.Millisecond})
	if _, ok := s.clock.(realClock); !ok || s.parse == nil || s.log == nil {
		t.Fatalf("defaults: %+v", s)
	}
	if d := New(Options{}).maxSleep; d != 30*time.Second {
		t.Fatalf("default max sleep %v", d)
	}
	if now := s.clock.Now(); time.Since(now) > time.Minute {
		t.Fatal(now)
	}
	src := &failingSpecs{}
	s.SetSpecSource(src)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for src.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || src.calls.Load() < 3 {
		t.Fatalf("a failed pass must be logged and retried after the sleep: %v (%d passes)", err, src.calls.Load())
	}
}

func TestRunWaitsUntilTheNextFire(t *testing.T) {
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 59, 59, 999_000_000, time.UTC))
	s := New(Options{Store: st, Clock: clk, MaxSleep: time.Hour, Log: quiet})
	s.SetSpecSource(hourly)
	s.SetEnqueuer(newEngine())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	s.Wake() // a second pass computes the (clamped) wait to the next fire
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type failingEngine struct{ n int }

func (f *failingEngine) EnqueueRun(context.Context, workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	f.n++
	return workflow.EnqueueResult{}, errors.New("engine down")
}

func TestEnqueueFailureRetriesWithoutAdvancing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	s := New(Options{Store: st, Clock: clk, MaxSleep: time.Minute, Log: quiet})
	s.SetSpecSource(hourly)
	fe := &failingEngine{}
	s.SetEnqueuer(fe)
	if _, err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC)
	clk.set(now)
	next, err := s.Tick(ctx)
	if err == nil || fe.n != 1 {
		t.Fatalf("enqueue error must be reported: %v", err)
	}
	if !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("a failed fire is retried after max sleep, got %v", next)
	}
	cs, _ := st.ListCronStates(ctx)
	if len(cs) != 1 || !cs[0].NextAt.Equal(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)) || cs[0].LastFiredAt != nil {
		t.Fatalf("a failed fire must not advance the schedule: %+v", cs)
	}
	e := newEngine()
	s.SetEnqueuer(e)
	if _, err := s.Tick(ctx); err != nil || e.n() != 1 {
		t.Fatalf("retry: %v %d", err, e.n())
	}
}

// closingEngine accepts the run, then closes the store so persisting the
// next fire time fails.
type closingEngine struct{ st *store.Store }

func (c closingEngine) EnqueueRun(context.Context, workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	_ = c.st.Close()
	return workflow.EnqueueResult{RunID: "r1"}, nil
}

func TestStoreErrorsAreReported(t *testing.T) {
	ctx := context.Background()

	t.Run("list", func(t *testing.T) {
		st := openStore(t)
		s := New(Options{Store: st, Clock: newFakeClock(time.Now()), Log: quiet})
		s.SetSpecSource(hourly)
		_ = st.Close()
		if _, err := s.Tick(ctx); err == nil {
			t.Fatal("list error must be reported")
		}
	})

	t.Run("register and forget", func(t *testing.T) {
		st := openStore(t)
		clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
		s := New(Options{Store: st, Clock: clk, Log: quiet})
		s.SetSpecSource(specs{{Version: 1, Name: "old", Triggers: []workflow.TriggerSpec{{Type: "cron", Cron: "0 * * * *"}}}})
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		// The parser runs between listing and writing state: closing the
		// store there makes both the new trigger's write and the stale
		// trigger's delete fail.
		s.parse = func(expr string) (Schedule, error) {
			_ = st.Close()
			return ParseStandard(expr)
		}
		s.SetSpecSource(hourly)
		_, err := s.Tick(ctx)
		if err == nil || strings.Count(err.Error(), "\n") < 1 {
			t.Fatalf("write and delete errors must both be reported: %v", err)
		}
	})

	t.Run("advance after fire", func(t *testing.T) {
		st := openStore(t)
		clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
		s := New(Options{Store: st, Clock: clk, Log: quiet})
		s.SetSpecSource(hourly)
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		s.SetEnqueuer(closingEngine{st})
		clk.set(time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC))
		if _, err := s.Tick(ctx); err == nil {
			t.Fatal("persisting the next fire time must report its error")
		}
	})
}

func TestRealClockAfter(t *testing.T) {
	select {
	case <-realClock{}.After(time.Millisecond):
	case <-time.After(3 * time.Second):
		t.Fatal("realClock.After never fired")
	}
}
