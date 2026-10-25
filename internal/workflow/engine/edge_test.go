package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/logs"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// faultDocker injects Docker API failures by method name. Docker is an
// edge of the engine, so faking its failures is fair game.
type faultDocker struct {
	*docker.Fake
	fail    map[string]error
	copyOut func() (io.ReadCloser, error)
}

func (d *faultDocker) err(method string) error { return d.fail[method] }

func (d *faultDocker) ContainerStarted(ctx context.Context, id string) (bool, bool, error) {
	if err := d.err("ContainerStarted"); err != nil {
		return false, false, err
	}
	return d.Fake.ContainerStarted(ctx, id)
}

func (d *faultDocker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if err := d.err("CreateVolume"); err != nil {
		return err
	}
	return d.Fake.CreateVolume(ctx, name, labels)
}

func (d *faultDocker) CreateContainer(ctx context.Context, spec docker.Spec) (string, error) {
	if err := d.err("CreateContainer"); err != nil {
		return "", err
	}
	return d.Fake.CreateContainer(ctx, spec)
}

func (d *faultDocker) CopyIn(ctx context.Context, id, dest string, r io.Reader) error {
	if err := d.err("CopyIn"); err != nil {
		return err
	}
	return d.Fake.CopyIn(ctx, id, dest, r)
}

func (d *faultDocker) StartContainer(ctx context.Context, id string) error {
	if err := d.err("StartContainer"); err != nil {
		return err
	}
	return d.Fake.StartContainer(ctx, id)
}

// Logs also ends the stream when ctx ends, as the real client does (the
// shared fake keeps following until the container exits).
func (d *faultDocker) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	if err := d.err("Logs"); err != nil {
		return nil, err
	}
	rc, err := d.Fake.Logs(ctx, id, follow)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()
	return rc, nil
}

func (d *faultDocker) Wait(ctx context.Context, id string) (int, error) {
	if err := d.err("Wait"); err != nil {
		return -1, err
	}
	return d.Fake.Wait(ctx, id)
}

func (d *faultDocker) CopyOut(ctx context.Context, id, path string) (io.ReadCloser, error) {
	if d.copyOut != nil {
		return d.copyOut()
	}
	return d.Fake.CopyOut(ctx, id, path)
}

func (f *fixture) faults(fail map[string]error) *faultDocker {
	fd := &faultDocker{Fake: f.docker, fail: fail}
	f.deps.Docker = fd
	return fd
}

// blockingActions blocks until the step's context ends.
type blockingActions struct{ called chan struct{} }

