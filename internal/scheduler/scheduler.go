// Package scheduler fires workflow cron triggers (architecture §6). It
// persists each trigger's next fire time, so a fire missed while the
// daemon was down runs exactly once on restart — not once per missed tick.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

// Clock is the time source; tests inject a fake.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Schedule yields fire times.
type Schedule interface{ Next(time.Time) time.Time }

// Parser parses a cron expression; the default is robfig/cron's standard
// 5-field parser (UTC unless the expression starts with CRON_TZ=).
type Parser func(expr string) (Schedule, error)

// ParseStandard is the default Parser.
func ParseStandard(expr string) (Schedule, error) {
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Options configure a Scheduler.
type Options struct {
	Store  *store.Store
	Clock  Clock
	Parser Parser
	Log    *slog.Logger
	// MaxSleep bounds the wait between passes so new workflows are picked
	// up without a wake-up (default 30s).
	MaxSleep time.Duration
}

// Scheduler is an app component.
type Scheduler struct {
	store    *store.Store
	clock    Clock
	parse    Parser
	log      *slog.Logger
	maxSleep time.Duration
	wake     chan struct{}

	specs atomic.Pointer[workflow.SpecSource]
	enq   atomic.Pointer[workflow.Enqueuer]
}

// New returns a scheduler.
func New(o Options) *Scheduler {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Parser == nil {
		o.Parser = ParseStandard
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxSleep <= 0 {
		o.MaxSleep = 30 * time.Second
	}
	return &Scheduler{store: o.Store, clock: o.Clock, parse: o.Parser, log: o.Log, maxSleep: o.MaxSleep, wake: make(chan struct{}, 1)}
}

// SetSpecSource wires the source of active workflows.
func (s *Scheduler) SetSpecSource(src workflow.SpecSource) { s.specs.Store(&src) }

// SetEnqueuer wires the workflow engine.
func (s *Scheduler) SetEnqueuer(e workflow.Enqueuer) { s.enq.Store(&e) }

// Name implements app.Component.
func (s *Scheduler) Name() string { return "scheduler" }

// Wake makes the scheduler re-read workflows now (e.g. after an apply).
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run fires triggers until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		next, err := s.Tick(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("scheduler pass failed", "err", err)
		}
		wait := s.maxSleep
		if !next.IsZero() {
			if d := next.Sub(s.clock.Now()); d < wait {
				wait = max(d, 10*time.Millisecond)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-s.clock.After(wait):
		}
	}
}

type triggerKey struct {
	workflow string
	idx      int
}

// Tick performs one pass: registers new cron triggers, fires due ones
// once each, forgets removed ones, and returns the earliest next fire.
func (s *Scheduler) Tick(ctx context.Context) (time.Time, error) {
	srcp := s.specs.Load()
	if srcp == nil {
		return time.Time{}, nil
	}
	specs, err := (*srcp).ActiveSpecs(ctx)
	if err != nil {
		return time.Time{}, err
	}
	states, err := s.store.ListCronStates(ctx)
	if err != nil {
		return time.Time{}, err
	}
	have := map[triggerKey]*store.CronState{}
	for _, st := range states {
		have[triggerKey{st.Workflow, st.Idx}] = st
	}
	now := s.clock.Now().UTC()
	var earliest time.Time
	note := func(t time.Time) {
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	want := map[triggerKey]bool{}
	var errs []error
	for _, spec := range specs {
		for idx, tr := range spec.Triggers {
			if tr.Type != workflow.TriggerCron || tr.Cron == "" {
				continue
			}
			key := triggerKey{spec.Name, idx}
			want[key] = true
			sched, err := s.parse(tr.Cron)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s trigger %d: %w", spec.Name, idx, err))
				continue
			}
			st, ok := have[key]
			if !ok || st.Expr != tr.Cron {
				// First sight (or a changed expression): schedule, don't fire.
				st = &store.CronState{Workflow: spec.Name, Idx: idx, Expr: tr.Cron, NextAt: sched.Next(now)}
				if err := s.store.PutCronState(ctx, st); err != nil {
					errs = append(errs, err)
					continue
				}
				note(st.NextAt)
				continue
			}
			if now.Before(st.NextAt) {
				note(st.NextAt)
				continue
			}
			if err := s.fire(ctx, spec.Name, idx, st.NextAt, now); err != nil {
				errs = append(errs, err)
				note(now.Add(s.maxSleep))
				continue
			}
			fired := st.NextAt
			st.LastFiredAt = &fired
			// Every tick missed while down collapses into the one fire above.
			st.NextAt = sched.Next(now)
			if err := s.store.PutCronState(ctx, st); err != nil {
				errs = append(errs, err)
			}
			note(st.NextAt)
		}
	}
	for key := range have {
		if !want[key] {
			if err := s.store.DeleteCronState(ctx, key.workflow, key.idx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return earliest, errors.Join(errs...)
}

// fire enqueues one run. The idempotency key names the scheduled time, so
// a crash between enqueue and persisting the next time cannot double-fire.
func (s *Scheduler) fire(ctx context.Context, wf string, idx int, at, now time.Time) error {
	ep := s.enq.Load()
	if ep == nil {
		return errors.New("workflow engine is not wired")
	}
	res, err := (*ep).EnqueueRun(ctx, workflow.EnqueueRequest{
		Workflow:       wf,
		TriggerKind:    workflow.RunTriggerCron,
		Trigger:        map[string]any{"scheduled_at": at.UTC().Format(time.RFC3339), "fired_at": now.UTC().Format(time.RFC3339), "late": now.Sub(at) > time.Minute},
		IdempotencyKey: fmt.Sprintf("cron:%s:%d:%d", wf, idx, at.Unix()),
		Actor:          "scheduler",
	})
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", wf, err)
	}
	s.log.Info("cron fired", "workflow", wf, "trigger", idx, "scheduled_at", at, "run", res.RunID, "deduplicated", res.Deduplicated, "skipped", res.Skipped)
	return nil
}

// Next reports the persisted next fire times (for status output).
func (s *Scheduler) Next(ctx context.Context) ([]*store.CronState, error) {
	return s.store.ListCronStates(ctx)
}
