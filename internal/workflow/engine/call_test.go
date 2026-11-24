package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// storeCalls enqueues call steps' children straight into the store, the
// way the workflow service does (ADR 0038): idempotency, the callee's
// version and policy, the depth cap, and the booking in one transaction.
type storeCalls struct {
	st       *store.Store
	maxDepth int
	budget   int
	err      error
}

func (c *storeCalls) EnqueueRun(ctx context.Context, req workflow.EnqueueRequest) (workflow.EnqueueResult, error) {
	if c.err != nil {
		return workflow.EnqueueResult{}, c.err
	}
	if prev, err := c.st.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
		return workflow.EnqueueResult{RunID: prev.ID, Deduplicated: true, Version: prev.WorkflowVersion}, nil
	}
	w, err := c.st.GetWorkflow(ctx, req.Workflow)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	version := w.Version
	if req.Version != 0 {
		version = req.Version
	}
	v, err := c.st.GetWorkflowVersion(ctx, w.ID, version)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	spec, _, _ := workflow.Parse([]byte(v.SpecYAML), workflow.Options{})
	steps, _ := StepInits(spec)
	parent, err := c.st.GetRun(ctx, req.ParentRunID)
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	if parent.Depth+1 > c.maxDepth {
		return workflow.EnqueueResult{}, fmt.Errorf("workflows.max_call_depth (%d) reached", c.maxDepth)
	}
	tj, _ := json.Marshal(req.Trigger)
	run := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: version, TriggerKind: req.TriggerKind, TriggerJSON: string(tj),
		IdempotencyKey: req.IdempotencyKey, Actor: req.Actor, RootRunID: parent.RootRunID, ParentRunID: parent.ID, Depth: parent.Depth + 1,
		Concurrency: spec.ConcurrencyPolicy()}
	out, err := c.st.EnqueueCall(ctx, run, steps, store.CallBooking{ParentRunID: parent.ID, StepID: req.ParentStep, MaxRunsPerRoot: c.budget})
	if errors.Is(err, store.ErrRootBudget) {
		return workflow.EnqueueResult{}, fmt.Errorf("workflows.max_runs_per_root (%d) reached", c.budget)
	}
	if err != nil {
		return workflow.EnqueueResult{}, err
	}
	if out.Blocking != nil {
		return workflow.EnqueueResult{Skipped: "concurrency forbid: run " + out.Blocking.ID + " is " + out.Blocking.Status}, nil
	}
	return workflow.EnqueueResult{RunID: run.ID, Version: version}, nil
}

func (f *fixture) calls() *storeCalls {
	c := &storeCalls{st: f.st, maxDepth: 4, budget: 64}
	f.deps.Calls = c
	return c
}

// apply stores a workflow version without starting a run.
func (f *fixture) apply(yaml string) {
	f.t.Helper()
	spec, issues, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil || len(issues) > 0 {
		f.t.Fatalf("workflow: %v %v", err, issues)
	}
	if _, err := f.st.ApplyWorkflow(f.ctx, spec.Name, yaml, "{}", "test"); err != nil {
		f.t.Fatal(err)
	}
}

