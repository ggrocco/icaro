package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/crypto"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

const testsYAML = `
version: 1
name: run-tests
triggers:
  - type: call
  - type: manual
outputs:
  passed: ${{ steps.test.outputs.passed }}
steps:
  - id: test
    image: img
    command: test ${{ trigger.repo }}
    outputs: {passed: "json:passed"}
`

const releaseYAML = `
version: 1
name: release
triggers: [{type: manual}]
steps:
  - id: test
    call:
      workflow: run-tests
      input: {repo: "${{ trigger.repo }}"}
  - id: report
    needs: [test]
    image: img
    command: report ${{ steps.test.outputs.passed }}
`

// callService is a service with a real engine over a fake Docker, so call
// steps run end to end through the service's enqueue (ADR 0038).
func callService(t *testing.T, depth, budget int, onStart func(*docker.FakeContainer) docker.Behavior) *Service {
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
	key, _ := crypto.NewKey()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet, MaxCallDepth: depth, MaxRunsPerRoot: budget})
	bus := events.New(ctx)
	fake := docker.NewFake()
	fake.OnStart = onStart
	e := engine.New(engine.Config{Concurrency: 4, MaxRuns: 1, StepTimeout: time.Minute, LogsDir: t.TempDir(), RunsDir: t.TempDir(),
		RetryBase: time.Millisecond, PollInterval: 20 * time.Millisecond, Heartbeat: 20 * time.Millisecond},
		engine.Deps{Store: st, Docker: fake, Events: bus, Calls: svc, Log: quiet})
	svc.SetRunner(e)
	svc.SetEvents(bus)
	done := make(chan struct{})
	ectx, stop := context.WithCancel(ctx)
	go func() { defer close(done); _ = e.Run(ectx) }()
	t.Cleanup(func() { stop(); <-done })
	return svc
}

func passing(c *docker.FakeContainer) docker.Behavior {
	if c.Spec.Labels[engine.LabelStep] == "test" {
		return docker.Behavior{Output: []byte(`{"passed":"yes"}`)}
	}
	return docker.Behavior{}
}

func waitRun(t *testing.T, svc *Service, id string) *RunInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := svc.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if Terminal(r.Status) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never finished", id)
	return nil
}