func (a *blockingActions) Execute(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
	if a.called != nil {
		close(a.called)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// waitStep polls until a step row satisfies ok.
func (f *fixture) waitStep(runID, id string, ok func(*store.StepRun) bool) *store.StepRun {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.step(runID, id); ok(r) {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("step %s never reached the expected state: %s", id, f.dump(runID))
	return nil
}

// TestStepFailures covers the ways a single step attempt fails: bad
// interpolation, missing collaborators, Docker API errors and output
// capture violations. Each failure is the step's (and the run's) error.
func TestStepFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		yaml    string
		setup   func(f *fixture)
		wantMsg string
	}{
		{name: "command", yaml: "command: echo ${{ trigger.missing }}", wantMsg: "command: trigger.missing is not defined"},
		{name: "input", yaml: "input: { x: \"${{ trigger.missing }}\" }", wantMsg: "input: x: trigger.missing is not defined"},
		{name: "env", yaml: "env: { A: \"${{ trigger.missing }}\" }", wantMsg: "env.A: trigger.missing is not defined"},
		{name: "repository without sessions", yaml: "repository: { ref: github.com/acme/app }", wantMsg: "repository github.com/acme/app: repository steps need the session manager"},
		{name: "pull", setup: func(f *fixture) { f.docker.PullErr = errors.New("pull denied") }, wantMsg: "pull denied"},
		{name: "volume", setup: func(f *fixture) { f.faults(map[string]error{"CreateVolume": boom}) }, wantMsg: "create io volume: boom"},
		{name: "create", setup: func(f *fixture) { f.faults(map[string]error{"CreateContainer": boom}) }, wantMsg: "create container: boom"},
		{name: "copy in", setup: func(f *fixture) { f.faults(map[string]error{"CopyIn": boom}) }, wantMsg: "copy inputs: boom"},
		{name: "start", setup: func(f *fixture) { f.faults(map[string]error{"StartContainer": boom}) }, wantMsg: "start container: boom"},
		{name: "wait", setup: func(f *fixture) { f.faults(map[string]error{"Wait": boom}) }, wantMsg: "wait: boom"},
		{name: "copy out", yaml: "outputs: { x: json:x }", setup: func(f *fixture) {
			f.faults(nil).copyOut = func() (io.ReadCloser, error) { return nil, boom }
		}, wantMsg: "read output.json: boom"},
		{name: "copy out not a tar", yaml: "outputs: { x: json:x }", setup: func(f *fixture) {
			f.faults(nil).copyOut = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("garbage")), nil }
		}, wantMsg: "read output.json:"},
		{name: "empty output.json", yaml: "outputs: { x: json:x }", setup: func(f *fixture) {
			f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Output: []byte(" \n")} }
		}, wantMsg: "output x: output.json has no x"},
		{name: "stdout over limit", yaml: "outputs: { o: stdout }", setup: func(f *fixture) {
			f.cfg.OutputLimit = 8
			f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Stdout: "0123456789\n"} }
		}, wantMsg: "output o: stdout exceeds 8 bytes"},
		{name: "file missing", yaml: "outputs: { f: file:nope.txt }", wantMsg: "output f: file nope.txt:"},
		{name: "file over limit", yaml: "outputs: { f: file:big.txt }", setup: func(f *fixture) {
			f.cfg.OutputLimit = 8
			f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
				_ = os.WriteFile(filepath.Join(c.Spec.HostConfig.Mounts[0].Source, "big.txt"), []byte("0123456789"), 0o600)
				return docker.Behavior{}
			}
		}, wantMsg: "file big.txt exceeds 8 bytes"},
		{name: "logs dir", setup: func(f *fixture) {
			p := filepath.Join(t.TempDir(), "file")
			_ = os.WriteFile(p, nil, 0o600)
			f.cfg.LogsDir = p
		}, wantMsg: "not a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if c.setup != nil {
				c.setup(f)
			}
			yaml := "version: 1\nname: fails\nsteps:\n  - id: s1\n    image: img\n"
			if c.yaml != "" {
				yaml += "    " + c.yaml + "\n"
			}
			run := f.enqueue(yaml, nil)
			_, stop := f.start()
			defer stop()
			got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
			r := f.step(run.ID, "s1")
			if got.Status != store.RunFailed || r.Status != store.StepFailed || !strings.Contains(r.Error, c.wantMsg) {
				t.Fatalf("run %s %q; step %s %q; want %q", got.Status, got.Error, r.Status, r.Error, c.wantMsg)
			}
			if r.StartedAt == nil || r.FinishedAt == nil {
				t.Fatalf("a failed step has start and finish times: %+v", r)
			}
		})
	}
}

func TestUsesStepFailures(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.deps.Actions = &fakeActions{}
		run := f.enqueue("version: 1\nname: uses-input\nsteps:\n  - { id: post, uses: slack.post-message, input: { text: \"${{ trigger.missing }}\" } }\n", nil)
		_, stop := f.start()
		defer stop()
		f.wait(run.ID, store.RunFailed)
		if r := f.step(run.ID, "post"); !strings.Contains(r.Error, "input: text: trigger.missing is not defined") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.deps.Actions = &blockingActions{}
		run := f.enqueue("version: 1\nname: uses-timeout\nsteps:\n  - { id: post, uses: slack.post-message, timeout: 50ms }\n", nil)
		_, stop := f.start()
		defer stop()
		f.wait(run.ID, store.RunFailed)
		r := f.step(run.ID, "post")
		if !strings.Contains(r.Error, "timed out after 50ms") {
			t.Fatalf("%+v", r)
		}
		if b, _ := os.ReadFile(r.LogPath); !strings.Contains(string(b), "error: timed out after 50ms") {
			t.Fatalf("log %q", b)
		}
	})
	t.Run("logs dir", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.deps.Actions = &fakeActions{}
		p := filepath.Join(t.TempDir(), "file")
		_ = os.WriteFile(p, nil, 0o600)
		f.cfg.LogsDir = p
		run := f.enqueue("version: 1\nname: uses-logs\nsteps:\n  - { id: post, uses: slack.post-message }\n", nil)
		_, stop := f.start()
		defer stop()
		f.wait(run.ID, store.RunFailed)
		if r := f.step(run.ID, "post"); !strings.Contains(r.Error, "not a directory") {
			t.Fatalf("%+v", r)
		}
	})
}

