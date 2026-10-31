package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/expr"
	"icaro/internal/serial"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	docker *docker.Fake
	deps   Deps
	cfg    Config
	bus    *events.Bus
}

func newFixture(t *testing.T) *fixture {
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
	f := &fixture{t: t, ctx: ctx, st: st, docker: docker.NewFake(), bus: events.New(ctx)}
	f.cfg = Config{
		Concurrency: 4, StepTimeout: time.Minute, LogsDir: t.TempDir(), RunsDir: t.TempDir(),
		RetryBase: time.Millisecond, PollInterval: 20 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
	}
	f.deps = Deps{Store: st, Docker: f.docker, Events: f.bus}
	return f
}

// enqueue stores the workflow and a queued run with trigger payload.
func (f *fixture) enqueue(yaml string, trigger map[string]any) *store.Run {
	f.t.Helper()
	spec, issues, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil || len(issues) > 0 {
		f.t.Fatalf("workflow: %v %v", err, issues)
	}
	w, err := f.st.ApplyWorkflow(f.ctx, spec.Name, yaml, "{}", "test")
	if err != nil {
		f.t.Fatal(err)
	}
	steps, err := StepInits(spec)
	if err != nil {
		f.t.Fatal(err)
	}
	tj, _ := json.Marshal(trigger)
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: w.Version, TriggerKind: "manual", TriggerJSON: string(tj)}
	if err := f.st.CreateRun(f.ctx, run, steps); err != nil {
		f.t.Fatal(err)
	}
	return run
}

// start runs an engine until the returned stop function is called.
func (f *fixture) start() (*Engine, func()) {
	f.t.Helper()
	e := New(f.cfg, f.deps)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Run(ctx); err != nil {
			f.t.Errorf("engine: %v", err)
		}
	}()
	return e, func() { cancel(); <-done }
}

