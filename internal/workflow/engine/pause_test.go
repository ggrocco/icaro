package engine

import (
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

const approvalYAML = `
version: 1
name: ship-it
steps:
  - id: build
    image: img
    outputs: {tag: "json:tag"}
  - id: review
    needs: [build]
    approval:
      message: "Deploy ${{ steps.build.outputs.tag }}?"
  - id: deploy
    needs: [review]
    image: img
    command: "deploy by ${{ steps.review.outputs.approved_by }}: ${{ steps.review.outputs.comment }}"
`

func tagged(c *docker.FakeContainer) docker.Behavior {
	if c.Spec.Labels[LabelStep] == "build" {
		return docker.Behavior{Output: []byte(`{"tag":"v1.2"}`)}
	}
	return docker.Behavior{}
}

// An approval parks the run, giving its engine slot to other runs; the
// decision resumes it, and its outputs reach the steps after it.
func TestApprovalParksAndResumes(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRuns = 1
	f.docker.OnStart = tagged
	run := f.enqueue(approvalYAML, nil)
	_, stop := f.start()
	defer stop()

	f.wait(run.ID, store.RunWaiting)
	if s := f.step(run.ID, "review"); s.Status != store.StepWaiting || s.StartedAt == nil {
		t.Fatalf("%+v", s)
	}
	a, err := f.st.GetApproval(f.ctx, run.ID, "review")
	if err != nil || a.Message != "Deploy v1.2?" || a.Decision != "" {
		t.Fatalf("%+v %v", a, err)
	}
	// The parked run holds no slot: another run of MaxRuns 1 runs now.
	other := f.enqueue(dataFlowYAML, map[string]any{"who": "x"})
	f.wait(other.ID, store.RunSucceeded, store.RunFailed)

	if _, err := f.st.DecideApproval(f.ctx, run.ID, "review", store.ApprovalApproved, "alice", "ship it"); err != nil {
		t.Fatal(err)
	}
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("%s %s %s", got.Status, got.Error, f.dump(run.ID))
	}
	if outs := f.outputs(run.ID, "review"); outs["approved_by"] != "alice" || outs["comment"] != "ship it" {
		t.Fatalf("%v", outs)
	}
	deploy := f.containersForStep("deploy")[0]
	if cmd := strings.Join(deploy.Spec.Cmd, " "); !strings.Contains(cmd, "deploy by alice: ship it") {
		t.Fatalf("%q", cmd)
	}
	if got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("%+v", got)
	}
}

func TestApprovalRejected(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = tagged
	run := f.enqueue(approvalYAML, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunWaiting)
	if _, err := f.st.DecideApproval(f.ctx, run.ID, "review", store.ApprovalRejected, "bob", "not on a friday"); err != nil {
		t.Fatal(err)
	}
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "rejected by bob: not on a friday") {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	if d := f.step(run.ID, "deploy"); d.Status != store.StepSkipped || !strings.HasPrefix(d.Error, notRunPrefix) {
		t.Fatalf("%+v", d)
	}
	if len(f.containersForStep("deploy")) != 0 {
		t.Fatal("a rejected approval runs nothing after it")
	}
}

// An approval left undecided past its timeout fails; a wait step ends on
// its own. Both park the run in between, with a wake time.
func TestApprovalExpiresAndWaitEnds(t *testing.T) {
	f := newFixture(t)
	run := f.enqueue(`
version: 1
name: timers
steps:
  - {id: pause, wait: 1s}
  - {id: gate, needs: [pause], approval: {}, timeout: 1s}
`, nil)
	_, stop := f.start()
	defer stop()
	parked := f.wait(run.ID, store.RunWaiting)
	if parked.WakeAt == nil {
		t.Fatal("a parked wait has a wake time")
	}
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "approval expired") {
		t.Fatalf("%s %q %s", got.Status, got.Error, f.dump(run.ID))
	}
	if p := f.step(run.ID, "pause"); p.Status != store.StepSucceeded {
		t.Fatalf("%+v", p)
	}
	a, _ := f.st.GetApproval(f.ctx, run.ID, "gate")
	if a.Decision != store.ApprovalExpired {
		t.Fatalf("%+v", a)
	}
	if _, err := f.st.DecideApproval(f.ctx, run.ID, "gate", store.ApprovalApproved, "late", ""); err != store.ErrConflict {
		t.Fatalf("too late to decide: %v", err)
	}
}