// A log stream failure is recorded in the step log but does not fail a
// step whose container exited cleanly; a missing output.json is an empty
// object.
func TestLogStreamErrorAndMissingOutputFile(t *testing.T) {
	f := newFixture(t)
	fd := f.faults(map[string]error{"Logs": errors.New("stream gone")})
	fd.copyOut = func() (io.ReadCloser, error) { return nil, docker.ErrNotFound }
	run := f.enqueue("version: 1\nname: no-logs\nsteps:\n  - { id: s1, image: img }\n", nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if b, _ := os.ReadFile(f.step(run.ID, "s1").LogPath); !strings.Contains(string(b), "error: read logs: stream gone") {
		t.Fatalf("log %q", b)
	}
}

type flakySessions struct {
	stubSessions
}

func (s *flakySessions) SetContainer(context.Context, string, string) error {
	return errors.New("session store unavailable")
}

func (s *flakySessions) FinishRun(context.Context, string, bool) error {
	return errors.New("worktree busy")
}

// Bookkeeping failures of the session manager are logged, not fatal; a
// leftover container of the same attempt is removed before a new one is
// created; step env and input keys reach the container.
func TestStepContainerDetails(t *testing.T) {
	f := newFixture(t)
	f.deps.Sessions = &flakySessions{stubSessions{dir: t.TempDir()}}
	run := f.enqueue(`
version: 1
name: details
steps:
  - id: s1
    image: img
    env: { MODE: "${{ trigger.mode }}" }
    input: { pr-number: 7, nested: { a: 1 } }
`, map[string]any{"mode": "fast"})
	left := f.docker.AddContainer("leftover", docker.Spec{Labels: map[string]string{LabelRun: run.ID, LabelStep: "s1", LabelAttempt: "1"}}, docker.Behavior{}, false)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if !f.docker.Container(left.ID).Removed {
		t.Fatal("leftover container of the same attempt must be removed")
	}
	env := strings.Join(f.containersForStep("s1")[0].Spec.Env, "\n")
	if !strings.Contains(env, "MODE=fast") || !strings.Contains(env, "ICARO_INPUT_PR_NUMBER=7") || strings.Contains(env, "ICARO_INPUT_NESTED") {
		t.Fatalf("env:\n%s", env)
	}
}

type failingSessions struct {
	stubSessions
	err error
}

func (s *failingSessions) StartRun(context.Context, string, string) (session.RunWorkspace, error) {
	return session.RunWorkspace{}, s.err
}

type failingEgress struct{}

func (failingEgress) Ensure(context.Context, string, []string) (container.Attachment, error) {
	return container.Attachment{}, errors.New("no proxy")
}

type brokenSecrets struct{ namesErr, hostsErr bool }

func (s brokenSecrets) Hosts(context.Context, string) ([]string, error) {
	if s.hostsErr {
		return nil, errors.New("keychain locked")
	}
	return nil, nil
}

func (s brokenSecrets) Names(context.Context) ([]string, error) {
	if s.namesErr {
		return nil, errors.New("keychain locked")
	}
	return []string{"tok"}, nil
}

func (s brokenSecrets) Resolve(context.Context, string) (string, error) {
	return "", errors.New("decrypt failed")
}

// Failures while preparing the run session fail the run before any step.
func TestPrepareFailures(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		setup func(f *fixture)
		want  string
	}{
		{name: "session", setup: func(f *fixture) { f.deps.Sessions = &failingSessions{err: errors.New("disk full")} }, want: "start run session: disk full"},
		{name: "egress", setup: func(f *fixture) { f.deps.Egress = failingEgress{} }, want: "egress for profile workflow: no proxy"},
		{name: "secret names", setup: func(f *fixture) { f.deps.Secrets = brokenSecrets{namesErr: true} }, want: "list secrets: keychain locked"},
		{name: "secret value", setup: func(f *fixture) { f.deps.Secrets = brokenSecrets{} }, want: "resolve secret tok: decrypt failed"},
		{name: "secret binding", yaml: "env:\n  A: ${{ secrets.tok }}\n", setup: func(f *fixture) { f.deps.Secrets = brokenSecrets{hostsErr: true} }, want: "secret tok: keychain locked"},
		{name: "workflow env", yaml: "env:\n  A: ${{ trigger.missing }}\n", want: "env.A: trigger.missing is not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if c.setup != nil {
				c.setup(f)
			}
			run := f.enqueue("version: 1\nname: prep\n"+c.yaml+"steps:\n  - { id: s1, image: img }\n", nil)
			_, stop := f.start()
			defer stop()
			got := f.wait(run.ID, store.RunFailed)
			if !strings.Contains(got.Error, c.want) {
				t.Fatalf("run error %q, want %q", got.Error, c.want)
			}
			if len(f.containersForStep("s1")) != 0 {
				t.Fatal("no step may start")
			}
		})
	}
}

