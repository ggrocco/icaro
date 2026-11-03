package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icaro/internal/store"
	"icaro/internal/workflow"
)

const gated = `
version: 1
name: gated
steps:
  - {id: build, image: x}
  - {id: review, needs: [build], approval: {message: "ship?"}, timeout: 2h}
  - {id: soak, needs: [review], wait: 1h}
`

// parkAtReview puts a run of gated where the engine leaves it: running,
// its approval open and waiting, then parked.
func parkAtReview(t *testing.T, svc *Service) string {
	t.Helper()
	ctx := context.Background()
	res, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "gated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	st, err := svc.store.GetStepRun(ctx, res.RunID, "review")
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	st.Status, st.Attempt, st.StartedAt = store.StepWaiting, 1, &now
	if err := svc.store.UpdateStepRun(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.OpenApproval(ctx, res.RunID, "review", "ship?"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ParkRun(ctx, res.RunID, nil); err != nil {
		t.Fatal(err)
	}
	return res.RunID
}

func TestDecideApproval(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	mustApply(t, svc, gated)
	id := parkAtReview(t, svc)

	r, err := svc.GetRun(ctx, id)
	if err != nil || r.Status != store.RunWaiting {
		t.Fatalf("%v %+v", err, r)
	}
	a := r.Steps[1].Approval
	if r.Steps[1].Kind != workflow.KindApproval || a == nil || a.Message != "ship?" || a.Decision != "" || a.Deadline == nil {
		t.Fatalf("%+v %+v", r.Steps[1], a)
	}
	if r.Steps[0].Approval != nil || r.Steps[2].Kind != workflow.KindWait {
		t.Fatalf("%+v", r.Steps)
	}

	// Only a waiting approval step can be decided.
	if _, err := svc.DecideApproval(ctx, id, "nope", true, "alice", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such step: %v", err)
	}
	for _, step := range []string{"build", "soak"} {
		var br *BadRequest
		if _, err := svc.DecideApproval(ctx, id, step, true, "alice", ""); !errors.As(err, &br) {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if _, err := svc.DecideApproval(ctx, id, "review", true, "alice", strings.Repeat("x", maxApprovalComment+1)); err == nil {
		t.Fatal("an oversized comment is refused")
	}

	woke := fr.woke
	r, err = svc.DecideApproval(ctx, id, "review", true, "alice", "go")
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Steps[1].Approval; a.Decision != store.ApprovalApproved || a.Actor != "alice" || a.Comment != "go" || a.DecidedAt == nil || a.Deadline != nil {
		t.Fatalf("%+v", a)
	}
	if fr.woke != woke+1 {
		t.Fatal("a decision wakes the engine")
	}
	if due, err := svc.store.ClaimDueRun(ctx, "daemon"); err != nil || due.ID != id {
		t.Fatalf("a decided approval makes its parked run due: %v", err)
	}
	if _, err := svc.DecideApproval(ctx, id, "review", false, "bob", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("decided twice: %v", err)
	}

	// An approval that expired cannot be decided after.
	id2 := parkAtReview(t, svc)
	if _, err := svc.store.DecideApproval(ctx, id2, "review", store.ApprovalExpired, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, id2, "review", true, "late", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired: %v", err)
	}
	// A step waiting before its approval row is written (the engine is
	// between the two writes) cannot be decided yet.
	if _, err := svc.store.DB().Exec(`DELETE FROM step_approvals WHERE run_id = ?`, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, id2, "review", true, "early", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("no approval row: %v", err)
	}
}

// Cancelling, replacing or deleting a parked run wakes the engine to
// cancel it, instead of reporting it cancelled before it is.
func TestCancelParkedRuns(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	mustApply(t, svc, gated)
	id := parkAtReview(t, svc)
	woke := fr.woke
	r, err := svc.CancelRun(ctx, id)
	if err != nil || r.Status != store.RunWaiting || fr.woke != woke+1 || fr.cancelled != 0 {
		t.Fatalf("%v %+v %+v", err, r, fr)
	}
	if due, err := svc.store.ClaimDueRun(ctx, "daemon"); err != nil || due.ID != id || due.CancelRequestedAt == nil {
		t.Fatalf("%v %+v", err, due)
	}

	// A replaced parked run is woken to cancel itself.
	svc, fr = newService(t)
	mustApply(t, svc, strings.Replace(gated, "name: gated", "name: gated\nconcurrency: replace", 1))
	_ = parkAtReview(t, svc)
	woke = fr.woke
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "gated"}); err != nil {
		t.Fatal(err)
	}
	if fr.cancelled != 0 || fr.woke != woke+2 {
		t.Fatalf("%+v", fr)
	}

	svc, fr = newService(t)
	mustApply(t, svc, gated)
	_ = parkAtReview(t, svc)
	woke = fr.woke
	if err := svc.DeleteWorkflow(ctx, "gated"); err != nil {
		t.Fatal(err)
	}
	if fr.woke != woke+1 {
		t.Fatalf("delete wakes for the parked run: %+v", fr)
	}
}
