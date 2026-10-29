// Package engine executes workflow runs: it claims queued runs from the
// store, plans each run's DAG, runs independent steps in parallel inside
// the run's session (ADR 0010), captures outputs and logs, and recovers
// runs left behind by a restart (ADR 0006).
//
// Ownership: the claim loop goroutine owns the set of active runs; each
// run's scheduler goroutine owns that run's step state; each step
// goroutine owns its step row while it executes. They communicate over
// channels; there are no mutexes.
package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// Docker is the engine API surface the executor uses.
type Docker interface {
	Ping(ctx context.Context) error
	EnsureImage(ctx context.Context, ref string) error
	CreateVolume(ctx context.Context, name string, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, spec docker.Spec) (string, error)
	StartContainer(ctx context.Context, id string) error
	CopyIn(ctx context.Context, id, destDir string, tar io.Reader) error
	CopyOut(ctx context.Context, id, path string) (io.ReadCloser, error)
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	Wait(ctx context.Context, id string) (int, error)
	Kill(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	ListByLabel(ctx context.Context, labels map[string]string) ([]string, error)
	// ContainerStarted distinguishes "created, never started" on re-attach.
	ContainerStarted(ctx context.Context, id string) (exists, started bool, err error)
}

// Config tunes the engine.
type Config struct {
	// Concurrency caps concurrently executing steps across all runs.
	Concurrency int
	// MaxRuns caps concurrently executing runs (default Concurrency).
	MaxRuns int
	// RunParallelism caps parallel steps within one run; 0 = no extra cap.
	RunParallelism int
	// StepTimeout is the default and the ceiling for step timeouts.
	StepTimeout time.Duration
	// Limits are the container ceilings (memory, CPU, pids, nofile).
	Limits sandbox.Limits
	// Profile is the run sessions' profile (workflows.profile, ADR 0010)
	// and Allow its egress allowlist.
	Profile string
	Allow   []string
	// Runtime is the OCI runtime of step containers: the run profile's
	// runtime (e.g. runsc for gVisor); empty is Docker's default.
	Runtime string
	// User runs step containers as uid:gid ("" = image default).
	User string
	// LogsDir holds step logs: <dir>/<run>/<step>.<attempt>.log.
	LogsDir string
	// RunsDir holds scratch workspaces when no session manager is wired.
	RunsDir string
	// OutputLimit caps /icaro/output.json, stdout and file captures (1 MiB).
	OutputLimit int64
	// RetryBase is the first retry delay; it doubles per attempt (2s).
	RetryBase time.Duration
	// PollInterval is the claim loop's idle poll (1s); Heartbeat is how
	// often running runs refresh liveness and look for cancel requests.
	PollInterval time.Duration
	Heartbeat    time.Duration
	// KeepWorkspaceOnFailure leaves a failed run's workspace for inspection.
	KeepWorkspaceOnFailure bool
}

// Deps are the engine's collaborators. Store and Docker are required;
// the rest degrade safely when nil: no Sessions => local scratch
// workspaces, no Egress => network none, no Actions => uses steps fail,
// no Secrets => ${{ secrets.* }} fails, no Catalog => only host-bound
// secrets count as integration credentials.
type Deps struct {
	Store    *store.Store
	Docker   Docker
	Sessions session.RunSessions
	Egress   container.Egress
	Actions  workflow.ActionExecutor
	Secrets  workflow.SecretResolver
	Catalog  workflow.Catalog
	Events   *events.Bus
	Log      *slog.Logger
}

// RunnerID is the claim owner recorded on runs (one daemon, ADR 0001).
const RunnerID = "daemon"

// errCancelled is the cancellation cause for user-requested cancels;
// any other cancellation is a daemon shutdown, which leaves containers
// running for re-attach.
var errCancelled = errors.New("cancelled")

// Engine executes runs.
type Engine struct {
	cfg   Config
	d     Deps
	log   *slog.Logger
	slots chan struct{} // global step concurrency

	wake   chan struct{}
	cancel chan string
}

// New builds an engine.
func New(cfg Config, d Deps) *Engine {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.MaxRuns < 1 {
		cfg.MaxRuns = cfg.Concurrency
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 30 * time.Minute
	}
	if cfg.OutputLimit <= 0 {
		cfg.OutputLimit = 1 << 20
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 2 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 5 * time.Second
	}
	if cfg.Profile == "" {
		cfg.Profile = "workflow"
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Sessions == nil {
		d.Sessions = &localSessions{dir: cfg.RunsDir}
	}
	return &Engine{
		cfg: cfg, d: d, log: d.Log.With("component", "workflow-engine"),
		slots: make(chan struct{}, cfg.Concurrency),
		wake:  make(chan struct{}, 1), cancel: make(chan string, 16),
	}
}

// Wake nudges the claim loop after a run was enqueued.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Cancel asks a running run to stop; queued runs are cancelled by the
// store directly.
func (e *Engine) Cancel(runID string) {
	select {
	case e.cancel <- runID:
	default:
		go func() { e.cancel <- runID }()
	}
}

// Run resumes runs left running by a previous daemon, then claims and
// executes queued runs until ctx ends. On shutdown it stops waiting on
// step containers without killing them, so the next start re-attaches.
func (e *Engine) Run(ctx context.Context) error {
	type running struct {
		cancel context.CancelCauseFunc
	}
	active := map[string]running{}
	done := make(chan string, e.cfg.MaxRuns)
	start := func(run *store.Run, resumed bool) {
		rctx, cancel := context.WithCancelCause(ctx)
		active[run.ID] = running{cancel: cancel}
		go func() {
			defer cancel(nil)
			e.executeRun(rctx, cancel, run, resumed)
			done <- run.ID
		}()
	}

	left, err := e.d.Store.ListRunsByStatus(ctx, store.RunRunning)
	if err != nil {
		return err
	}
	for _, run := range left {
		e.log.Info("resuming run after restart", "run", run.ID, "workflow", run.WorkflowName)
		start(run, true)
	}

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for len(active) < e.cfg.MaxRuns && ctx.Err() == nil {
			run, err := e.d.Store.ClaimRun(ctx, RunnerID)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					e.log.Error("claim run", "err", err)
				}
				break
			}
			start(run, false)
		}
		select {
		case <-ctx.Done():
			for len(active) > 0 {
				delete(active, <-done)
			}
			return nil
		case id := <-done:
			delete(active, id)
		case id := <-e.cancel:
			if r, ok := active[id]; ok {
				r.cancel(errCancelled)
			}
		case <-e.wake:
		case <-ticker.C:
		}
	}
}

func (e *Engine) publish(ev events.Event) { e.d.Events.Publish(ev) }

// StepInits returns the step rows to create with a run of spec, in plan
// order.
func StepInits(spec *workflow.Spec) ([]store.StepInit, error) {
	plan, err := workflow.NewPlan(spec)
	if err != nil {
		return nil, err
	}
	out := make([]store.StepInit, 0, len(plan.Order))
	for i, id := range plan.Order {
		out = append(out, store.StepInit{StepID: id, Idx: i, Kind: spec.Step(id).Kind()})
	}
	return out, nil
}