// Cancelling a parked run wakes it and cancels its waiting steps.
func TestCancelWaitingRun(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = tagged
	run := f.enqueue(approvalYAML, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunWaiting)
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	f.wait(run.ID, store.RunCancelled)
	if s := f.step(run.ID, "review"); s.Status != store.StepCancelled {
		t.Fatalf("%+v", s)
	}
	if s := f.step(run.ID, "deploy"); s.Status != store.StepCancelled {
		t.Fatalf("%+v", s)
	}
}

// A decision made while the daemon is down is acted on by the next one;
// a decision made while other steps still run is seen without parking.
func TestApprovalAcrossRestartAndWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = tagged
	run := f.enqueue(approvalYAML, nil)
	_, stop := f.start()
	f.wait(run.ID, store.RunWaiting)
	stop()
	if _, err := f.st.DecideApproval(f.ctx, run.ID, "review", store.ApprovalApproved, "carol", ""); err != nil {
		t.Fatal(err)
	}
	_, stop = f.start()
	if got := f.wait(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%s %s", got.Status, got.Error)
	}
	stop()

	f.docker.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "slow" {
			return docker.Behavior{Duration: 2 * time.Second}
		}
		return docker.Behavior{}
	}
	both := f.enqueue(`
version: 1
name: side-by-side
steps:
  - {id: slow, image: img}
  - {id: gate, approval: {}}
  - {id: after, needs: [gate], image: img}
`, nil)
	_, stop = f.start()
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for f.step(both.ID, "gate").Status != store.StepWaiting {
		if time.Now().After(deadline) {
			t.Fatal(f.dump(both.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.st.DecideApproval(f.ctx, both.ID, "gate", store.ApprovalApproved, "dave", ""); err != nil {
		t.Fatal(err)
	}
	for f.step(both.ID, "after").Status != store.StepSucceeded {
		if time.Now().After(deadline) {
			t.Fatal("the decision is acted on while slow still runs: " + f.dump(both.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.step(both.ID, "slow").Status == store.StepSucceeded {
		t.Fatal("slow must still be running when after finished")
	}
	f.wait(both.ID, store.RunSucceeded)
}

// pauseRx builds the scheduler state of a stored run, to drive the pause
// helpers directly.
func pauseRx(t *testing.T, f *fixture, yaml string) *runExec {
	t.Helper()
	run := f.enqueue(yaml, nil)
	spec, _, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := workflow.NewPlan(spec)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.GetStepRuns(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rx := &runExec{e: New(f.cfg, f.deps), run: run, spec: spec, plan: plan, rows: map[string]*store.StepRun{}, trigger: map[string]any{}}
	for _, r := range rows {
		rx.rows[r.StepID] = r
	}
	return rx
}

func TestPauseEdges(t *testing.T) {
	f := newFixture(t)
	rx := pauseRx(t, f, `
version: 1
name: edges
steps:
  - {id: gate, approval: {message: "for ${{ trigger.who }}"}}
`)
	gate := rx.spec.Step("gate")
	// A message that cannot be rendered fails the step.
	rx.startPause(f.ctx, gate)
	if row := rx.rows["gate"]; row.Status != store.StepFailed || !strings.Contains(row.Error, "approval message") {
		t.Fatalf("%+v", row)
	}

	// park refuses a run that is not running, and wakes one whose
	// approval was decided before it parked.
	rx.rows["gate"].Status = store.StepWaiting
	if err := rx.park(f.ctx, nil); err == nil {
		t.Fatal("a queued run cannot be parked")
	}
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.OpenApproval(f.ctx, rx.run.ID, "gate", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DecideApproval(f.ctx, rx.run.ID, "gate", store.ApprovalApproved, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := rx.park(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.ClaimDueRun(f.ctx, RunnerID); got == nil || got.ID != rx.run.ID {
		t.Fatal("a decision made just before parking makes the run due")
	}

	// Store errors fail the step instead of losing it.
	rx2 := pauseRx(t, f, `
version: 1
name: closed
steps:
  - {id: gate, approval: {}}
`)
	_ = f.st.Close()
	rx2.startPause(f.ctx, rx2.spec.Step("gate"))
	if row := rx2.rows["gate"]; row.Status != store.StepFailed || !strings.Contains(row.Error, "open approval") {
		t.Fatalf("%+v", row)
	}
	rx2.rows["gate"].Status = store.StepWaiting
	if settled, _ := rx2.checkPause(f.ctx, rx2.spec.Step("gate")); !settled || !strings.Contains(rx2.rows["gate"].Error, "load approval") {
		t.Fatalf("%+v", rx2.rows["gate"])
	}
}
