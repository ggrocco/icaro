package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

type awaitCatalog struct{ workflow.Catalog }

func (awaitCatalog) Event(integration, event string) (workflow.EventInfo, bool) {
	if integration == "github" && event == "check_suite.completed" {
		return workflow.EventInfo{Fields: []string{"head_sha", "conclusion"}}, true
	}
	return workflow.EventInfo{}, false
}

const awaiting = `
version: 1
name: awaiting
steps:
  - {id: build, image: x}
  - id: ci
    needs: [build]
    await: {integration: github, event: check_suite.completed, key: abc, match: event.head_sha}
    timeout: 2h
`

// parkAtCI puts a run of awaiting where the engine leaves it: the await
// open and waiting, the run parked.
func parkAtCI(t *testing.T, svc *Service, event string) string {
	t.Helper()
	ctx := context.Background()
	res, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "awaiting"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.store.GetStepRun(ctx, res.RunID, "ci")
	now := store.Now()
	st.Status, st.Attempt, st.StartedAt = store.StepWaiting, 1, &now
	if err := svc.store.UpdateStepRun(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.OpenAwait(ctx, &store.StepAwait{RunID: res.RunID, StepID: "ci", Integration: "github", Event: event,
		Workflow: "awaiting", Key: "abc", Match: "event.head_sha", Deadline: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ParkRun(ctx, res.RunID, nil); err != nil {
		t.Fatal(err)
	}
	return res.RunID
}

func TestDeliverAwait(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	svc.SetCatalog(awaitCatalog{})
	mustApply(t, svc, awaiting)
	id := parkAtCI(t, svc, "check_suite.completed")

	r, err := svc.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Steps[1].Await; a == nil || a.Key != "abc" || a.State != store.AwaitWaiting || a.Deadline == "" || a.Event != "check_suite.completed" {
		t.Fatalf("%+v", r.Steps[1])
	}
	var br *BadRequest
	for _, tc := range []struct {
		step    string
		payload map[string]any
		want    string
	}{
		{"ci", nil, "payload is required"},
		{"ci", map[string]any{"head_sha": "abc", "extra": 1}, `does not declare payload field "extra"`},
		{"build", map[string]any{}, "not an await step"},
	} {
		if _, err := svc.DeliverAwait(ctx, id, tc.step, tc.payload, "token:me"); !errors.As(err, &br) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s %v: %v", tc.step, tc.payload, err)
		}
	}
	if _, err := svc.DeliverAwait(ctx, id, "nope", map[string]any{}, "token:me"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	woke := fr.woke
	r, err = svc.DeliverAwait(ctx, id, "ci", map[string]any{"head_sha": "abc", "conclusion": "success"}, "token:me")
	if err != nil || fr.woke != woke+1 {
		t.Fatal(err)
	}
	if a := r.Steps[1].Await; a.State != store.AwaitMatched || a.Actor != "token:me" || !strings.HasPrefix(a.DeliveryID, "manual:") || a.DecidedAt == nil {
		t.Fatalf("%+v", a)
	}
	if _, err := svc.DeliverAwait(ctx, id, "ci", map[string]any{}, "token:me"); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolved once: %v", err)
	}

	// The step must be waiting, with its await open, and its event still
	// declared.
	id2 := parkAtCI(t, svc, "check_suite.gone")
	if _, err := svc.DeliverAwait(ctx, id2, "ci", map[string]any{}, "x"); !errors.As(err, &br) || !strings.Contains(err.Error(), "no longer declares") {
		t.Fatal(err)
	}
	if err := svc.store.CancelAwaits(ctx, id2); err != nil {
		t.Fatal(err)
	}
	svc.SetCatalog(nil)
	if _, err := svc.DeliverAwait(ctx, id2, "ci", map[string]any{}, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled: %v", err)
	}
	res, _ := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "awaiting"})
	if _, err := svc.DeliverAwait(ctx, res.RunID, "ci", map[string]any{}, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending: %v", err)
	}
	st, _ := svc.store.GetStepRun(ctx, res.RunID, "ci")
	st.Status = store.StepWaiting
	_ = svc.store.UpdateStepRun(ctx, st)
	if _, err := svc.DeliverAwait(ctx, res.RunID, "ci", map[string]any{}, "x"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "not waiting for an event yet") {
		t.Fatalf("no row: %v", err)
	}
}

// Await steps on a daemon without daemon.public_url warn, without failing.
func TestAwaitWarnings(t *testing.T) {
	svc, _ := newService(t)
	info, err := svc.ApplyWorkflow(context.Background(), awaiting, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Warnings) != 1 || info.Warnings[0].Path != "/steps/1/await" || !strings.Contains(info.Warnings[0].Message, "step ci awaits github.check_suite.completed") || !strings.Contains(info.Warnings[0].Hint, "icaro runs deliver") {
		t.Fatalf("%v", info.Warnings)
	}
	if w := svc.awaitWarnings(nil); w != nil {
		t.Fatal(w)
	}
	svc.publicURL = "https://icaro.example"
	if info, _ := svc.ApplyWorkflow(context.Background(), awaiting, "t"); len(info.Warnings) != 0 {
		t.Fatalf("%v", info.Warnings)
	}
}