// tree returns the runs of a tree, oldest first.
func (f *fixture) tree(root string) []*store.Run {
	f.t.Helper()
	runs, err := f.st.ListRuns(f.ctx, store.RunFilter{RootRunID: root})
	if err != nil {
		f.t.Fatal(err)
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs
}

const leafYAML = `
version: 1
name: leaf
triggers: [{type: call}]
outputs:
  sum: ${{ steps.add.outputs.sum }}
  note: "leaf of ${{ trigger.who }}"
steps:
  - id: add
    image: img
    command: add ${{ trigger.who }}
    outputs: {sum: "json:sum"}
`

const midYAML = `
version: 1
name: mid
triggers: [{type: call}]
outputs:
  sum: ${{ steps.inner.outputs.sum }}
steps:
  - id: inner
    call:
      workflow: leaf
      input: {who: "${{ trigger.who }}"}
`

const topYAML = `
version: 1
name: top
steps:
  - id: outer
    call:
      workflow: mid
      input: {who: "${{ trigger.who }}"}
  - id: report
    needs: [outer]
    image: img
    command: "report ${{ steps.outer.outputs.sum }}"
`

func summing(c *docker.FakeContainer) docker.Behavior {
	if c.Spec.Labels[LabelStep] == "add" {
		return docker.Behavior{Output: []byte(`{"sum":"42"}`)}
	}
	return docker.Behavior{}
}

// A three-deep chain of calls completes with MaxRuns 1, since a waiting
// caller holds no slot; outputs flow back up and lineage is recorded.
func TestCallChainWithOneSlot(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRuns = 1
	f.calls()
	f.docker.OnStart = summing
	f.apply(leafYAML)
	f.apply(midYAML)
	top := f.enqueue(topYAML, map[string]any{"who": "ana"})
	_, stop := f.start()
	defer stop()

	got := f.wait(top.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("%s %s %s", got.Status, got.Error, f.dump(top.ID))
	}
	report := f.containersForStep("report")[0]
	if cmd := strings.Join(report.Spec.Cmd, " "); !strings.Contains(cmd, "report 42") {
		t.Fatalf("%q", cmd)
	}
	tree := f.tree(top.ID)
	if len(tree) != 3 {
		t.Fatalf("tree of %d", len(tree))
	}
	mid, leaf := tree[1], tree[2]
	if mid.ParentRunID != top.ID || mid.Depth != 1 || leaf.ParentRunID != mid.ID || leaf.Depth != 2 || leaf.RootRunID != top.ID {
		t.Fatalf("lineage %+v %+v", mid, leaf)
	}
	if mid.TriggerKind != workflow.RunTriggerCall || mid.Actor != "run:"+top.ID || leaf.TriggerJSON != `{"who":"ana"}` {
		t.Fatalf("child %+v", mid)
	}
	if leaf.OutputsJSON != `{"note":"leaf of ana","sum":"42"}` || mid.OutputsJSON != `{"sum":"42"}` {
		t.Fatalf("outputs %s %s", leaf.OutputsJSON, mid.OutputsJSON)
	}
	st := f.step(top.ID, "outer")
	if st.ChildRunID != mid.ID || st.ChildVersion != 1 || st.Attempt != 1 {
		t.Fatalf("step %+v", st)
	}
	if out := f.outputs(top.ID, "outer"); out["sum"] != "42" {
		t.Fatal(out)
	}
	logs, _ := os.ReadFile(st.LogPath)
	if !strings.Contains(string(logs), "call mid version 1 (attempt 1): child run "+mid.ID) || !strings.Contains(string(logs), "child run "+mid.ID+" succeeded") {
		t.Fatalf("log %q", logs)
	}
}

const flakyLeafYAML = `
version: 1
name: flaky
triggers: [{type: call}]
steps:
  - id: work
    image: img
`

const retryCallerYAML = `
version: 1
name: caller
steps:
  - id: call
    call: {workflow: flaky}
    retry: 1
`

// A failed child fails the attempt; retry starts a new child under a new
// key, and the step succeeds with the second child.
func TestCallRetryStartsANewChild(t *testing.T) {
	f := newFixture(t)
	f.calls()
	n := 0
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		n++
		if n == 1 {
			return docker.Behavior{ExitCode: 3}
		}
		return docker.Behavior{}
	}
	f.apply(flakyLeafYAML)
	run := f.enqueue(retryCallerYAML, nil)
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("%s %s %s", got.Status, got.Error, f.dump(run.ID))
	}
	tree := f.tree(run.ID)
	if len(tree) != 3 || tree[1].Status != store.RunFailed || tree[2].Status != store.RunSucceeded {
		t.Fatalf("tree %+v", tree)
	}
	if tree[1].IdempotencyKey != "call:"+run.ID+":call:1" || tree[2].IdempotencyKey != "call:"+run.ID+":call:2" {
		t.Fatal(tree[1].IdempotencyKey, tree[2].IdempotencyKey)
	}
	st := f.step(run.ID, "call")
	if st.Attempt != 2 || st.ChildRunID != tree[2].ID {
		t.Fatalf("%+v", st)
	}
	first, _ := os.ReadFile(f.engineLog(run.ID, "call", 1))
	if !strings.Contains(string(first), "error: child run "+tree[1].ID+" failed") {
		t.Fatalf("%q", first)
	}
}

func (f *fixture) engineLog(runID, step string, attempt int) string {
	return New(f.cfg, f.deps).logPath(runID, step, attempt)
}

