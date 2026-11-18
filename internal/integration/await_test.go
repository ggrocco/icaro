package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// awaitRun stores a run of workflow parked on an await step.
func (f *hookFixture) awaitRun(workflow, step, integration, event, key, match, filter string) *store.Run {
	f.t.Helper()
	ctx := context.Background()
	w, err := f.store.ApplyWorkflow(ctx, workflow, "x", "{}", "t")
	if err != nil {
		f.t.Fatal(err)
	}
	r := &store.Run{WorkflowID: w.ID, WorkflowName: workflow, WorkflowVersion: w.Version, TriggerKind: "manual"}
	if err := f.store.CreateRun(ctx, r, []store.StepInit{{StepID: step, Kind: "await"}}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.OpenAwait(ctx, &store.StepAwait{RunID: r.ID, StepID: step, Integration: integration, Event: event,
		Workflow: workflow, Key: key, Match: match, Filter: filter, Deadline: time.Now().Add(time.Hour)}); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *hookFixture) await(r *store.Run, step string) *store.StepAwait {
	f.t.Helper()
	a, err := f.store.GetAwait(context.Background(), r.ID, step)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func resumedOf(t *testing.T, out map[string]any) []string {
	t.Helper()
	var ids []string
	evs, _ := out["events"].([]any)
	for _, e := range evs {
		rs, _ := e.(map[string]any)["resumed"].([]any)
		for _, r := range rs {
			m := r.(map[string]any)
			ids = append(ids, m["run_id"].(string)+"/"+m["step"].(string))
		}
	}
	return ids
}

// Two runs of one workflow await the same event type with different
// keys: each is resumed only by the event carrying its own key. A false
// filter, a duplicate and a step already resolved resume nothing.
func TestDeliveryResumesAwaitByKey(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	mine := f.awaitRun("ci", "pr", "github", "pull_request.opened", "fix-flux", "event.head_ref", "")
	other := f.awaitRun("ci", "pr", "github", "pull_request.opened", "other-branch", "event.head_ref", "")
	drafts := f.awaitRun("ci", "pr", "github", "pull_request.opened", "fix-flux", "event.head_ref", "event.draft == true")
	body := fixture(t, "github_pull_request_opened.json")

	code, out, raw := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusAccepted || out["status"] != store.DeliveryProcessed {
		t.Fatalf("a delivery that only resumed steps is processed: %d %s", code, raw)
	}
	if got := resumedOf(t, out); len(got) != 1 || got[0] != mine.ID+"/pr" {
		t.Fatalf("resumed %v", got)
	}
	a := f.await(mine, "pr")
	var payload map[string]any
	_ = json.Unmarshal([]byte(a.PayloadJSON), &payload)
	if a.State != store.AwaitMatched || a.DeliveryID != out["delivery_id"] || a.Actor != "integration:github" || payload["pr_number"] != float64(42) {
		t.Fatalf("%+v", a)
	}
	if f.await(other, "pr").State != store.AwaitWaiting || f.await(drafts, "pr").State != store.AwaitWaiting {
		t.Fatal("other keys and false filters keep waiting")
	}
	info, err := f.hub.Delivery(context.Background(), out["delivery_id"].(string))
	if err != nil || len(info.Events) != 1 || len(info.Events[0].Resumed) != 1 || info.Events[0].Resumed[0].Workflow != "ci" {
		t.Fatalf("the delivery record lists resumed steps: %+v %v", info, err)
	}

	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusOK || out["status"] != store.DeliveryDuplicate || len(resumedOf(t, out)) != 0 {
		t.Fatalf("duplicate: %d %v", code, out)
	}
	// An event after the step matched resumes nothing, and says so.
	later := append(append([]byte{}, body...), ' ') // signed differently: not a replay
	code, out, _ = f.post("/hooks/github", later, ghHeaders("pull_request", "d-2", "s3cret-hook", later))
	if code != http.StatusAccepted || out["status"] != store.DeliveryNoMatch || len(resumedOf(t, out)) != 0 {
		t.Fatalf("late: %d %v", code, out)
	}
	// Redelivery runs step 6b too.
	again := f.awaitRun("ci", "pr2", "github", "pull_request.opened", "fix-flux", "event.head_ref", "")
	res, err := f.hub.Redeliver(context.Background(), out["delivery_id"].(string))
	if err != nil || res.Status != store.DeliveryProcessed || len(res.Events[0].Resumed) != 1 || res.Events[0].Resumed[0].RunID != again.ID {
		t.Fatalf("redeliver: %+v %v", res, err)
	}
	// Every verified delivery's events are kept for lookbacks.
	evs, _ := f.store.ListDeliveryEventsSince(context.Background(), "github", "pull_request.opened", "", time.Time{})
	if len(evs) != 3 {
		t.Fatalf("stored events: %d", len(evs))
	}
}

// Generic webhook events resume only steps of the workflow whose token
// received them; awaiting a generic webhook gives a workflow a token.
func TestGenericWebhookResumesOwnWorkflowOnly(t *testing.T) {
	a := mustSpec(t, "version: 1\nname: hook-a\nsteps:\n  - {id: ww, await: {integration: webhook, event: received, key: k, match: event.body.k}, timeout: 1h}\n")
	b := mustSpec(t, "version: 1\nname: hook-b\nsteps:\n  - {id: ww, await: {integration: webhook, event: received, key: k, match: event.body.k}, timeout: 1h}\n")
	f := newHookFixture(t, fakeSpecs{a, b}, config.WebhooksConfig{})
	toks, err := f.hub.SyncWebhookTokens(context.Background())
	if err != nil || len(toks) != 2 {
		t.Fatalf("%+v %v", toks, err)
	}
	runA := f.awaitRun("hook-a", "ww", "webhook", "received", "k", "event.body.k", "")
	runB := f.awaitRun("hook-b", "ww", "webhook", "received", "k", "event.body.k", "")
	tokB := toks[0].Token
	if toks[0].Workflow != "hook-b" {
		tokB = toks[1].Token
	}
	code, out, raw := f.post("/hooks/webhook/"+tokB, []byte(`{"k":"k"}`), nil)
	if code != http.StatusAccepted || out["status"] != store.DeliveryProcessed {
		t.Fatalf("%d %s", code, raw)
	}
	if f.await(runA, "ww").State != store.AwaitWaiting || f.await(runB, "ww").State != store.AwaitMatched {
		t.Fatal("another workflow's token cannot resume this one")
	}
}

// When step 6b cannot write, the delivery is an error answered with 503;
// the provider's retry then resumes the step exactly once.
func TestAwaitWriteFailureIs503(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	r := f.awaitRun("ci", "pr", "github", "pull_request.opened", "fix-flux", "event.head_ref", "")
	ctx := context.Background()
	if _, err := f.store.DB().ExecContext(ctx, `CREATE TRIGGER no_resume BEFORE UPDATE ON step_awaits BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	body := fixture(t, "github_pull_request_opened.json")
	code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusServiceUnavailable || out["status"] != store.DeliveryError {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := f.store.DB().ExecContext(ctx, `DROP TRIGGER no_resume`); err != nil {
		t.Fatal(err)
	}
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusAccepted || len(resumedOf(t, out)) != 1 || f.await(r, "pr").State != store.AwaitMatched {
		t.Fatalf("retry: %d %v", code, out)
	}
	code, out, _ = f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	if code != http.StatusOK || out["status"] != store.DeliveryDuplicate {
		t.Fatalf("once: %d %v", code, out)
	}

	// Storing the events failing is an error too.
	if _, err := f.store.DB().ExecContext(ctx, `CREATE TRIGGER no_events BEFORE INSERT ON delivery_events BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	later := append(append([]byte{}, body...), ' ')
	code, out, _ = f.post("/hooks/github", later, ghHeaders("pull_request", "d-3", "s3cret-hook", later))
	if code != http.StatusServiceUnavailable || out["status"] != store.DeliveryError {
		t.Fatalf("%d %v", code, out)
	}
}

// A delivery is checked against many waiting steps within the budget of
// ADR 0024; one of them correlates.
func TestLoadDeliveryAgainstWaitingSteps(t *testing.T) {
	f := newHookFixture(t, fakeSpecs{}, config.WebhooksConfig{})
	f.secret("github.webhook_secret", "s3cret-hook")
	for i := 0; i < awaitLoadSteps-1; i++ {
		f.awaitRun("ci", fmt.Sprintf("s%d", i), "github", "pull_request.opened", fmt.Sprintf("branch-%d", i), "event.head_ref", "event.draft == false")
	}
	r := f.awaitRun("ci", "hit", "github", "pull_request.opened", "fix-flux", "event.head_ref", "event.draft == false")
	body := fixture(t, "github_pull_request_opened.json")
	start := time.Now()
	code, out, _ := f.post("/hooks/github", body, ghHeaders("pull_request", "d-1", "s3cret-hook", body))
	took := time.Since(start)
	if code != http.StatusAccepted || len(resumedOf(t, out)) != 1 || f.await(r, "hit").State != store.AwaitMatched {
		t.Fatalf("%d %v", code, out)
	}
	t.Logf("%d waiting steps checked in %v", awaitLoadSteps, took)
	if took > awaitLoadBudget {
		t.Fatalf("checked %d waiting steps in %v, budget %v", awaitLoadSteps, took, awaitLoadBudget)
	}
}

func mustSpec(t *testing.T, yaml string) *workflow.Spec {
	t.Helper()
	s, issues, err := workflow.Parse([]byte(yaml), workflow.Options{})
	if err != nil || len(issues) > 0 {
		t.Fatalf("%v %v", err, issues)
	}
	return s
}