// A run whose stored state is unusable fails with a clear error.
func TestBrokenRunState(t *testing.T) {
	f := newFixture(t)
	w, err := f.st.ApplyWorkflow(f.ctx, "bad-yaml", "steps: [", "{}", "t")
	if err != nil {
		t.Fatal(err)
	}
	badYAML := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
	if err := f.st.CreateRun(f.ctx, badYAML, nil); err != nil {
		t.Fatal(err)
	}
	noVersion := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 9, TriggerKind: "manual"}
	if err := f.st.CreateRun(f.ctx, noVersion, nil); err != nil {
		t.Fatal(err)
	}
	ok, err := f.st.ApplyWorkflow(f.ctx, "no-rows", "version: 1\nname: no-rows\nsteps:\n  - { id: s1, image: img }\n", "{}", "t")
	if err != nil {
		t.Fatal(err)
	}
	noRows := &store.Run{WorkflowID: ok.ID, WorkflowName: ok.Name, WorkflowVersion: 1, TriggerKind: "manual", TriggerJSON: "not json"}
	if err := f.st.CreateRun(f.ctx, noRows, nil); err != nil {
		t.Fatal(err)
	}
	_, stop := f.start()
	defer stop()
	for run, want := range map[*store.Run]string{badYAML: "parse workflow", noVersion: "load workflow version", noRows: "run has no state for step s1"} {
		if got := f.wait(run.ID, store.RunFailed); !strings.Contains(got.Error, want) {
			t.Fatalf("error %q, want %q", got.Error, want)
		}
	}
}

// claimRunning simulates a previous daemon that claimed the run and left
// step s1 running at the given attempt.
func (f *fixture) claimRunning(run *store.Run, attempt int, containerID string) {
	f.t.Helper()
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		f.t.Fatal(err)
	}
	row := f.step(run.ID, "s1")
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.StartedAt = store.StepRunning, attempt, containerID, &now
	if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
		f.t.Fatal(err)
	}
}

