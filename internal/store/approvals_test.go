package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A parked run gives its slot back, counts as active, and is claimed again
// when it is due, decided, or asked to cancel.
func TestParkAndWakeRuns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, _ := s.ApplyWorkflow(ctx, "park", "x", "{}", "a")
		steps := []StepInit{{StepID: "gate", Idx: 0, Kind: "approval"}}
		enq := func(policy string) *Run {
			t.Helper()
			r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual", Concurrency: policy}
			if _, err := s.EnqueueRun(ctx, r, steps); err != nil {
				t.Fatal(err)
			}
			return r
		}
		run := enq("queue")
		claimed, err := s.ClaimRun(ctx, "daemon")
		if err != nil || claimed.ID != run.ID {
			t.Fatal(claimed, err)
		}
		started := claimed.StartedAt

		// Parked until a decision: nothing is due.
		if err := s.ParkRun(ctx, run.ID, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.ParkRun(ctx, run.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("only a running run parks: %v", err)
		}
		if got, _ := s.GetRun(ctx, run.ID); got.Status != RunWaiting || got.WakeAt != nil {
			t.Fatalf("%+v", got)
		}
		if _, err := s.ClaimDueRun(ctx, "daemon"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("not due: %v", err)
		}
		// Still active: a queue-policy run waits behind it, and forbid
		// sees it.
		next := enq("queue")
		if _, err := s.ClaimRun(ctx, "daemon"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a queue run waits behind a waiting run: %v", err)
		}
		if active, _ := s.ActiveRuns(ctx, w.ID); len(active) != 2 || active[0].ID != run.ID {
			t.Fatalf("active: %+v", active)
		}

		// A decision wakes it; the claim keeps its start time.
		if _, err := s.OpenApproval(ctx, run.ID, "gate", "ship it?"); err != nil {
			t.Fatal(err)
		}
		if a, err := s.OpenApproval(ctx, run.ID, "gate", "a resumed run keeps the first message"); err != nil || a.Message != "ship it?" || a.Decision != "" {
			t.Fatalf("%+v %v", a, err)
		}
		a, err := s.DecideApproval(ctx, run.ID, "gate", ApprovalApproved, "alice", "lgtm")
		if err != nil || a.Decision != ApprovalApproved || a.Actor != "alice" || a.Comment != "lgtm" || a.DecidedAt == nil {
			t.Fatalf("%+v %v", a, err)
		}
		if _, err := s.DecideApproval(ctx, run.ID, "gate", ApprovalRejected, "bob", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("decided once: %v", err)
		}
		if _, err := s.DecideApproval(ctx, run.ID, "nope", ApprovalApproved, "bob", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no such approval: %v", err)
		}
		woken, err := s.ClaimDueRun(ctx, "daemon")
		if err != nil || woken.ID != run.ID || woken.Status != RunRunning || woken.WakeAt != nil ||
			woken.StartedAt == nil || !woken.StartedAt.Equal(*started) {
			t.Fatalf("%+v %v", woken, err)
		}
		if list, _ := s.ListApprovals(ctx, run.ID); len(list) != 1 || list[0].StepID != "gate" {
			t.Fatalf("%+v", list)
		}

		// A wake time in the future is not due; one in the past is.
		soon := Now().Add(time.Hour)
		if err := s.ParkRun(ctx, run.ID, &soon); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimDueRun(ctx, "daemon"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("not yet due: %v", err)
		}
		// WakeRun makes it due now.
		if err := s.WakeRun(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetRun(ctx, run.ID); got.WakeAt == nil || got.WakeAt.After(Now()) {
			t.Fatalf("woken: %+v", got.WakeAt)
		}
		soon = Now().Add(time.Hour)
		if _, err := s.ClaimDueRun(ctx, "daemon"); err != nil {
			t.Fatal(err)
		}
		if err := s.ParkRun(ctx, run.ID, &soon); err != nil {
			t.Fatal(err)
		}
		// A cancel request wakes it at once.
		if got, err := s.RequestCancel(ctx, run.ID); err != nil || got.Status != RunWaiting || got.CancelRequestedAt == nil {
			t.Fatalf("%+v %v", got, err)
		}
		if woken, err := s.ClaimDueRun(ctx, "daemon"); err != nil || woken.ID != run.ID || woken.CancelRequestedAt == nil {
			t.Fatalf("%+v %v", woken, err)
		}
		if err := s.FinishRun(ctx, run.ID, RunCancelled, "cancelled"); err != nil {
			t.Fatal(err)
		}
		if c, err := s.ClaimRun(ctx, "daemon"); err != nil || c.ID != next.ID {
			t.Fatalf("the queued run goes once the waiting one ends: %+v %v", c, err)
		}
	})
}
