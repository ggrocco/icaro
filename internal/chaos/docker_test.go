package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

var errEngineGone = errors.New("cannot connect to the Docker daemon at unix:///var/run/docker.sock")

// flakyDocker is the fake engine with a switch that makes every call fail
// the way an unreachable daemon does.
type flakyDocker struct {
	*docker.Fake
	down atomic.Bool
}

func (f *flakyDocker) gone() error {
	if f.down.Load() {
		return errEngineGone
	}
	return nil
}

func (f *flakyDocker) Ping(ctx context.Context) error {
	if err := f.gone(); err != nil {
		return err
	}
	return f.Fake.Ping(ctx)
}

func (f *flakyDocker) EnsureImage(ctx context.Context, ref string) error {
	if err := f.gone(); err != nil {
		return err
	}
	return f.Fake.EnsureImage(ctx, ref)
}

func (f *flakyDocker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if err := f.gone(); err != nil {
		return err
	}
	return f.Fake.CreateVolume(ctx, name, labels)
}

func (f *flakyDocker) CreateContainer(ctx context.Context, spec docker.Spec) (string, error) {
	if err := f.gone(); err != nil {
		return "", err
	}
	return f.Fake.CreateContainer(ctx, spec)
}

func (f *flakyDocker) StartContainer(ctx context.Context, id string) error {
	if err := f.gone(); err != nil {
		return err
	}
	return f.Fake.StartContainer(ctx, id)
}

type engineFixture struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	docker *flakyDocker
}

func newEngine(t *testing.T) *engineFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &engineFixture{t: t, ctx: ctx, st: st, docker: &flakyDocker{Fake: docker.NewFake()}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := engine.New(engine.Config{
		Concurrency: 4, StepTimeout: time.Minute, LogsDir: t.TempDir(), RunsDir: t.TempDir(),
		RetryBase: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
	}, engine.Deps{Store: st, Docker: f.docker, Events: events.New(ctx), Log: quiet})
	go func() { _ = e.Run(ctx) }()
	return f
}

func (f *engineFixture) enqueue(yaml string) string {
	f.t.Helper()
	spec, issues, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil || len(issues) > 0 {
		f.t.Fatalf("workflow: %v %v", err, issues)
	}
	w, err := f.st.ApplyWorkflow(f.ctx, spec.Name, yaml, "{}", "test")
	if err != nil {
		f.t.Fatal(err)
	}
	steps, err := engine.StepInits(spec)
	if err != nil {
		f.t.Fatal(err)
	}
	tj, _ := json.Marshal(map[string]any{})
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version, TriggerKind: "manual", TriggerJSON: string(tj)}
	if err := f.st.CreateRun(f.ctx, run, steps); err != nil {
		f.t.Fatal(err)
	}
	return run.ID
}

func (f *engineFixture) wait(id string) *store.Run {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := f.st.GetRun(f.ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		switch r.Status {
		case store.RunSucceeded, store.RunFailed, store.RunCancelled:
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("run %s stuck", id)
	return nil
}

func wf(name string, retry int) string {
	return "version: 1\nname: " + name + "\nsteps:\n  - id: work\n    image: alpine:3.20\n    retry: " +
		strconv.Itoa(retry) + "\n    command: echo hi\n"
}

// Docker vanishing mid-flight fails the attempt with the engine's error;
// the retry policy carries the step across a short outage.
func TestDockerOutageRecoveredByRetry(t *testing.T) {
	f := newEngine(t)
	f.docker.down.Store(true)
	id := f.enqueue(wf("outage-retry", 3))
	time.AfterFunc(250*time.Millisecond, func() { f.docker.down.Store(false) })
	r := f.wait(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run %s: %s", r.Status, r.Error)
	}
	st, err := f.st.GetStepRun(f.ctx, id, "work")
	if err != nil || st.Attempt < 2 {
		t.Fatalf("expected a retried attempt: %+v %v", st, err)
	}
}

// Without retries the run fails deterministically, naming the cause, and
// the engine keeps working once Docker is back.
func TestDockerOutageFailsThenEngineRecovers(t *testing.T) {
	f := newEngine(t)
	f.docker.down.Store(true)
	id := f.enqueue(wf("outage-fail", 0))
	r := f.wait(id)
	st, _ := f.st.GetStepRun(f.ctx, id, "work")
	if r.Status != store.RunFailed || st == nil || !strings.Contains(st.Error, "Docker daemon") {
		t.Fatalf("want a failed run naming Docker: run %s %q, step %+v", r.Status, r.Error, st)
	}
	f.docker.down.Store(false)
	if r := f.wait(f.enqueue(wf("outage-after", 0))); r.Status != store.RunSucceeded {
		t.Fatalf("engine did not recover: %s %s", r.Status, r.Error)
	}
}
