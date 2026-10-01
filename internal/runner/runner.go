// Package runner claims queued runs and executes their steps.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ggrocco/icaro/internal/runner/docker"
	"github.com/ggrocco/icaro/internal/runner/logs"
	"github.com/ggrocco/icaro/internal/store"
)

// Config tunes the runner.
type Config struct {
	RunnerID      string
	Concurrency   int
	PollInterval  time.Duration
	Heartbeat     time.Duration
	StaleAfter    time.Duration // other runners' runs without heartbeat for this long are re-queued
	StepTimeout   time.Duration // default and ceiling for step timeouts
	MemoryBytes   int64
	NanoCPUs      int64
	Pids          int64
	NoFile        int64
	StrictRuntime string
	LogsDir       string
	// KeepWorkspaceOnFailure leaves the run volumes behind for inspection.
	KeepWorkspaceOnFailure bool
	// OutputLimit caps /icaro/output.json (default 1 MiB).
	OutputLimit int64
}

// Secrets resolves a connection's fields for injection into steps.
type Secrets interface {
	ResolveConnection(ctx context.Context, name string) (fields map[string]string, err error)
}

// Listener is notified when a run or step changes; used by SSE later.
type Listener interface {
	RunChanged(runID string)
}

// Runner executes runs.
type Runner struct {
	cfg      Config
	store    *store.Store
	docker   docker.Client
	logs     logs.Store
	secrets  Secrets
	listener Listener
	log      *slog.Logger
	wake     chan struct{}

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// New creates a runner. secrets and listener may be nil.
func New(cfg Config, st *store.Store, dc docker.Client, secrets Secrets, listener Listener, log *slog.Logger) *Runner {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 2 * time.Minute
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 30 * time.Minute
	}
	if cfg.OutputLimit <= 0 {
		cfg.OutputLimit = 1 << 20
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		cfg: cfg, store: st, docker: dc, logs: logs.Store{Dir: cfg.LogsDir},
		secrets: secrets, listener: listener, log: log.With("runner", cfg.RunnerID),
		wake: make(chan struct{}, 1), active: map[string]context.CancelFunc{},
	}
}

// Wake nudges the claim loop (e.g. after enqueueing a run in-process).
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run reconciles runs left over from a previous process, then claims and
// executes runs until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.docker.Ping(ctx); err != nil {
		return err
	}
	if err := r.Reconcile(ctx); err != nil {
		r.log.Error("reconcile", "err", err)
	}
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	stale := time.NewTicker(r.cfg.StaleAfter)
	defer stale.Stop()

	for {
		// Fill free slots.
		for len(sem) < cap(sem) {
			run, err := r.store.ClaimRun(ctx, r.cfg.RunnerID)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				r.log.Error("claim", "err", err)
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r.execute(ctx, run, false)
			}()
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-r.wake:
		case <-ticker.C:
		case <-stale.C:
			if n, err := r.store.ReleaseStaleRuns(ctx, r.cfg.RunnerID, r.cfg.StaleAfter); err != nil {
				r.log.Error("release stale", "err", err)
			} else if n > 0 {
				r.log.Warn("re-queued stale runs", "count", n)
			}
		}
	}
}

// Reconcile resumes runs this runner was executing before a restart.
func (r *Runner) Reconcile(ctx context.Context) error {
	runs, err := r.store.ListRunsClaimedBy(ctx, r.cfg.RunnerID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		r.log.Info("resuming run", "run", run.ID)
		go r.execute(ctx, run, true)
	}
	return nil
}

// Cancel asks an in-flight run on this runner to stop.
func (r *Runner) Cancel(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.active[runID]; ok {
		cancel()
	}
}

func (r *Runner) track(runID string, cancel context.CancelFunc) func() {
	r.mu.Lock()
	r.active[runID] = cancel
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.active, runID)
		r.mu.Unlock()
	}
}

func (r *Runner) notify(runID string) {
	if r.listener != nil {
		r.listener.RunChanged(runID)
	}
}