func (f *fixture) wait(runID string, statuses ...string) *store.Run {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := f.st.GetRun(f.ctx, runID)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, s := range statuses {
			if r.Status == s {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := f.st.GetRun(f.ctx, runID)
	f.t.Fatalf("run %s stuck in %s (want %v): %s", runID, r.Status, statuses, f.dump(runID))
	return nil
}

func (f *fixture) dump(runID string) string {
	rows, _ := f.st.GetStepRuns(f.ctx, runID)
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString("\n  " + r.StepID + " " + r.Status + " " + r.Error)
	}
	return sb.String()
}

func (f *fixture) step(runID, id string) *store.StepRun {
	f.t.Helper()
	r, err := f.st.GetStepRun(f.ctx, runID, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) outputs(runID, id string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(f.step(runID, id).OutputsJSON), &out)
	return out
}

func (f *fixture) containersForStep(stepID string) []*docker.FakeContainer {
	var out []*docker.FakeContainer
	for _, id := range sortedIDs(f.docker) {
		c := f.docker.Container(id)
		if c.Spec.Labels[LabelStep] == stepID {
			out = append(out, c)
		}
	}
	return out
}

// sortedIDs lists every container the fake created, removed ones included
// (the fake numbers them ctr001, ctr002, ...).
func sortedIDs(d *docker.Fake) []string {
	var all []string
	for i := 1; ; i++ {
		id := fmt.Sprintf("ctr%03d", i)
		if d.Container(id) == nil {
			return all
		}
		all = append(all, id)
	}
}

const dataFlowYAML = `
version: 1
name: data-flow
env:
  GREETING: hello
steps:
  - id: greet
    image: img
    command: echo ${{ env.GREETING }} ${{ trigger.who }}
    input: { who: "${{ trigger.who }}", n: 3 }
    outputs:
      message: json:message
      out: stdout
      last: stdout:last-line
  - id: shout
    image: img
    needs: [greet]
    command: echo '${{ steps.greet.outputs.message }}' ${{ steps.greet.outputs.last }}
`

func TestDataFlowAndIOContract(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "greet" {
			return docker.Behavior{Output: []byte(`{"message":"hi icaro"}`), Stdout: "line one\nline two\n"}
		}
		return docker.Behavior{Stdout: "done\n"}
	}
	run := f.enqueue(dataFlowYAML, map[string]any{"who": "icaro"})
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("run %s: %s %s", got.Status, got.Error, f.dump(run.ID))
	}
	outs := f.outputs(run.ID, "greet")
	if outs["message"] != "hi icaro" || outs["out"] != "line one\nline two" || outs["last"] != "line two" {
		t.Fatalf("outputs %v", outs)
	}
	greet := f.containersForStep("greet")[0]
	if strings.Join(greet.Spec.Cmd, " ") != "echo hello icaro" || greet.Spec.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("command %v %v", greet.Spec.Entrypoint, greet.Spec.Cmd)
	}
	var input map[string]any
	_ = json.Unmarshal(greet.Files["/icaro/input.json"], &input)
	if input["who"] != "icaro" || input["n"] != 3.0 {
		t.Fatalf("input.json %s", greet.Files["/icaro/input.json"])
	}
	env := strings.Join(greet.Spec.Env, "\n")
	for _, want := range []string{"ICARO_INPUT_WHO=icaro", "ICARO_INPUT_N=3", "GREETING=hello", "ICARO_STEP=greet", "ICARO_ATTEMPT=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %s:\n%s", want, env)
		}
	}
	hc := greet.Spec.HostConfig
	if hc.NetworkMode != "none" || !hc.ReadonlyRootfs || hc.CapDrop[0] != "ALL" {
		t.Fatalf("baseline not applied: %+v", hc)
	}
	if hc.Mounts[0].Type != "bind" || hc.Mounts[0].Target != "/workspace" || !strings.Contains(hc.Mounts[0].Source, run.ID) {
		t.Fatalf("workspace mount %+v", hc.Mounts[0])
	}
	if greet.Spec.Labels[LabelRun] != run.ID || greet.Spec.Labels[LabelAttempt] != "1" || !greet.Removed {
		t.Fatalf("labels/cleanup %+v removed=%v", greet.Spec.Labels, greet.Removed)
	}
	shout := f.containersForStep("shout")[0]
	if strings.Join(shout.Spec.Cmd, " ") != "echo 'hi icaro' line two" {
		t.Fatalf("interpolated command %v", shout.Spec.Cmd)
	}
	// Logs are on disk and indexed.
	row := f.step(run.ID, "greet")
	b, err := os.ReadFile(row.LogPath)
	if err != nil || string(b) != "line one\nline two\n" || row.LogLines != 2 || row.LogSize != int64(len(b)) {
		t.Fatalf("log %q %v %+v", b, err, row)
	}
	if c, err := f.st.LastLogChunk(f.ctx, run.ID, "greet", 1); err != nil || c.FileOffset != int64(len(b)) {
		t.Fatalf("chunk %+v %v", c, err)
	}
	// The local workspace is removed after a successful run.
	if _, err := os.Stat(filepath.Join(f.cfg.RunsDir, run.ID)); !os.IsNotExist(err) {
		t.Fatalf("workspace not cleaned: %v", err)
	}
}

func TestIndependentStepsRunInParallel(t *testing.T) {
	f := newFixture(t)
	// The start times live in a one-slot channel; a receiver owns them
	// until it sends them back.
	startsCh := make(chan map[string]time.Time, 1)
	startsCh <- map[string]time.Time{}
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		starts := <-startsCh
		starts[c.Spec.Labels[LabelStep]] = time.Now()
		startsCh <- starts
		return docker.Behavior{Duration: 400 * time.Millisecond}
	}
	run := f.enqueue(`
version: 1
name: parallel
steps:
  - { id: left, image: img }
  - { id: right, image: img }
  - { id: join, image: img, needs: [left, right] }
`, nil)
	begin := time.Now()
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	starts := <-startsCh
	gap := starts["left"].Sub(starts["right"]).Abs()
	if gap > 200*time.Millisecond {
		t.Fatalf("independent steps did not overlap: started %s apart", gap)
	}
	if !starts["join"].After(starts["left"].Add(350*time.Millisecond)) || time.Since(begin) > 2*time.Second {
		t.Fatalf("join must wait for both: %v", starts)
	}
}

