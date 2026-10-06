package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/runner/docker"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	store  *store.Store
	docker *docker.Fake
	runner *Runner
	logDir string
}

type fakeSecrets map[string]map[string]string

func (f fakeSecrets) ResolveConnection(_ context.Context, name string) (map[string]string, error) {
	if v, ok := f[name]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fd := docker.NewFake()
	logDir := t.TempDir()
	cfg := Config{
		RunnerID: "r1", Concurrency: 2, PollInterval: 10 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
		StepTimeout: 2 * time.Second, MemoryBytes: 1 << 30, NanoCPUs: 1e9, Pids: 256, NoFile: 4096, LogsDir: logDir,
	}
	secrets := fakeSecrets{"slack": {"token": "xoxb-super-secret"}} //nolint:gosec // test fixture
	r := New(cfg, st, fd, secrets, nil, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return &fixture{t: t, ctx: ctx, store: st, docker: fd, runner: r, logDir: logDir}
}

func (f *fixture) enqueue(yaml string, inputs map[string]any) *store.Run {
	f.t.Helper()
	wf, issues, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil || len(issues) > 0 {
		f.t.Fatalf("workflow: %v %v", err, issues)
	}
	w, err := f.store.ApplyWorkflow(f.ctx, wf.Name, yaml, "{}", "test")
	if err != nil {
		f.t.Fatal(err)
	}
	in, _ := json.Marshal(inputs)
	names := make([]string, len(wf.Steps))
	for i, s := range wf.Steps {
		names[i] = s.Name
	}
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version, TriggerKind: store.TriggerAPI, InputJSON: string(in)}
	if err := f.store.CreateRun(f.ctx, run, names); err != nil {
		f.t.Fatal(err)
	}
	return run
}