func TestCallEndToEnd(t *testing.T) {
	cmds := make(chan string, 16)
	svc := callService(t, 0, 0, func(c *docker.FakeContainer) docker.Behavior {
		cmds <- strings.Join(c.Spec.Cmd, " ")
		return passing(c)
	})
	ctx := context.Background()
	for _, y := range []string{testsYAML, releaseYAML} {
		if _, err := svc.ApplyWorkflow(ctx, y, "t"); err != nil {
			t.Fatal(err)
		}
	}
	run, err := svc.StartRun(ctx, "release", map[string]any{"repo": "acme/app"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	got := waitRun(t, svc, run.ID)
	if got.Status != store.RunSucceeded || got.Depth != 0 || got.RootRunID != run.ID {
		t.Fatalf("%+v", got)
	}
	call := got.Steps[0].Call
	if call == nil || call.Workflow != "run-tests" || call.Status != store.RunSucceeded || call.Version != 1 || call.RunID == "" {
		t.Fatalf("call %+v", call)
	}
	if string(got.Steps[0].Outputs) != `{"passed":"yes"}` {
		t.Fatal(string(got.Steps[0].Outputs))
	}
	child, err := svc.GetRun(ctx, call.RunID)
	if err != nil || child.ParentRunID != run.ID || child.RootRunID != run.ID || child.Depth != 1 || child.TriggerKind != "call" || string(child.Outputs) != `{"passed":"yes"}` {
		t.Fatalf("child %+v %v", child, err)
	}
	tree, err := svc.ListRunsFiltered(ctx, RunQuery{Root: run.ID})
	if err != nil || len(tree) != 2 {
		t.Fatalf("tree %d %v", len(tree), err)
	}
	var reported bool
	for len(cmds) > 0 {
		reported = reported || strings.Contains(<-cmds, "report yes")
	}
	if !reported {
		t.Fatal("the caller's later step did not read the callee's output")
	}
	w, err := svc.GetWorkflow(ctx, "run-tests")
	if err != nil || !w.Callable || strings.Join(w.Callers, ",") != "release" || strings.Join(w.Outputs, ",") != "passed" {
		t.Fatalf("%+v %v", w, err)
	}
}

// The brakes fail the step, name the limit and start nothing.
func TestCallBrakes(t *testing.T) {
	ctx := context.Background()
	t.Run("depth", func(t *testing.T) {
		svc := callService(t, 1, 0, passing)
		mid := "version: 1\nname: mid\ntriggers: [{type: call}]\nsteps:\n  - id: inner\n    call: {workflow: run-tests, input: {repo: x}}\n"
		top := "version: 1\nname: top\nsteps:\n  - id: outer\n    call: {workflow: mid}\n"
		for _, y := range []string{testsYAML, mid, top} {
			if _, err := svc.ApplyWorkflow(ctx, y, "t"); err != nil {
				t.Fatal(err)
			}
		}
		run, _ := svc.StartRun(ctx, "top", nil, "t")
		got := waitRun(t, svc, run.ID)
		if got.Status != store.RunFailed || !strings.Contains(got.Steps[0].Error, "child run") {
			t.Fatalf("%+v", got.Steps)
		}
		tree, _ := svc.ListRunsFiltered(ctx, RunQuery{Root: run.ID})
		if len(tree) != 2 {
			t.Fatalf("the leaf was started: %d runs", len(tree))
		}
		midRun, _ := svc.GetRun(ctx, got.Steps[0].Call.RunID)
		if e := midRun.Steps[0].Error; !strings.Contains(e, "workflows.max_call_depth (1) reached") || !strings.Contains(e, "nothing was started") {
			t.Fatal(e)
		}
	})
	t.Run("budget", func(t *testing.T) {
		svc := callService(t, 0, 2, func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{ExitCode: 1} })
		caller := "version: 1\nname: retries\nsteps:\n  - id: call\n    call: {workflow: run-tests, input: {repo: x}}\n    retry: 3\n"
		for _, y := range []string{testsYAML, caller} {
			if _, err := svc.ApplyWorkflow(ctx, y, "t"); err != nil {
				t.Fatal(err)
			}
		}
		run, _ := svc.StartRun(ctx, "retries", nil, "t")
		got := waitRun(t, svc, run.ID)
		if e := got.Steps[0].Error; got.Status != store.RunFailed || !strings.Contains(e, "workflows.max_runs_per_root (2) reached") {
			t.Fatalf("%s %q", got.Status, e)
		}
		if tree, _ := svc.ListRunsFiltered(ctx, RunQuery{Root: run.ID}); len(tree) != 2 {
			t.Fatalf("%d runs", len(tree))
		}
	})
}

// Enqueueing a child: lineage, the booking, replace as queue, dedupe, and
// the refusals.
func TestEnqueueChild(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	callee := strings.Replace(testsYAML, "name: run-tests", "name: run-tests\nconcurrency: replace", 1)
	for _, y := range []string{callee, releaseYAML} {
		if _, err := svc.ApplyWorkflow(ctx, y, "t"); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := svc.StartRun(ctx, "release", map[string]any{"repo": "r"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	req := workflow.EnqueueRequest{Workflow: "run-tests", TriggerKind: "call", Trigger: map[string]any{"repo": "r"},
		IdempotencyKey: "call:" + parent.ID + ":test:1", ParentRunID: parent.ID, ParentStep: "test", Actor: "run:" + parent.ID}
	res, err := svc.EnqueueRun(ctx, req)
	if err != nil || res.Version != 1 || res.Deduplicated {
		t.Fatalf("%+v %v", res, err)
	}
	child, _ := svc.store.GetRun(ctx, res.RunID)
	if child.Concurrency != workflow.ConcurrencyQueue || child.ParentRunID != parent.ID || child.Depth != 1 {
		t.Fatalf("%+v", child)
	}
	if st, _ := svc.store.GetStepRun(ctx, parent.ID, "test"); st.ChildRunID != res.RunID || st.ChildVersion != 1 {
		t.Fatalf("booking %+v", st)
	}
	if again, err := svc.EnqueueRun(ctx, req); err != nil || !again.Deduplicated || again.RunID != res.RunID || again.Version != 1 {
		t.Fatalf("dedupe %+v %v", again, err)
	}
	// Cancelling the parent stops the child too.
	if _, err := svc.store.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	before := fr.cancelled
	if _, err := svc.CancelRun(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if fr.cancelled != before+2 {
		t.Fatalf("cancels %d", fr.cancelled-before)
	}

	req.IdempotencyKey, req.ParentRunID = "", "nope"
	var bad *BadRequest
	if _, err := svc.EnqueueRun(ctx, req); !errors.As(err, &bad) || !strings.Contains(err.Error(), "parent run nope does not exist") {
		t.Fatal(err)
	}
	req.Workflow, req.ParentRunID = "release", parent.ID
	if _, err := svc.EnqueueRun(ctx, req); !errors.As(err, &bad) || !strings.Contains(err.Error(), "workflow release has no call trigger") {
		t.Fatal(err)
	}
}

// Saving a workflow refuses a call cycle with its path; saving a callee
// warns about what it would break in its callers.
func TestCallGraphOnSave(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	for _, y := range []string{testsYAML, releaseYAML} {
		if _, err := svc.ApplyWorkflow(ctx, y, "t"); err != nil {
			t.Fatal(err)
		}
	}
	cyc := strings.Replace(testsYAML, "steps:\n", "steps:\n  - id: again\n    call: {workflow: release, input: {repo: x}}\n", 1)
	cyc = strings.Replace(cyc, "  - type: manual\n", "", 1)
	_, err := svc.ApplyWorkflow(ctx, cyc, "t")
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "call cycle: run-tests → release → run-tests") {
		t.Fatal(err)
	}
	edited := strings.Replace(testsYAML, "outputs:\n  passed: ${{ steps.test.outputs.passed }}\n", "", 1)
	spec, issues, _ := svc.ValidateWorkflow(ctx, edited)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	w := svc.Warnings(ctx, spec)
	if len(w) != 1 || !strings.Contains(w[0].Message, `caller release would fail validation at /steps/1/command: workflow run-tests declares no output "passed"`) {
		t.Fatalf("%+v", w)
	}
	if c, err := svc.Callers(ctx, "release"); err != nil || len(c) != 0 {
		t.Fatal(c, err)
	}
}