// Without retries a failed or cancelled child fails the step, and only
// the steps that need it are skipped.
func TestCallChildFailsOrIsCancelled(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		switch c.Spec.Labels[LabelStep] {
		case "work":
			return docker.Behavior{ExitCode: 1, Stderr: "boom"}
		case "slow":
			return docker.Behavior{Duration: time.Hour}
		}
		return docker.Behavior{}
	}
	f.apply(flakyLeafYAML)
	f.apply(strings.Replace(strings.Replace(flakyLeafYAML, "name: flaky", "name: slow", 1), "id: work", "id: slow", 1))
	run := f.enqueue(`
version: 1
name: caller
steps:
  - id: fails
    call: {workflow: flaky}
  - id: cancelled
    call: {workflow: slow}
  - id: after
    needs: [fails]
    image: img
  - id: other
    image: img
`, nil)
	_, stop := f.start()
	defer stop()
	f.waitStatus(run.ID, "fails", store.StepFailed)
	if s := f.step(run.ID, "fails"); !strings.Contains(s.Error, "child run "+s.ChildRunID+" failed: step work failed") {
		t.Fatal(s.Error)
	}
	f.waitStatus(run.ID, "after", store.StepSkipped)
	// Cancelling the child by hand fails only the parent's step.
	f.waitStatus(run.ID, "other", store.StepSucceeded)
	child := f.waitChild(run.ID, "cancelled", store.RunRunning)
	if _, err := f.st.RequestCancel(f.ctx, child); err != nil {
		t.Fatal(err)
	}
	got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
	if got.Status != store.RunFailed {
		t.Fatal(got.Status)
	}
	if s := f.step(run.ID, "cancelled"); s.Status != store.StepFailed || s.Error != "child run "+child+" cancelled" {
		t.Fatalf("%+v", s)
	}
}

func (f *fixture) waitStatus(runID, step, status string) {
	f.t.Helper()
	f.waitStep(runID, step, func(r *store.StepRun) bool { return r.Status == status })
}

// waitChild waits until a call step's child run is in status.
func (f *fixture) waitChild(runID, step, status string) string {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if id := f.step(runID, step).ChildRunID; id != "" {
			if r, _ := f.st.GetRun(f.ctx, id); r != nil && r.Status == status {
				return id
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("child of %s never %s: %s", step, status, f.dump(runID))
	return ""
}

// A step timeout cancels the child and fails the step.
func TestCallTimeoutCancelsTheChild(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
	f.apply(flakyLeafYAML)
	run := f.enqueue(`
version: 1
name: caller
steps:
  - id: call
    call: {workflow: flaky}
    timeout: 300ms
`, nil)
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
	st := f.step(run.ID, "call")
	if got.Status != store.RunFailed || !strings.Contains(st.Error, "timed out after 300ms; child run "+st.ChildRunID+" cancelled") {
		t.Fatalf("%s %+v", got.Status, st)
	}
	f.wait(st.ChildRunID, store.RunCancelled)
}

// Cancelling the root cancels every active descendant, and the root is
// recorded cancelled only once they are.
func TestCancelRootCancelsTheTree(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
	f.apply(leafYAML)
	f.apply(midYAML)
	top := f.enqueue(topYAML, map[string]any{"who": "x"})
	e, stop := f.start()
	defer stop()
	mid := f.waitChild(top.ID, "outer", store.RunWaiting)
	leaf := f.waitChild(mid, "inner", store.RunRunning)
	if _, err := f.st.RequestCancel(f.ctx, top.ID); err != nil {
		t.Fatal(err)
	}
	e.Wake()
	root := f.wait(top.ID, store.RunCancelled)
	for _, id := range []string{mid, leaf} {
		r := f.wait(id, store.RunCancelled)
		if r.FinishedAt == nil || r.FinishedAt.After(*root.FinishedAt) {
			t.Fatalf("%s finished after the root: %v %v", id, r.FinishedAt, root.FinishedAt)
		}
	}
	if s := f.step(top.ID, "outer"); s.Status != store.StepCancelled {
		t.Fatalf("%+v", s)
	}
}

// A daemon that stops while a caller is parked, and a restart between a
// child's enqueue and the step's bookkeeping, both end with one child and
// a woken parent.
func TestCallSurvivesRestarts(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		b := summing(c)
		b.Duration = 500 * time.Millisecond
		return b
	}
	f.apply(leafYAML)
	run := f.enqueue(`
version: 1
name: caller
steps:
  - id: call
    call: {workflow: leaf, input: {who: me}}
`, nil)
	// The child was enqueued and booked, but the parent's step never
	// recorded it before the daemon died.
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deps.Calls.EnqueueRun(f.ctx, workflow.EnqueueRequest{Workflow: "leaf", TriggerKind: "call", Trigger: map[string]any{"who": "me"},
		IdempotencyKey: "call:" + run.ID + ":call:1", ParentRunID: run.ID, ParentStep: "call"}); err != nil {
		t.Fatal(err)
	}
	_, stop := f.start()
	f.wait(run.ID, store.RunWaiting)
	stop()
	// The child's container finishes while no engine runs; a new one
	// re-attaches to it, and the child's end wakes the parent.
	_, stop = f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("%s %s", got.Status, f.dump(run.ID))
	}
	if tree := f.tree(run.ID); len(tree) != 2 {
		t.Fatalf("children: %d", len(tree)-1)
	}
}