func TestRunParallelismCap(t *testing.T) {
	f := newFixture(t)
	f.cfg.RunParallelism = 1
	startsCh := make(chan map[string]time.Time, 1)
	startsCh <- map[string]time.Time{}
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		starts := <-startsCh
		starts[c.Spec.Labels[LabelStep]] = time.Now()
		startsCh <- starts
		return docker.Behavior{Duration: 200 * time.Millisecond}
	}
	run := f.enqueue("version: 1\nname: serial\nsteps:\n  - { id: s1, image: img }\n  - { id: s2, image: img }\n", nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if starts := <-startsCh; starts["s1"].Sub(starts["s2"]).Abs() < 150*time.Millisecond {
		t.Fatal("run parallelism 1 must serialize steps")
	}
}

func TestWhenFailureAndSkipPropagation(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "broken" {
			return docker.Behavior{ExitCode: 3, Stderr: "boom\n"}
		}
		return docker.Behavior{}
	}
	run := f.enqueue(`
version: 1
name: gating
steps:
  - id: gated
    image: img
    when: trigger.deploy == true
  - id: after-gated
    image: img
    needs: [gated]
    when: steps.gated.status == 'skipped'
  - id: broken
    image: img
  - id: after-broken
    image: img
    needs: [broken]
  - id: after-after
    image: img
    needs: [after-broken]
`, map[string]any{"deploy": false})
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "step broken failed: exit code 3") {
		t.Fatalf("run: %+v", got)
	}
	checks := map[string][2]string{
		"gated":        {store.StepSkipped, ""},
		"after-gated":  {store.StepSucceeded, ""},
		"broken":       {store.StepFailed, "exit code 3"},
		"after-broken": {store.StepSkipped, "not run: needs broken failed"},
		"after-after":  {store.StepSkipped, "not run: needs after-broken skipped"},
	}
	for id, want := range checks {
		r := f.step(run.ID, id)
		if r.Status != want[0] || r.Error != want[1] {
			t.Fatalf("%s: %s %q, want %v", id, r.Status, r.Error, want)
		}
	}
	if len(f.containersForStep("gated")) != 0 || len(f.containersForStep("after-broken")) != 0 {
		t.Fatal("skipped steps must not start containers")
	}
	if b, _ := os.ReadFile(f.step(run.ID, "broken").LogPath); string(b) != "boom\n" {
		t.Fatalf("stderr log %q", b)
	}
	// A failed run keeps nothing by default.
	if _, err := os.Stat(filepath.Join(f.cfg.RunsDir, run.ID)); !os.IsNotExist(err) {
		t.Fatal("workspace not cleaned")
	}
}