func TestReattachEdgeCases(t *testing.T) {
	t.Run("uses step is retried, not resumed", func(t *testing.T) {
		for _, retry := range []string{"0", "1"} {
			f := newFixture(t)
			acts := &fakeActions{}
			f.deps.Actions = acts
			run := f.enqueue("version: 1\nname: uses-restart\nsteps:\n  - { id: s1, uses: slack.post-message, retry: "+retry+" }\n", nil)
			f.claimRunning(run, 1, "")
			_, stop := f.start()
			got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
			stop()
			r := f.step(run.ID, "s1")
			if retry == "0" && (got.Status != store.RunFailed || !strings.Contains(r.Error, "interrupted by a daemon restart")) {
				t.Fatalf("retry 0: %+v %+v", got, r)
			}
			if retry == "1" && (got.Status != store.RunSucceeded || r.Attempt != 2 || len(acts.calls) != 1) {
				t.Fatalf("retry 1: %+v %+v calls=%d", got, r, len(acts.calls))
			}
		}
	})
	t.Run("never attempted", func(t *testing.T) {
		// Running but with no attempt recorded: nothing ran, so the first
		// attempt starts now, even without retries.
		f := newFixture(t)
		run := f.enqueue("version: 1\nname: zero-attempt\nsteps:\n  - { id: s1, image: img }\n", nil)
		f.claimRunning(run, 0, "")
		_, stop := f.start()
		defer stop()
		f.wait(run.ID, store.RunSucceeded)
		if r := f.step(run.ID, "s1"); r.Attempt != 1 || len(f.containersForStep("s1")) != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("docker probe fails", func(t *testing.T) {
		f := newFixture(t)
		f.faults(map[string]error{"ContainerStarted": errors.New("daemon unreachable")})
		run := f.enqueue("version: 1\nname: probe\nsteps:\n  - { id: s1, image: img }\n", nil)
		f.claimRunning(run, 1, "ctr-x")
		_, stop := f.start()
		defer stop()
		f.wait(run.ID, store.RunFailed)
		if r := f.step(run.ID, "s1"); r.Error != "daemon unreachable" {
			t.Fatalf("%+v", r)
		}
	})
}

// A cancel or a shutdown during the retry back-off ends the step without
// another attempt.
func TestRetryBackoffInterrupted(t *testing.T) {
	setup := func(t *testing.T) (*fixture, *store.Run, *Engine, func()) {
		f := newFixture(t)
		f.cfg.RetryBase = time.Hour
		f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{ExitCode: 1} }
		run := f.enqueue("version: 1\nname: backoff\nsteps:\n  - { id: s1, image: img, retry: 3 }\n", nil)
		e, stop := f.start()
		f.waitStep(run.ID, "s1", func(r *store.StepRun) bool { return strings.Contains(r.Error, "retrying") })
		return f, run, e, stop
	}
	t.Run("cancel", func(t *testing.T) {
		f, run, e, stop := setup(t)
		defer stop()
		e.Cancel(run.ID)
		f.wait(run.ID, store.RunCancelled)
		if r := f.step(run.ID, "s1"); r.Status != store.StepCancelled || r.Attempt != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		f, run, _, stop := setup(t)
		stop()
		if got, _ := f.st.GetRun(f.ctx, run.ID); got.Status != store.RunRunning {
			t.Fatalf("run must stay running for re-attach: %+v", got)
		}
		if r := f.step(run.ID, "s1"); r.Status != store.StepRunning {
			t.Fatalf("%+v", r)
		}
	})
}

// A step waiting for a global slot gives up when the run is cancelled or
// the daemon stops.
func TestSlotWaitInterrupted(t *testing.T) {
	// Either step may win the only slot; the other one waits.
	setup := func(t *testing.T) (f *fixture, run *store.Run, e *Engine, stop func(), waiter string) {
		f = newFixture(t)
		f.faults(nil)
		f.cfg.Concurrency = 1
		f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
		run = f.enqueue("version: 1\nname: slots\nsteps:\n  - { id: s1, image: img }\n  - { id: s2, image: img }\n", nil)
		e, stop = f.start()
		deadline := time.Now().Add(10 * time.Second)
		for waiter == "" && time.Now().Before(deadline) {
			switch {
			case f.step(run.ID, "s1").Status == store.StepRunning:
				waiter = "s2"
			case f.step(run.ID, "s2").Status == store.StepRunning:
				waiter = "s1"
			}
			time.Sleep(5 * time.Millisecond)
		}
		if waiter == "" {
			t.Fatal("no step started")
		}
		return f, run, e, stop, waiter
	}
	t.Run("cancel", func(t *testing.T) {
		f, run, e, stop, waiter := setup(t)
		defer stop()
		e.Cancel(run.ID)
		f.wait(run.ID, store.RunCancelled)
		if r := f.step(run.ID, waiter); r.Status != store.StepCancelled {
			t.Fatalf("%+v", r)
		}
		for _, c := range f.containersForStep(waiter) {
			if c.Started {
				t.Fatal("a cancelled waiting step must not start a container")
			}
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		f, run, _, stop, waiter := setup(t)
		stop()
		if got, _ := f.st.GetRun(f.ctx, run.ID); got.Status != store.RunRunning {
			t.Fatalf("run must stay running for re-attach: %+v", got)
		}
		if r := f.step(run.ID, waiter); r.Status != store.StepPending {
			t.Fatalf("waiting step must stay pending: %+v", r)
		}
	})
}

// stopOnCall stops the engine while an action runs, then completes it.
type stopOnCall struct {
	stop func()
}

func (a *stopOnCall) Execute(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
	go a.stop()
	<-ctx.Done()
	return map[string]any{"ok": true}, nil
}

// When the daemon stops right after a step finishes, a run with pending
// steps is left for re-attach and a run with nothing left is finished.
func TestShutdownBetweenSteps(t *testing.T) {
	for _, pending := range []bool{true, false} {
		f := newFixture(t)
		acts := &stopOnCall{}
		f.deps.Actions = acts
		yaml := "version: 1\nname: between\nsteps:\n  - { id: s1, uses: slack.post-message }\n"
		if pending {
			yaml += "  - { id: s2, image: img, needs: [s1] }\n"
		}
		run := f.enqueue(yaml, nil)
		e := New(f.cfg, f.deps)
		ctx, cancel := context.WithCancel(f.ctx)
		done := make(chan struct{})
		acts.stop = func() { cancel(); <-done }
		go func() {
			defer close(done)
			_ = e.Run(ctx)
		}()
		<-ctx.Done()
		<-done
		got, _ := f.st.GetRun(f.ctx, run.ID)
		want := store.RunSucceeded
		if pending {
			want = store.RunRunning
		}
		if got.Status != want || f.step(run.ID, "s1").Status != store.StepSucceeded {
			t.Fatalf("pending=%v: run %s, want %s: %s", pending, got.Status, want, f.dump(run.ID))
		}
	}
}

type blockingSessions struct {
	stubSessions
	entered chan struct{}
}

func (s *blockingSessions) StartRun(ctx context.Context, _, _ string) (session.RunWorkspace, error) {
	close(s.entered)
	<-ctx.Done()
	return session.RunWorkspace{}, ctx.Err()
}

func TestShutdownDuringPrepare(t *testing.T) {
	f := newFixture(t)
	bs := &blockingSessions{entered: make(chan struct{})}
	f.deps.Sessions = bs
	run := f.enqueue("version: 1\nname: prep-stop\nsteps:\n  - { id: s1, image: img }\n", nil)
	_, stop := f.start()
	<-bs.entered
	stop()
	if got, _ := f.st.GetRun(f.ctx, run.ID); got.Status != store.RunRunning {
		t.Fatalf("run must stay running for the next daemon: %+v", got)
	}
}

func TestOutputsThatCannotBeEncoded(t *testing.T) {
	orig := jsonMarshal
	defer func() { jsonMarshal = orig }()
	jsonMarshal = func(any) ([]byte, error) { return nil, errors.New("unencodable") }
	if got := mustJSON(map[string]any{"a": 1}); got != "{}" {
		t.Fatalf("mustJSON = %q", got)
	}
}

func TestEngineDefaultsAndSignals(t *testing.T) {
	e := New(Config{}, Deps{})
	c := e.cfg
	if c.Concurrency != 1 || c.MaxRuns != 1 || c.StepTimeout != 30*time.Minute || c.OutputLimit != 1<<20 ||
		c.RetryBase != 2*time.Second || c.PollInterval != time.Second || c.Heartbeat != 5*time.Second || c.Profile != "workflow" {
		t.Fatalf("defaults: %+v", c)
	}
	if _, ok := e.d.Sessions.(*localSessions); !ok || e.d.Log == nil {
		t.Fatal("fallback collaborators")
	}
	e.Wake()
	e.Wake() // coalesced, never blocks
	if len(e.wake) != 1 {
		t.Fatal("wake must coalesce")
	}
	// Cancel never blocks the caller, even with a full queue.
	n := cap(e.cancel) + 2
	for i := 0; i < n; i++ {
		e.Cancel("r")
	}
	for i := 0; i < n; i++ {
		select {
		case <-e.cancel:
		case <-time.After(2 * time.Second):
			t.Fatalf("got %d of %d cancels", i, n)
		}
	}
	if _, err := StepInits(&workflow.Spec{Steps: []workflow.StepSpec{{ID: "a", Needs: []string{"a"}}}}); err == nil {
		t.Fatal("StepInits must reject a cycle")
	}
	if got := e.stepTimeout(&workflow.StepSpec{Timeout: "2h"}); got != 30*time.Minute {
		t.Fatalf("timeouts above the ceiling are capped: %s", got)
	}
}

func TestEngineStoreFailures(t *testing.T) {
	f := newFixture(t)
	st, err := store.Open(f.ctx, store.SQLite, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	f.deps.Store = st
	if err := New(f.cfg, f.deps).Run(f.ctx); err == nil {
		t.Fatal("Run must fail when it cannot list running runs")
	}

	// A claim error after start is logged and retried until shutdown.
	f2 := newFixture(t)
	_, stop := f2.start()
	time.Sleep(30 * time.Millisecond)
	_ = f2.st.Close()
	time.Sleep(60 * time.Millisecond)
	stop()
}

func TestLocalSessions(t *testing.T) {
	if got := (&localSessions{}).root(); got != filepath.Join(os.TempDir(), "icaro-runs") {
		t.Fatalf("default root %s", got)
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := (&localSessions{dir: file}).StartRun(context.Background(), "r1", "p"); err == nil {
		t.Fatal("StartRun under a file must fail")
	}
	dir := t.TempDir()
	l := &localSessions{dir: dir}
	ws, err := l.StartRun(context.Background(), "r1", "p")
	if err != nil {
		t.Fatal(err)
	}
	// Kept, foreign and suspicious session ids are left alone.
	for _, sid := range []string{"other-r1", localPrefix + "../x", localPrefix} {
		if err := l.FinishRun(context.Background(), sid, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.FinishRun(context.Background(), ws.SessionID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatal("kept workspace removed")
	}
	if err := l.FinishRun(context.Background(), ws.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r1")); !os.IsNotExist(err) {
		t.Fatal("workspace not removed")
	}
}

func TestLogCapture(t *testing.T) {
	f := newFixture(t)
	e := New(f.cfg, f.deps)
	run := f.enqueue("version: 1\nname: logcap\nsteps:\n  - { id: s1, image: img }\n", nil)
	rx := &runExec{e: e, run: run, redact: logs.NewRedactor([]string{"hunter2"})}
	row := &store.StepRun{StepID: "s1"}

	lc, err := e.openLog(f.ctx, rx, row, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", chunkBytes) + "\n"
	_, _ = lc.writer(true).Write([]byte(big))
	_, _ = lc.writer(false).Write([]byte("pass hunter2 no newline"))
	lc.close()
	lc.close() // idempotent
	b, _ := os.ReadFile(e.logPath(run.ID, "s1", 1))
	if !strings.HasSuffix(string(b), "pass *** no newline\n") || lc.lines != 2 {
		t.Fatalf("log tail %q lines %d", b[len(b)-30:], lc.lines)
	}
	c, err := f.st.LastLogChunk(f.ctx, run.ID, "s1", 1)
	if err != nil || c.FileOffset != int64(len(b)) || c.Seq < 2 {
		t.Fatalf("chunks %+v %v", c, err)
	}

	// Resuming truncates to the last indexed offset and skips the lines
	// already written when the stream is replayed.
	lc, err = e.openLog(f.ctx, rx, row, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if lc.offset != int64(len(b)) || lc.skip != 2 {
		t.Fatalf("resume state %+v", lc)
	}
	lc.close()

	// A directory in the way of the log file fails the attempt.
	if err := os.MkdirAll(e.logPath(run.ID, "s1", 2), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.openLog(f.ctx, rx, row, 2, false); err == nil {
		t.Fatal("openLog over a directory must fail")
	}

	// An unreadable log index fails a resume; failing to index is logged.
	broken, err := store.Open(f.ctx, store.SQLite, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.Close()
	e2 := New(f.cfg, Deps{Store: broken, Docker: f.docker, Events: f.bus})
	rx.e = e2
	if _, err := e2.openLog(f.ctx, rx, row, 3, true); err == nil {
		t.Fatal("resume must fail when the index cannot be read")
	}
	lc, err = e2.openLog(f.ctx, rx, row, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	lc.line("hello")
	lc.close()
}

func TestUntarFirst(t *testing.T) {
	build := func(entries ...*tar.Header) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			_ = tw.WriteHeader(h)
			_, _ = tw.Write(make([]byte, h.Size))
		}
		_ = tw.Close()
		return &buf
	}
	if _, err := untarFirst(build(), 10); err == nil || err.Error() != "empty archive" {
		t.Fatalf("empty: %v", err)
	}
	if _, err := untarFirst(strings.NewReader(strings.Repeat("z", 1024)), 10); err == nil {
		t.Fatal("garbage must fail")
	}
	got, err := untarFirst(build(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}, &tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644}), 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("directories are skipped: %q %v", got, err)
	}
	if _, err := untarFirst(build(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 11, Mode: 0o644}), 10); !errors.Is(err, errTooLarge) {
		t.Fatalf("too large: %v", err)
	}
}