// A re-run calls the child versions the original recorded.
func TestCallReRunUsesRecordedVersions(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = summing
	f.apply(leafYAML)
	caller := "version: 1\nname: caller\nsteps:\n  - id: call\n    call: {workflow: leaf, input: {who: me}}\n"
	run := f.enqueue(caller, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunSucceeded)
	f.apply(strings.Replace(leafYAML, "leaf of", "edited leaf of", 1))
	w, _ := f.st.GetWorkflow(f.ctx, "caller")
	steps, _ := StepInits(mustParse(t, caller))
	again := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual", RerunOf: run.ID}
	if err := f.st.CreateRun(f.ctx, again, steps); err != nil {
		t.Fatal(err)
	}
	f.wait(again.ID, store.RunSucceeded)
	if st := f.step(again.ID, "call"); st.ChildVersion != 1 {
		t.Fatalf("re-run called version %d", st.ChildVersion)
	}
	if out := f.outputs(again.ID, "call"); out["note"] != "leaf of me" {
		t.Fatal(out)
	}
	// A new run calls the current version, and is its own root.
	fresh := f.enqueue(caller, nil)
	f.wait(fresh.ID, store.RunSucceeded)
	if st := f.step(fresh.ID, "call"); st.ChildVersion != 2 {
		t.Fatal(st.ChildVersion)
	}
	if again.RootRunID != again.ID {
		t.Fatal("a re-run is a new root")
	}
}

func mustParse(t *testing.T, y string) *workflow.Spec {
	t.Helper()
	s, issues, err := workflow.Parse([]byte(y), workflow.Options{})
	if err != nil || len(issues) > 0 {
		t.Fatal(err, issues)
	}
	return s
}

// The ways a call can fail to start: no enqueuer, an input that does not
// render, an enqueue error (a brake), and a callee policy that skips.
func TestCallStartFailures(t *testing.T) {
	cases := []struct {
		name, caller, want string
		calls              func(*storeCalls)
		noCalls            bool
	}{
		{name: "no enqueuer", noCalls: true, want: "no workflow service is configured; cannot call flaky"},
		{name: "input", caller: `{workflow: flaky, input: {x: "${{ trigger.missing }}"}}`, want: "input: "},
		{name: "brake", calls: func(c *storeCalls) { c.maxDepth = 0 }, want: "call flaky: workflows.max_call_depth (0) reached"},
		{name: "budget", calls: func(c *storeCalls) { c.budget = 1 }, want: "call flaky: workflows.max_runs_per_root (1) reached"},
		{name: "error", calls: func(c *storeCalls) { c.err = errors.New("db down") }, want: "call flaky: db down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if !c.noCalls {
				calls := f.calls()
				if c.calls != nil {
					c.calls(calls)
				}
			}
			f.apply(flakyLeafYAML)
			call := c.caller
			if call == "" {
				call = "{workflow: flaky}"
			}
			run := f.enqueue("version: 1\nname: caller\nsteps:\n  - id: call\n    call: "+call+"\n", nil)
			_, stop := f.start()
			defer stop()
			f.wait(run.ID, store.RunFailed)
			if s := f.step(run.ID, "call"); !strings.Contains(s.Error, c.want) {
				t.Fatalf("%q", s.Error)
			}
		})
	}
}

// A callee with concurrency forbid fails a call while it is active.
func TestCallForbidPolicy(t *testing.T) {
	f := newFixture(t)
	f.calls()
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior { return docker.Behavior{Duration: time.Hour} }
	f.apply(strings.Replace(flakyLeafYAML, "name: flaky", "name: flaky\nconcurrency: forbid", 1))
	run := f.enqueue("version: 1\nname: caller\nsteps:\n  - id: one\n    call: {workflow: flaky}\n  - id: two\n    needs: [one]\n    call: {workflow: flaky}\n  - id: three\n    call: {workflow: flaky}\n", nil)
	_, stop := f.start()
	defer stop()
	f.waitStatus(run.ID, "three", store.StepFailed)
	if s := f.step(run.ID, "three"); !strings.Contains(s.Error, "call flaky: concurrency forbid: run ") {
		t.Fatal(s.Error)
	}
}