func TestWhenErrorFailsStep(t *testing.T) {
	f := newFixture(t)
	run := f.enqueue("version: 1\nname: when-err\nsteps:\n  - { id: s1, image: img, when: \"trigger.missing == 1\" }\n", nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if r := f.step(run.ID, "s1"); r.Status != store.StepFailed || !strings.Contains(r.Error, "trigger.missing is not defined") {
		t.Fatalf("%+v", r)
	}
}

func TestRetryAndTimeout(t *testing.T) {
	f := newFixture(t)
	attemptsCh := make(chan map[string]int, 1)
	attemptsCh <- map[string]int{}
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		attempts := <-attemptsCh
		defer func() { attemptsCh <- attempts }()
		step := c.Spec.Labels[LabelStep]
		attempts[step]++
		switch {
		case step == "flaky" && attempts[step] < 3:
			return docker.Behavior{ExitCode: 1}
		case step == "slow":
			return docker.Behavior{Duration: time.Hour}
		}
		return docker.Behavior{}
	}
	run := f.enqueue(`
version: 1
name: retries
steps:
  - { id: flaky, image: img, retry: 2 }
  - { id: slow, image: img, timeout: 100ms, retry: 1 }
`, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if r := f.step(run.ID, "flaky"); r.Status != store.StepSucceeded || r.Attempt != 3 {
		t.Fatalf("flaky: %+v", r)
	}
	slow := f.step(run.ID, "slow")
	if slow.Status != store.StepFailed || slow.Attempt != 2 || !strings.Contains(slow.Error, "timed out after 100ms") {
		t.Fatalf("slow: %+v", slow)
	}
	for _, c := range f.containersForStep("slow") {
		if !c.Killed {
			t.Fatal("timed-out container must be killed")
		}
	}
}

func TestCancelKillsContainers(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
	run := f.enqueue(`
version: 1
name: cancel-me
steps:
  - { id: long, image: img }
  - { id: next, image: img, needs: [long] }
`, nil)
	e, stop := f.start()
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for f.step(run.ID, "long").Status != store.StepRunning && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	e.Cancel(run.ID)
	got := f.wait(run.ID, store.RunCancelled)
	if got.Error != "cancelled" {
		t.Fatalf("%+v", got)
	}
	if c := f.containersForStep("long")[0]; !c.Killed || !c.Removed {
		t.Fatalf("container not killed/removed: %+v", c)
	}
	if r := f.step(run.ID, "long"); r.Status != store.StepCancelled {
		t.Fatalf("long: %+v", r)
	}
	if r := f.step(run.ID, "next"); r.Status != store.StepCancelled {
		t.Fatalf("next: %+v", r)
	}
}

// A cancel request recorded in the store (by another API client) is
// picked up by the heartbeat.
func TestPersistedCancelRequest(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
	run := f.enqueue("version: 1\nname: cancel-db\nsteps:\n  - { id: long, image: img }\n", nil)
	_, stop := f.start()
	defer stop()
	for f.step(run.ID, "long").Status != store.StepRunning {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	f.wait(run.ID, store.RunCancelled)
}

// The acceptance criterion: a restart never leaves a run ambiguous. The
// daemon stops mid-step; the container keeps running; the next daemon
// re-attaches to it (no second container) and finishes the run.
func TestRestartReattachesToRunningContainer(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "long" {
			return docker.Behavior{Duration: 600 * time.Millisecond, Stdout: "a\nb\n", Output: []byte(`{"v":"done"}`)}
		}
		return docker.Behavior{}
	}
	run := f.enqueue(`
version: 1
name: survive
steps:
  - id: long
    image: img
    outputs: { v: json:v }
  - id: after
    image: img
    needs: [long]
    command: echo ${{ steps.long.outputs.v }}
`, nil)
	_, stop := f.start()
	for f.step(run.ID, "long").Status != store.StepRunning {
		time.Sleep(10 * time.Millisecond)
	}
	stop() // daemon shutdown
	r, _ := f.st.GetRun(f.ctx, run.ID)
	long := f.step(run.ID, "long")
	if r.Status != store.RunRunning || long.Status != store.StepRunning || long.ContainerID == "" {
		t.Fatalf("after shutdown: run %s step %+v", r.Status, long)
	}
	if c := f.docker.Container(long.ContainerID); c.Killed || c.Removed {
		t.Fatal("shutdown must leave the step container running")
	}

	_, stop2 := f.start()
	defer stop2()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("after re-attach: %+v %s", got, f.dump(run.ID))
	}
	if n := len(f.containersForStep("long")); n != 1 {
		t.Fatalf("re-attach must not start a second container, got %d", n)
	}
	if after := f.containersForStep("after")[0]; strings.Join(after.Spec.Cmd, " ") != "echo done" {
		t.Fatalf("outputs after re-attach: %v", after.Spec.Cmd)
	}
	if b, _ := os.ReadFile(f.step(run.ID, "long").LogPath); string(b) != "a\nb\n" {
		t.Fatalf("log after re-attach %q", b)
	}
}

// If the container is gone after a restart, the attempt fails and the
// retry policy decides; the run never stays "running".
func TestRestartWithMissingContainer(t *testing.T) {
	for _, retry := range []int{0, 1} {
		f := newFixture(t)
		yaml := "version: 1\nname: gone\nsteps:\n  - { id: step-a, image: img, retry: " + strconv.Itoa(retry) + " }\n"
		run := f.enqueue(yaml, nil)
		// Simulate the previous daemon: run claimed, step running with a
		// container id the engine no longer has.
		if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
			t.Fatal(err)
		}
		row := f.step(run.ID, "step-a")
		now := time.Now()
		row.Status, row.Attempt, row.ContainerID, row.StartedAt = store.StepRunning, 1, "vanished", &now
		if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
			t.Fatal(err)
		}
		_, stop := f.start()
		got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
		stop()
		r := f.step(run.ID, "step-a")
		if retry == 0 && (got.Status != store.RunFailed || !strings.Contains(r.Error, "disappeared")) {
			t.Fatalf("no retry: %+v %+v", got, r)
		}
		if retry == 1 && (got.Status != store.RunSucceeded || r.Attempt != 2) {
			t.Fatalf("retry: %+v %+v", got, r)
		}
	}
}