// claimAndRun executes the next queued run synchronously.
func (f *fixture) claimAndRun() *store.Run {
	f.t.Helper()
	run, err := f.store.ClaimRun(f.ctx, "r1")
	if err != nil {
		f.t.Fatal(err)
	}
	f.runner.execute(f.ctx, run, false)
	got, err := f.store.GetRun(f.ctx, run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *fixture) steps(runID string) []*store.RunStep {
	f.t.Helper()
	steps, err := f.store.GetRunSteps(f.ctx, runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return steps
}

const twoSteps = `
name: two
steps:
  - name: enrich
    run:
      image: python:3.12-slim
      script: |
        echo "hello {{ inputs.who }}"
    env: { GREETING: "hi-{{ inputs.who }}" }
    connection: slack
  - name: notify
    run:
      image: alpine
      command: ["sh", "-c", "echo {{ steps.enrich.outputs.plan }}"]
    if: '{{ eq steps.enrich.outputs.plan "pro" }}'
    sandbox: strict
`

func TestHappyPathPassesOutputsAndInjectsEnv(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		switch c.Spec.Labels[LabelStep] {
		case "0":
			return docker.Behavior{Stdout: "token=xoxb-super-secret\n", Output: []byte(`{"plan":"pro"}`)}
		default:
			return docker.Behavior{Stdout: "notified\n"}
		}
	}
	run := f.enqueue(twoSteps, map[string]any{"who": "icaro", "n": 2})
	got := f.claimAndRun()
	if got.Status != store.RunSucceeded || got.Error != "" {
		t.Fatalf("run: %+v", got)
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepSucceeded || steps[1].Status != store.StepSucceeded {
		t.Fatalf("steps: %+v %+v", steps[0], steps[1])
	}
	if steps[0].OutputJSON != `{"plan":"pro"}` || *steps[0].ExitCode != 0 || steps[0].Attempt != 1 {
		t.Fatalf("step0: %+v", steps[0])
	}

	c0 := f.docker.Container("ctr001")
	if c0 == nil || !c0.Removed {
		t.Fatal("container 0 should exist and be removed")
	}
	env := strings.Join(c0.Spec.Env, "\n")
	for _, want := range []string{"ICARO_INPUT_WHO=icaro", "ICARO_INPUT_N=2", "GREETING=hi-icaro", "ICARO_CONN_TOKEN=xoxb-super-secret", "ICARO_RUN_ID=" + run.ID, "ICARO_STEP=enrich"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q in:\n%s", want, env)
		}
	}
	if string(c0.Files["/icaro/script.sh"]) != "echo \"hello icaro\"\n" {
		t.Fatalf("script: %q", c0.Files["/icaro/script.sh"])
	}
	if string(c0.Files["/icaro/input.json"]) != `{"n":2,"who":"icaro"}` {
		t.Fatalf("input.json: %s", c0.Files["/icaro/input.json"])
	}
	if c0.Spec.Entrypoint[0] != "sh" || c0.Spec.Entrypoint[1] != "-e" || c0.Spec.Entrypoint[2] != "/icaro/script.sh" {
		t.Fatalf("entrypoint: %v", c0.Spec.Entrypoint)
	}
	hc := c0.Spec.HostConfig
	if hc.CapDrop[0] != "ALL" || !hc.ReadonlyRootfs || string(hc.NetworkMode) != "bridge" || hc.Privileged {
		t.Fatalf("hostconfig: %+v", hc)
	}
	if len(hc.Mounts) != 2 || hc.Mounts[0].Source != "icaro-ws-"+strings.ToLower(run.ID) {
		t.Fatalf("mounts: %+v", hc.Mounts)
	}

	c1 := f.docker.Container("ctr002")
	if strings.Join(c1.Spec.Entrypoint, " ") != "sh -c echo pro" {
		t.Fatalf("templated command: %v", c1.Spec.Entrypoint)
	}
	if string(c1.Spec.HostConfig.NetworkMode) != "none" || *c1.Spec.HostConfig.PidsLimit != 64 {
		t.Fatalf("strict sandbox: %+v", c1.Spec.HostConfig)
	}
	var ctxData map[string]any
	_ = json.Unmarshal(c1.Files["/icaro/context.json"], &ctxData)
	if ctxData["steps"].(map[string]any)["enrich"].(map[string]any)["outputs"].(map[string]any)["plan"] != "pro" {
		t.Fatalf("context.json: %s", c1.Files["/icaro/context.json"])
	}

	logb, err := os.ReadFile(steps[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logb) != "token=***\n" || steps[0].LogSize != int64(len(logb)) {
		t.Fatalf("log scrubbing: %q size=%d", logb, steps[0].LogSize)
	}
	if _, ok := f.docker.Volumes["icaro-ws-"+strings.ToLower(run.ID)]; ok {
		t.Fatal("volumes should be removed after success")
	}
}

func TestSkipAndContinueOnError(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "0" {
			return docker.Behavior{ExitCode: 3, Output: []byte(`{"plan":"free"}`)}
		}
		return docker.Behavior{}
	}
	yaml := `
name: skip
steps:
  - name: enrich
    run: { image: alpine, script: "exit 3" }
    continue_on_error: true
  - name: notify
    run: { image: alpine, script: "echo" }
    if: '{{ eq steps.enrich.outputs.plan "pro" }}'
  - name: always
    run: { image: alpine, script: "echo" }
`
	run := f.enqueue(yaml, nil)
	got := f.claimAndRun()
	if got.Status != store.RunSucceeded {
		t.Fatalf("run: %+v", got)
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepFailed || steps[0].Error != "exit code 3" || *steps[0].ExitCode != 3 {
		t.Fatalf("step0: %+v", steps[0])
	}
	if steps[1].Status != store.StepSkipped || steps[2].Status != store.StepSucceeded {
		t.Fatalf("steps: %+v %+v", steps[1], steps[2])
	}
}

func TestRetryThenSucceed(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		calls++
		if calls == 1 {
			return docker.Behavior{ExitCode: 1, Stderr: "flaky\n"}
		}
		return docker.Behavior{Output: []byte(`{"ok":true}`)}
	}
	yaml := `
name: retry
steps:
  - name: flaky
    run: { image: alpine, script: "maybe" }
    retry: { attempts: 3, backoff: 1ms }
`
	run := f.enqueue(yaml, nil)
	got := f.claimAndRun()
	if got.Status != store.RunSucceeded {
		t.Fatalf("run: %+v", got)
	}
	st := f.steps(run.ID)[0]
	if st.Status != store.StepSucceeded || st.Attempt != 2 || st.OutputJSON != `{"ok":true}` || calls != 2 {
		t.Fatalf("step: %+v calls=%d", st, calls)
	}
	if f.docker.Container("ctr001").Spec.Labels[LabelAttempt] != "1" || f.docker.Container("ctr002").Spec.Labels[LabelAttempt] != "2" {
		t.Fatal("attempt labels")
	}
}

func TestFailureStopsRunAndKeepsNothing(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{ExitCode: 2} }
	yaml := `
name: fail
steps:
  - name: a
    run: { image: alpine, script: "false" }
  - name: b
    run: { image: alpine, script: "true" }
`
	run := f.enqueue(yaml, nil)
	got := f.claimAndRun()
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "step a: exit code 2") {
		t.Fatalf("run: %+v", got)
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepFailed || steps[1].Status != store.StepPending {
		t.Fatalf("steps: %+v %+v", steps[0], steps[1])
	}
	if len(f.docker.Volumes) != 0 {
		t.Fatal("volumes removed on failure by default")
	}
}

func TestTimeoutKillsContainer(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: 5 * time.Second} }
	yaml := `
name: slow
steps:
  - name: a
    run: { image: alpine, script: "sleep 100" }
    timeout: 50ms
`
	run := f.enqueue(yaml, nil)
	got := f.claimAndRun()
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "timed out after 50ms") {
		t.Fatalf("run: %+v", got)
	}
	if !f.docker.Container("ctr001").Killed {
		t.Fatal("container should be killed")
	}
	if st := f.steps(run.ID)[0]; st.Status != store.StepFailed || *st.ExitCode != -1 {
		t.Fatalf("step: %+v", st)
	}
}