// Workflow outputs that fail to evaluate, or are too large, fail the run.
func TestWorkflowOutputErrors(t *testing.T) {
	skipped := `version: 1
name: outs
outputs:
  x: ${{ steps.skip.outputs.sum }}
steps:
  - id: skip
    image: img
    when: "1 == 2"
    outputs: {sum: "json:sum"}
`
	big := `version: 1
name: outs
outputs:
  x: "${{ steps.add.outputs.sum }}${{ steps.add.outputs.sum }}${{ steps.add.outputs.sum }}${{ steps.add.outputs.sum }}${{ steps.add.outputs.sum }}"
steps:
  - id: add
    image: img
    outputs: {sum: "json:sum"}
`
	for _, c := range []struct{ yaml, want string }{{skipped, "outputs.x: "}, {big, "over the 16-byte limit"}} {
		f := newFixture(t)
		f.cfg.OutputLimit = 16
		f.docker.OnStart = summing
		run := f.enqueue(c.yaml, nil)
		_, stop := f.start()
		got := f.wait(run.ID, store.RunFailed, store.RunSucceeded)
		stop()
		if got.Status != store.RunFailed || !strings.Contains(got.Error, c.want) {
			t.Fatalf("%s %q", got.Status, got.Error)
		}
	}
}

// A call step left waiting by a previous daemon resumes from its row: one
// whose enqueue never happened starts it, and one whose child is gone
// fails.
func TestCallResumesFromItsRow(t *testing.T) {
	for _, c := range []struct {
		child, want string
	}{{"", store.RunSucceeded}, {"ghost", store.RunFailed}} {
		f := newFixture(t)
		f.calls()
		f.docker.OnStart = summing
		f.apply(leafYAML)
		run := f.enqueue("version: 1\nname: caller\nsteps:\n  - id: call\n    call: {workflow: leaf, input: {who: me}}\n", nil)
		row := f.step(run.ID, "call")
		now := time.Now()
		row.Status, row.Attempt, row.StartedAt, row.ChildRunID = store.StepWaiting, 1, &now, c.child
		if err := f.st.UpdateStepRun(f.ctx, row); err != nil {
			t.Fatal(err)
		}
		_, stop := f.start()
		got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
		stop()
		if got.Status != c.want {
			t.Fatalf("%q: %s %s", c.child, got.Status, f.dump(run.ID))
		}
		if c.child == "ghost" && !strings.Contains(f.step(run.ID, "call").Error, "load child run") {
			t.Fatal(f.dump(run.ID))
		}
	}
}

// A call whose log cannot be written still runs; the log is best effort.
func TestCallLogUnwritable(t *testing.T) {
	f := newFixture(t)
	f.calls()
	blocked := t.TempDir() + "/file"
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg.LogsDir = blocked
	f.apply("version: 1\nname: none\ntriggers: [{type: call}]\nsteps:\n  - id: later\n    wait: 1s\n")
	run := f.enqueue("version: 1\nname: caller\nsteps:\n  - id: call\n    call: {workflow: none}\n", nil)
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatal(f.dump(run.ID))
	}
}

func TestStopRunAndRecordedVersionErrors(t *testing.T) {
	f := newFixture(t)
	e := New(f.cfg, f.deps)
	e.stopRun(f.ctx, "nope") // logged, nothing to stop
	rx := &runExec{e: e, run: &store.Run{ID: "r", RerunOf: "missing"}}
	if v, err := rx.calledVersion(f.ctx, "call"); err != nil || v != 0 {
		t.Fatal(v, err)
	}
	_ = f.st.Close()
	if _, err := rx.calledVersion(f.ctx, "call"); err == nil || !strings.Contains(err.Error(), "re-run's original step") {
		t.Fatal(err)
	}
	if e.waitForChildren(f.ctx, "r") {
		t.Fatal("no children can be listed")
	}
}

// A child parked on an approval keeps its caller parked, with neither
// holding a slot, until the approval is decided.
func TestCallChildWaitsOnApproval(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRuns = 1
	f.calls()
	f.apply("version: 1\nname: gated\ntriggers: [{type: call}]\nsteps:\n  - id: gate\n    approval: {message: \"ok?\"}\n")
	run := f.enqueue("version: 1\nname: caller\nsteps:\n  - id: call\n    call: {workflow: gated}\n", nil)
	_, stop := f.start()
	defer stop()
	child := f.waitChild(run.ID, "call", store.RunWaiting)
	f.wait(run.ID, store.RunWaiting)
	// Both parked: another run gets the single slot.
	other := f.enqueue(dataFlowYAML, map[string]any{"who": "x"})
	f.wait(other.ID, store.RunSucceeded, store.RunFailed)
	if _, err := f.st.DecideApproval(f.ctx, child, "gate", store.ApprovalApproved, "ana", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.WakeRun(f.ctx, child); err != nil {
		t.Fatal(err)
	}
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatal(f.dump(run.ID))
	}
}