// A container created but never started (crash between persist and
// start) is not mistaken for a finished one.
func TestRestartWithNeverStartedContainer(t *testing.T) {
	f := newFixture(t)
	run := f.enqueue("version: 1\nname: unstarted\nsteps:\n  - { id: step-a, image: img }\n", nil)
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	id, _ := f.docker.CreateContainer(f.ctx, docker.Spec{Labels: map[string]string{LabelRun: run.ID, LabelStep: "step-a", LabelAttempt: "1"}})
	row := f.step(run.ID, "step-a")
	now := time.Now()
	row.Status, row.Attempt, row.ContainerID, row.StartedAt = store.StepRunning, 1, id, &now
	_ = f.st.UpdateStepRun(f.ctx, row)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if r := f.step(run.ID, "step-a"); !strings.Contains(r.Error, "before the container started") {
		t.Fatalf("%+v", r)
	}
	if !f.docker.Container(id).Removed {
		t.Fatal("orphan container must be removed")
	}
}

type fakeActions struct {
	ops   serial.Queue
	calls []map[string]any
	fail  bool
}

func newFakeActions() *fakeActions { return &fakeActions{ops: serial.New()} }

func (a *fakeActions) Execute(_ context.Context, ref string, input map[string]any) (out map[string]any, err error) {
	a.ops.Do(func() {
		a.calls = append(a.calls, input)
		if a.fail {
			err = errors.New("slack said no")
			return
		}
		out = map[string]any{"ts": "123.45", "ref": ref}
	})
	return out, err
}

func TestUsesSteps(t *testing.T) {
	f := newFixture(t)
	acts := newFakeActions()
	f.deps.Actions = acts
	run := f.enqueue(`
version: 1
name: notify
steps:
  - id: post
    uses: slack.post-message
    input: { channel: "#ci", text: "PR ${{ trigger.pr }}", count: "${{ trigger.pr }}" }
  - id: echo
    image: img
    needs: [post]
    command: echo ${{ steps.post.outputs.ts }}
`, map[string]any{"pr": 7.0})
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if len(acts.calls) != 1 || acts.calls[0]["text"] != "PR 7" || acts.calls[0]["count"] != 7.0 {
		t.Fatalf("calls %v", acts.calls)
	}
	if strings.Join(f.containersForStep("echo")[0].Spec.Cmd, " ") != "echo 123.45" {
		t.Fatal("uses outputs not interpolated")
	}
	if b, _ := os.ReadFile(f.step(run.ID, "post").LogPath); !strings.Contains(string(b), "uses slack.post-message") {
		t.Fatalf("uses log %q", b)
	}

	// Without an executor the step fails clearly.
	f2 := newFixture(t)
	run2 := f2.enqueue("version: 1\nname: no-exec\nsteps:\n  - { id: post, uses: slack.post-message }\n", nil)
	_, stop2 := f2.start()
	defer stop2()
	f2.wait(run2.ID, store.RunFailed)
	if r := f2.step(run2.ID, "post"); !strings.Contains(r.Error, "no integration action executor") {
		t.Fatalf("%+v", r)
	}
}

type fakeSecrets map[string]string

func (s fakeSecrets) Resolve(_ context.Context, n string) (string, error) {
	v, ok := s[n]
	if !ok {
		return "", errors.New("no such secret " + n)
	}
	return v, nil
}

func (s fakeSecrets) Names(context.Context) ([]string, error) {
	var out []string
	for k := range s {
		out = append(out, k)
	}
	return out, nil
}

func (s fakeSecrets) Hosts(context.Context, string) ([]string, error) { return nil, nil }

// boundSecrets adds host bindings (integration credentials).
type boundSecrets struct {
	fakeSecrets
	hosts map[string][]string
}

func (s boundSecrets) Hosts(_ context.Context, n string) ([]string, error) { return s.hosts[n], nil }

type fakeCatalog struct{ workflow.Catalog }

func (fakeCatalog) Integrations() []string { return []string{"github"} }

