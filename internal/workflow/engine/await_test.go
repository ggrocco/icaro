package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

const awaitYAML = `
version: 1
name: ci-wait
steps:
  - id: push
    image: img
    outputs: {sha: "json:sha"}
  - id: ci
    needs: [push]
    await:
      integration: github
      event: check_suite.completed
      key: "${{ steps.push.outputs.sha }}"
      match: event.head_sha
      filter: "event.conclusion != 'neutral'"
    timeout: 1h
  - id: report
    needs: [ci]
    image: img
    command: "ci ${{ steps.ci.outputs.conclusion }} via ${{ steps.ci.outputs.delivery_id }}"
`

// pushed makes the push step print a sha, and optionally runs before(c)
// while it does, so a test can deliver an event mid-step.
func pushed(sha string, during func()) func(c *docker.FakeContainer) docker.Behavior {
	return func(c *docker.FakeContainer) docker.Behavior {
		if c.Spec.Labels[LabelStep] == "push" {
			if during != nil {
				during()
			}
			return docker.Behavior{Output: []byte(`{"sha":"` + sha + `"}`)}
		}
		return docker.Behavior{}
	}
}

// deliver stores a verified delivery with one event, as the integration
// pipeline does before it reads waiting rows.
func (f *fixture) deliver(integration, event, workflow string, received time.Time, payload map[string]any) string {
	f.t.Helper()
	d := &store.Delivery{Integration: integration, Workflow: workflow, Method: "POST", HeadersJSON: "{}", Verified: true, Status: store.DeliveryProcessed, ReceivedAt: received}
	if err := f.st.CreateDelivery(f.ctx, d); err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(payload)
	if err := f.st.AddDeliveryEvents(f.ctx, []store.DeliveryEvent{{DeliveryID: d.ID, Integration: integration, Event: event, Workflow: workflow, PayloadJSON: string(b), ReceivedAt: received}}); err != nil {
		f.t.Fatal(err)
	}
	return d.ID
}

// An event that arrives while the step's needs ancestor still runs is
// found by the lookback as soon as the step starts. Events from before
// the run, with another key, or failing the filter are not taken.
func TestAwaitLooksBackFromRunStart(t *testing.T) {
	f := newFixture(t)
	f.deliver("github", "check_suite.completed", "", time.Now().Add(-time.Hour), map[string]any{"head_sha": "abc", "conclusion": "stale"})
	var want string
	f.docker.OnStart = pushed("abc", func() {
		f.deliver("github", "check_suite.completed", "", time.Now(), map[string]any{"head_sha": "zzz", "conclusion": "success"})
		f.deliver("github", "check_suite.completed", "", time.Now(), map[string]any{"head_sha": "abc", "conclusion": "neutral"})
		want = f.deliver("github", "check_suite.completed", "", time.Now(), map[string]any{"head_sha": "abc", "conclusion": "success"})
		f.deliver("github", "check_suite.completed", "", time.Now(), map[string]any{"head_sha": "abc", "conclusion": "failure"})
	})
	run := f.enqueue(awaitYAML, nil)
	_, stop := f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("%s %s %s", got.Status, got.Error, f.dump(run.ID))
	}
	outs := f.outputs(run.ID, "ci")
	if outs["conclusion"] != "success" || outs["delivery_id"] != want || outs["head_sha"] != "abc" {
		t.Fatalf("%v", outs)
	}
	if cmd := strings.Join(f.containersForStep("report")[0].Spec.Cmd, " "); !strings.Contains(cmd, "ci success via "+want) {
		t.Fatalf("%q", cmd)
	}
	a, err := f.st.GetAwait(f.ctx, run.ID, "ci")
	if err != nil || a.State != store.AwaitMatched || a.Key != "abc" || a.Actor != "integration:github" || a.Workflow != "ci-wait" {
		t.Fatalf("%+v %v", a, err)
	}
}

// With nothing to look back on, the run parks holding no slot, and a
// match recorded by the pipeline wakes and resumes it.
func TestAwaitParksAndResumes(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRuns = 1
	f.docker.OnStart = pushed("abc", nil)
	run := f.enqueue(awaitYAML, nil)
	_, stop := f.start()
	defer stop()
	parked := f.wait(run.ID, store.RunWaiting)
	if parked.WakeAt == nil {
		t.Fatal("a parked await wakes at its deadline")
	}
	if s := f.step(run.ID, "ci"); s.Status != store.StepWaiting {
		t.Fatalf("%+v", s)
	}
	other := f.enqueue(dataFlowYAML, map[string]any{"who": "x"})
	f.wait(other.ID, store.RunSucceeded, store.RunFailed)

	if _, err := f.st.ResolveAwait(f.ctx, run.ID, "ci", store.AwaitMatched, "d9", `{"head_sha":"abc","conclusion":"success","n":3}`, "integration:github"); err != nil {
		t.Fatal(err)
	}
	if got := f.wait(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%s %s", got.Status, got.Error)
	}
	if outs := f.outputs(run.ID, "ci"); outs["n"] != "3" || outs["delivery_id"] != "d9" {
		t.Fatalf("%v", outs)
	}
}