func TestCancelRequestStopsRun(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: 5 * time.Second} }
	yaml := `
name: cancel
steps:
  - name: a
    run: { image: alpine, script: "sleep 100" }
  - name: b
    run: { image: alpine, script: "true" }
`
	run := f.enqueue(yaml, nil)
	claimed, err := f.store.ClaimRun(f.ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.runner.execute(f.ctx, claimed, false); close(done) }()
	// Wait for the container to start, then request cancellation via the store.
	deadline := time.Now().Add(2 * time.Second)
	for !f.docker.IsStarted("ctr001") {
		if time.Now().After(deadline) {
			t.Fatal("container never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := f.store.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop")
	}
	got, _ := f.store.GetRun(f.ctx, run.ID)
	if got.Status != store.RunCancelled {
		t.Fatalf("run: %+v", got)
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepFailed || steps[0].Error != "cancelled" || steps[1].Status != store.StepPending {
		t.Fatalf("steps: %+v %+v", steps[0], steps[1])
	}
	if !f.docker.Container("ctr001").Killed {
		t.Fatal("container should be killed")
	}
}

func TestReconcileReattachesAndCompletes(t *testing.T) {
	f := newFixture(t)
	yaml := `
name: resume
steps:
  - name: a
    run: { image: alpine, script: "long" }
  - name: b
    run: { image: alpine, script: "echo" }
`
	run := f.enqueue(yaml, map[string]any{"x": 1})
	claimed, err := f.store.ClaimRun(f.ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a previous process that created and started the container,
	// persisted the row, then died.
	spec := docker.Spec{Image: "alpine", Labels: map[string]string{LabelRun: run.ID, LabelStep: "0", LabelAttempt: "1"}}
	f.docker.AddContainer("old-ctr", spec, docker.Behavior{Duration: 100 * time.Millisecond, Stdout: "resumed output\n", Output: []byte(`{"from":"old"}`)}, true)
	row, _ := f.store.GetRunStep(f.ctx, run.ID, 0)
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.StartedAt, row.LogPath = store.StepRunning, 1, "old-ctr", &now, f.runner.logs.Path(run.ID, 0)
	if err := f.store.UpdateRunStep(f.ctx, row); err != nil {
		t.Fatal(err)
	}
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Stdout: "b ran\n"} }

	if err := f.runner.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, _ := f.store.GetRun(f.ctx, claimed.ID)
		if got.Status != store.RunRunning {
			if got.Status != store.RunSucceeded {
				t.Fatalf("run: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepSucceeded || steps[0].OutputJSON != `{"from":"old"}` || steps[0].ContainerID != "old-ctr" {
		t.Fatalf("step0: %+v", steps[0])
	}
	if steps[1].Status != store.StepSucceeded {
		t.Fatalf("step1: %+v", steps[1])
	}
	logb, _ := os.ReadFile(steps[0].LogPath)
	if string(logb) != "resumed output\n" {
		t.Fatalf("resumed log: %q", logb)
	}
	// No new container was created for step a.
	if f.docker.Container("ctr001").Spec.Labels[LabelStep] != "1" {
		t.Fatal("step a should not have been re-run")
	}
}

func TestReconcileMissingContainerFails(t *testing.T) {
	f := newFixture(t)
	yaml := `
name: gone
steps:
  - name: a
    run: { image: alpine, script: "long" }
`
	run := f.enqueue(yaml, nil)
	if _, err := f.store.ClaimRun(f.ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	row, _ := f.store.GetRunStep(f.ctx, run.ID, 0)
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.StartedAt = store.StepRunning, 1, "vanished", &now
	_ = f.store.UpdateRunStep(f.ctx, row)
	if err := f.runner.Reconcile(f.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, _ := f.store.GetRun(f.ctx, run.ID)
		if got.Status == store.RunFailed {
			if !strings.Contains(got.Error, "disappeared") {
				t.Fatalf("error: %s", got.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not fail: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunLoopClaimsAndExecutes(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Output: []byte(`{"ok":1}`)} }
	yaml := `
name: loop
steps:
  - name: a
    run: { image: alpine, script: "echo" }
`
	r1 := f.enqueue(yaml, nil)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { _ = f.runner.Run(ctx); close(done) }()
	r2 := f.enqueue(yaml, nil)
	f.runner.Wake()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a, _ := f.store.GetRun(f.ctx, r1.ID)
		b, _ := f.store.GetRun(f.ctx, r2.ID)
		if a.Status == store.RunSucceeded && b.Status == store.RunSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs did not complete: %s %s", a.Status, b.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}