// Integration credentials never reach a template or a container, even
// when the workflow was saved before the secret was bound.
func TestIntegrationCredentialsAreRefused(t *testing.T) {
	cases := map[string]string{
		"bound":   "deploy_key",
		"by name": "github.webhook_secret",
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.deps.Secrets = boundSecrets{
				fakeSecrets: fakeSecrets{"deploy_key": "k3y-value-123", "github.webhook_secret": "wh-secret-456"},
				hosts:       map[string][]string{"deploy_key": {"api.example.com"}},
			}
			f.deps.Catalog = fakeCatalog{}
			run := f.enqueue("version: 1\nname: creds\nsteps:\n  - id: s1\n    image: img\n    env: { K: \"${{ secrets['"+secret+"'] }}\" }\n", nil)
			_, stop := f.start()
			defer stop()
			got := f.wait(run.ID, store.RunFailed)
			if !strings.Contains(got.Error, "integration credential") || !strings.Contains(got.Error, "uses:") {
				t.Fatalf("run error %q", got.Error)
			}
			if len(f.containersForStep("s1")) != 0 {
				t.Fatal("no step may start")
			}
		})
	}
}

// echoActions returns its text input in nested outputs, the way an
// integration response may echo a credential back.
type echoActions struct{}

func (echoActions) Execute(_ context.Context, _ string, input map[string]any) (map[string]any, error) {
	return map[string]any{"echo": input["text"], "nested": map[string]any{"list": []any{input["text"], 1.0}}}, nil
}

func TestSecretsAreInjectedAndRedacted(t *testing.T) {
	f := newFixture(t)
	f.deps.Secrets = fakeSecrets{"api_token": "s3cr3t-value", "other": "another-secret"}
	f.deps.Actions = echoActions{}
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Stdout: "token=s3cr3t-value other=another-secret\n", Output: []byte(`{"leak":"s3cr3t-value","deep":{"items":[{"v":"x another-secret y"}],"another-secret":true,"n":2}}`)}
	}
	run := f.enqueue(`
version: 1
name: secrets
env:
  TOKEN: ${{ secrets.api_token }}
steps:
  - id: use
    image: img
    outputs: { out: stdout, leak: json:leak, deep: json:deep, item: json:deep.items.0.v, doc: json:$ }
  - id: call
    uses: slack.post-message
    input: { text: "${{ secrets.api_token }}" }
`, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	c := f.containersForStep("use")[0]
	if !strings.Contains(strings.Join(c.Spec.Env, "\n"), "TOKEN=s3cr3t-value") {
		t.Fatal("secret not injected")
	}
	b, _ := os.ReadFile(f.step(run.ID, "use").LogPath)
	if strings.Contains(string(b), "s3cr3t") || strings.Contains(string(b), "another-secret") || !strings.Contains(string(b), "token=***") {
		t.Fatalf("log not redacted: %q", b)
	}
	outs := f.outputs(run.ID, "use")
	if out := outs["out"]; out != "token=*** other=***" {
		t.Fatalf("stdout capture not redacted: %v", out)
	}
	if outs["leak"] != "***" || outs["item"] != "x *** y" {
		t.Fatalf("json capture not redacted: %v", outs)
	}
	for _, step := range []string{"use", "call"} {
		raw := f.step(run.ID, step).OutputsJSON
		if strings.Contains(raw, "s3cr3t") || strings.Contains(raw, "another-secret") {
			t.Fatalf("%s outputs leak a secret: %s", step, raw)
		}
	}
	if call := f.outputs(run.ID, "call"); call["echo"] != "***" {
		t.Fatalf("uses outputs not redacted: %v", call)
	}
}