// Past its deadline an await fails and its dependents are skipped, also
// when the daemon was stopped across the deadline.
func TestAwaitExpires(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = pushed("abc", nil)
	run := f.enqueue(strings.Replace(awaitYAML, "timeout: 1h", "timeout: 1s", 1), nil)
	_, stop := f.start()
	f.wait(run.ID, store.RunWaiting)
	stop()
	time.Sleep(1100 * time.Millisecond)
	_, stop = f.start()
	defer stop()
	got := f.wait(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "no matching github.check_suite.completed before ") {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	if r := f.step(run.ID, "report"); r.Status != store.StepSkipped {
		t.Fatalf("%+v", r)
	}
	if a, _ := f.st.GetAwait(f.ctx, run.ID, "ci"); a.State != store.AwaitExpired {
		t.Fatalf("%+v", a)
	}
}

// Cancelling a parked run records its await cancelled, so a later event
// resumes nothing.
func TestAwaitCancelled(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = pushed("abc", nil)
	run := f.enqueue(awaitYAML, nil)
	_, stop := f.start()
	defer stop()
	f.wait(run.ID, store.RunWaiting)
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	f.wait(run.ID, store.RunCancelled)
	if a, _ := f.st.GetAwait(f.ctx, run.ID, "ci"); a.State != store.AwaitCancelled {
		t.Fatalf("%+v", a)
	}
	if _, err := f.st.ResolveAwait(f.ctx, run.ID, "ci", store.AwaitMatched, "late", "{}", ""); err != store.ErrConflict {
		t.Fatalf("a late event resumes nothing: %v", err)
	}
}

const webhookAwaitYAML = `
version: 1
name: hook-wait
steps:
  - id: hook
    await: {integration: webhook, event: received, key: "${{ run.workflow }}", match: "'hook-wait'"}
    timeout: 1h
`

// The event lands between the commit of the waiting row and the
// lookback, in both orders: seen by the lookback, or claimed by the
// pipeline first. Either way it is used once. Generic webhook events
// count only on the run's own workflow token.
func TestAwaitStartRace(t *testing.T) {
	f := newFixture(t)
	rx := pauseRx(t, f, webhookAwaitYAML)
	st := rx.spec.Step("hook")
	f.deliver("webhook", "received", "other-workflow", time.Now(), map[string]any{})
	a := rx.openAwait(f.ctx, st)
	if a == nil || a.Key != "hook-wait" {
		t.Fatalf("%+v %+v", a, rx.rows["hook"])
	}
	// Order 1: the event is persisted before the lookback reads.
	mine := f.deliver("webhook", "received", "hook-wait", time.Now(), map[string]any{"x": 1})
	rx.lookBack(f.ctx, a)
	got, _ := f.st.GetAwait(f.ctx, rx.run.ID, "hook")
	if got.State != store.AwaitMatched || got.DeliveryID != mine {
		t.Fatalf("%+v", got)
	}

	// Order 2: the pipeline claims it before the lookback reads; the
	// lookback's claim loses and changes nothing.
	rx2 := pauseRx(t, f, webhookAwaitYAML)
	a2 := rx2.openAwait(f.ctx, st)
	if _, err := f.st.ResolveAwait(f.ctx, rx2.run.ID, "hook", store.AwaitMatched, "pipeline", "{}", "integration:webhook"); err != nil {
		t.Fatal(err)
	}
	rx2.lookBack(f.ctx, a2)
	if got, _ := f.st.GetAwait(f.ctx, rx2.run.ID, "hook"); got.DeliveryID != "pipeline" {
		t.Fatalf("%+v", got)
	}
	// The step settles from the row either way.
	rx2.rows["hook"].Status = store.StepWaiting
	if settled, _ := rx2.checkPause(f.ctx, st); !settled || rx2.rows["hook"].Status != store.StepSucceeded {
		t.Fatalf("%+v", rx2.rows["hook"])
	}
}

func TestAwaitEdges(t *testing.T) {
	f := newFixture(t)
	rx := pauseRx(t, f, `
version: 1
name: edges
steps:
  - {id: bad, await: {integration: github, event: push, key: "${{ trigger.who }}", match: event.x}, timeout: 1h}
  - {id: empty, await: {integration: github, event: push, key: "${{ env.none }}", match: event.x}, timeout: 1h}
  - {id: odd, await: {integration: github, event: push, key: "k", match: "event.x.y"}, timeout: 1h}
env: {none: ""}
`)
	rx.env = map[string]string{"none": ""}
	rx.startPause(f.ctx, rx.spec.Step("bad"))
	if row := rx.rows["bad"]; row.Status != store.StepFailed || !strings.Contains(row.Error, "await key") {
		t.Fatalf("%+v", row)
	}
	rx.startPause(f.ctx, rx.spec.Step("empty"))
	if row := rx.rows["empty"]; row.Status != store.StepFailed || row.Error != "await key rendered empty" {
		t.Fatalf("%+v", row)
	}
	// An event whose payload cannot be evaluated is passed over.
	f.deliver("github", "push", "", time.Now(), map[string]any{"x": "string"})
	rx.startPause(f.ctx, rx.spec.Step("odd"))
	if row := rx.rows["odd"]; row.Status != store.StepWaiting {
		t.Fatalf("%+v", row)
	}
	if a, _ := f.st.GetAwait(f.ctx, rx.run.ID, "odd"); a.State != store.AwaitWaiting {
		t.Fatalf("%+v", a)
	}
	if settled, due := rx.checkPause(f.ctx, rx.spec.Step("odd")); settled || due == nil {
		t.Fatal("waits until its deadline")
	}

	// park wakes a run whose await matched before it parked.
	if _, err := f.st.ClaimRun(f.ctx, RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ResolveAwait(f.ctx, rx.run.ID, "odd", store.AwaitMatched, "d", "{}", ""); err != nil {
		t.Fatal(err)
	}
	if err := rx.park(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.ClaimDueRun(f.ctx, RunnerID); got == nil || got.ID != rx.run.ID {
		t.Fatal("a match made just before parking makes the run due")
	}
	// A cancelled await fails its step.
	if _, err := f.st.OpenAwait(f.ctx, &store.StepAwait{RunID: rx.run.ID, StepID: "bad", Integration: "github", Event: "push", Key: "k", Match: "event.x", Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CancelAwaits(f.ctx, rx.run.ID); err != nil {
		t.Fatal(err)
	}
	rx.rows["bad"].Status = store.StepWaiting
	if settled, _ := rx.checkPause(f.ctx, rx.spec.Step("bad")); !settled || rx.rows["bad"].Error != "await cancelled" {
		t.Fatalf("%+v", rx.rows["bad"])
	}

	// Store errors fail the step instead of losing it.
	rx2 := pauseRx(t, f, `
version: 1
name: closed
steps:
  - {id: hook, await: {integration: github, event: push, key: k, match: event.x}, timeout: 1h}
`)
	_ = f.st.Close()
	rx2.startPause(f.ctx, rx2.spec.Step("hook"))
	if row := rx2.rows["hook"]; row.Status != store.StepFailed || !strings.Contains(row.Error, "open await") {
		t.Fatalf("%+v", row)
	}
	rx2.lookBack(f.ctx, &store.StepAwait{RunID: rx2.run.ID, StepID: "hook", Integration: "github", Event: "push"})
	rx2.rows["hook"].Status = store.StepWaiting
	if settled, _ := rx2.checkPause(f.ctx, rx2.spec.Step("hook")); !settled || !strings.Contains(rx2.rows["hook"].Error, "load await") {
		t.Fatalf("%+v", rx2.rows["hook"])
	}
}

// A run parked on an await resumes on its event after a daemon restart.
func TestAwaitResumesAfterRestart(t *testing.T) {
	f := newFixture(t)
	f.docker.OnStart = pushed("abc", nil)
	run := f.enqueue(awaitYAML, nil)
	_, stop := f.start()
	f.wait(run.ID, store.RunWaiting)
	stop()
	if _, err := f.st.ResolveAwait(f.ctx, run.ID, "ci", store.AwaitMatched, "d1", `{"head_sha":"abc","conclusion":"success"}`, "integration:github"); err != nil {
		t.Fatal(err)
	}
	_, stop = f.start()
	defer stop()
	if got := f.wait(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%s %s", got.Status, got.Error)
	}
	if cmd := strings.Join(f.containersForStep("report")[0].Spec.Cmd, " "); !strings.Contains(cmd, "ci success via d1") {
		t.Fatalf("%q", cmd)
	}
}