func TestFileCaptureAndOutputErrors(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		dir := c.Spec.HostConfig.Mounts[0].Source
		switch c.Spec.Labels[LabelStep] {
		case "writes":
			_ = os.MkdirAll(filepath.Join(dir, "out"), 0o750)
			_ = os.WriteFile(filepath.Join(dir, "out", "r.txt"), []byte("report\n"), 0o600)
			_ = os.Symlink("/etc/passwd", filepath.Join(dir, "escape"))
		case "bad-json":
			return docker.Behavior{Output: []byte(`[1,2]`)}
		case "big":
			return docker.Behavior{Output: []byte(`{"x":"` + strings.Repeat("a", 2<<20) + `"}`)}
		}
		return docker.Behavior{}
	}
	run := f.enqueue(`
version: 1
name: files
steps:
  - id: writes
    image: img
    outputs: { report: file:out/r.txt }
  - id: escapes
    image: img
    needs: [writes]
    outputs: { x: file:escape }
  - id: missing-key
    image: img
    outputs: { x: json:nope }
  - id: bad-json
    image: img
  - id: big
    image: img
`, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunFailed)
	if v := f.outputs(run.ID, "writes")["report"]; v != "report" {
		t.Fatalf("file capture %v %s", v, f.dump(run.ID))
	}
	want := map[string]string{"escapes": "outside /workspace", "missing-key": "output.json has no nope", "bad-json": "not a JSON object", "big": "exceeds"}
	for id, msg := range want {
		if r := f.step(run.ID, id); r.Status != store.StepFailed || !strings.Contains(r.Error, msg) {
			t.Fatalf("%s: %+v", id, r)
		}
	}
}

type stubEgress struct{ profile string }

func (s *stubEgress) Ensure(_ context.Context, profile string, allow []string) (container.Attachment, error) {
	s.profile = profile
	return container.Attachment{Network: "icaro-egress-" + profile, Env: []string{"HTTPS_PROXY=http://proxy:3128", "NO_PROXY="}, Hosts: []string{"icaro-egress:10.0.0.2"}}, nil
}

type stubSessions struct {
	ops      serial.Queue
	dir      string
	started  []string
	finished []string
	ctrs     []string
	repos    []string
}

func newStubSessions(dir string) *stubSessions { return &stubSessions{ops: serial.New(), dir: dir} }

func (s *stubSessions) StartRun(_ context.Context, runID, profile string) (session.RunWorkspace, error) {
	s.ops.Do(func() { s.started = append(s.started, runID+"/"+profile) })
	return session.RunWorkspace{SessionID: "sess-" + runID, Dir: s.dir}, nil
}

func (s *stubSessions) RepoWorktree(_ context.Context, sid, ref, rev string) (string, error) {
	s.ops.Do(func() { s.repos = append(s.repos, ref+"@"+rev) })
	d := filepath.Join(s.dir, "repo")
	return d, os.MkdirAll(d, 0o700)
}

func (s *stubSessions) SetContainer(_ context.Context, _, id string) error {
	s.ops.Do(func() { s.ctrs = append(s.ctrs, id) })
	return nil
}

func (s *stubSessions) FinishRun(_ context.Context, sid string, keep bool) error {
	s.ops.Do(func() { s.finished = append(s.finished, sid) })
	return nil
}

func TestSessionsAndEgressSeams(t *testing.T) {
	f := newFixture(t)
	eg := &stubEgress{}
	ss := newStubSessions(t.TempDir())
	f.deps.Egress, f.deps.Sessions = eg, ss
	f.cfg.Profile, f.cfg.Runtime = "workflow", "runsc"
	run := f.enqueue(`
version: 1
name: seams
steps:
  - id: plain
    image: img
  - id: repo
    image: img
    repository: { ref: github.com/acme/app, revision: main }
`, nil)
	sub := f.bus.Subscribe(run.ID)
	defer sub.Cancel()
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	if eg.profile != "workflow" || len(ss.started) != 1 || ss.started[0] != run.ID+"/workflow" {
		t.Fatalf("seams: %+v %+v", eg, ss)
	}
	got, _ := f.st.GetRun(f.ctx, run.ID)
	if got.SessionID != "sess-"+run.ID || len(ss.finished) != 1 || len(ss.ctrs) != 2 || ss.repos[0] != "github.com/acme/app@main" {
		t.Fatalf("session bookkeeping: %+v %+v", got, ss)
	}
	plain := f.containersForStep("plain")[0]
	repo := f.containersForStep("repo")[0]
	if plain.Spec.HostConfig.NetworkMode != "icaro-egress-workflow" || !strings.Contains(strings.Join(plain.Spec.Env, " "), "HTTPS_PROXY=http://proxy:3128") {
		t.Fatalf("egress not applied: %v %v", plain.Spec.HostConfig.NetworkMode, plain.Spec.Env)
	}
	if plain.Spec.HostConfig.Mounts[0].Source != ss.dir || repo.Spec.HostConfig.Mounts[0].Source != filepath.Join(ss.dir, "repo") {
		t.Fatal("workspace mounts")
	}
	if plain.Spec.Labels[LabelSession] != "sess-"+run.ID {
		t.Fatal("session label")
	}
	if plain.Spec.HostConfig.Runtime != "runsc" || repo.Spec.HostConfig.Runtime != "runsc" {
		t.Fatalf("steps run on the run profile's runtime: %q %q", plain.Spec.HostConfig.Runtime, repo.Spec.HostConfig.Runtime)
	}
	if strings.Join(plain.Spec.HostConfig.ExtraHosts, " ") != "icaro-egress:10.0.0.2" {
		t.Fatalf("a step on gVisor names the proxy in /etc/hosts: %v", plain.Spec.HostConfig.ExtraHosts)
	}
	// Events: run running/terminal and step transitions were published.
	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for !seen["run:succeeded"] {
		select {
		case ev := <-sub.C:
			seen[ev.Type+":"+ev.Status] = true
		case <-timeout:
			t.Fatalf("events seen: %v", seen)
		}
	}
	if !seen["run:running"] || !seen["step:running"] || !seen["step:succeeded"] {
		t.Fatalf("events: %v", seen)
	}
}

// repository ref and revision are rendered before git sees them, and a
// rendered value that git would read as an option, or that carries a
// secret, fails the step without reaching git.
func TestRepositoryIsRendered(t *testing.T) {
	const yaml = `
version: 1
name: repo-tmpl
steps:
  - id: co
    image: img
    repository: { ref: "https://example.com/${{ trigger.repo }}.git", revision: "${{ trigger.sha }}" }
`
	cases := []struct {
		name    string
		trigger map[string]any
		want    string // step error; "" = success
	}{
		{"rendered", map[string]any{"repo": "acme/app", "sha": "abc123"}, ""},
		{"option revision", map[string]any{"repo": "acme/app", "sha": "--output=/tmp/x"}, "git would read as an option"},
		{"secret in ref", map[string]any{"repo": "s3cr3t-value", "sha": "main"}, "contains a secret value"},
		{"missing field", map[string]any{"repo": "acme/app"}, "trigger.sha is not defined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ss := newStubSessions(t.TempDir())
			f.deps.Sessions = ss
			f.deps.Secrets = fakeSecrets{"tok": "s3cr3t-value"}
			run := f.enqueue(yaml, c.trigger)
			_, stop := f.start()
			defer stop()
			if c.want == "" {
				f.wait(run.ID, store.RunSucceeded)
				if len(ss.repos) != 1 || ss.repos[0] != "https://example.com/acme/app.git@abc123" {
					t.Fatalf("repos %v", ss.repos)
				}
				return
			}
			f.wait(run.ID, store.RunFailed)
			if r := f.step(run.ID, "co"); r.Status != store.StepFailed || !strings.Contains(r.Error, c.want) {
				t.Fatalf("step %+v, want %q", r, c.want)
			}
			if len(ss.repos) != 0 {
				t.Fatalf("git was reached: %v", ss.repos)
			}
		})
	}
	// A literal ref is checked the same way at run time.
	rx := &runExec{}
	if _, _, err := rx.renderRepository(&workflow.RepositoryRef{Ref: "  "}, expr.Env{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty ref: %v", err)
	}
	if _, _, err := rx.renderRepository(&workflow.RepositoryRef{Ref: "-x"}, expr.Env{}); err == nil {
		t.Fatal("option ref accepted")
	}
	if _, _, err := rx.renderRepository(&workflow.RepositoryRef{Ref: "r", Revision: "${{ x }}"}, expr.Env{}); err == nil || !strings.Contains(err.Error(), "repository.revision") {
		t.Fatalf("revision render: %v", err)
	}
}

func TestInvalidStoredWorkflowFailsRun(t *testing.T) {
	f := newFixture(t)
	w, _ := f.st.ApplyWorkflow(f.ctx, "broken", "version: 1\nname: broken\nsteps: []\n", "{}", "t")
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
	if err := f.st.CreateRun(f.ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunFailed)
	if !strings.Contains(got.Error, "workflow is invalid") {
		t.Fatal(got.Error)
	}
}
