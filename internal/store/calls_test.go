package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Call steps (ADR 0038): a child is booked on its parent's step in the
// enqueue's transaction, under the root's budget; cancelling goes down
// the tree, finishing wakes the parent, and replace leaves children alone.
func TestCallRuns(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		parentWF, _ := s.ApplyWorkflow(ctx, "parent", "x", "{}", "a")
		childWF, _ := s.ApplyWorkflow(ctx, "child", "x", "{}", "a")
		root := &Run{WorkflowID: parentWF.ID, WorkflowName: parentWF.Name, WorkflowVersion: 1, TriggerKind: "manual"}
		if _, err := s.EnqueueRun(ctx, root, []StepInit{{StepID: "call", Kind: "call"}, {StepID: "other", Idx: 1, Kind: "image"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimRun(ctx, "daemon"); err != nil {
			t.Fatal(err)
		}
		child := func(policy string, budget int) (*Run, PolicyOutcome, error) {
			r := &Run{WorkflowID: childWF.ID, WorkflowName: childWF.Name, WorkflowVersion: childWF.Version, TriggerKind: "call",
				RootRunID: root.ID, ParentRunID: root.ID, Depth: 1, Concurrency: policy, Actor: "run:" + root.ID}
			out, err := s.EnqueueCall(ctx, r, []StepInit{{StepID: "work", Kind: "image"}}, CallBooking{ParentRunID: root.ID, StepID: "call", MaxRunsPerRoot: budget})
			return r, out, err
		}
		c1, _, err := child("allow", 3)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := s.GetStepRun(ctx, root.ID, "call")
		if st.ChildRunID != c1.ID || st.ChildVersion != 1 {
			t.Fatalf("booked %+v", st)
		}
		got, _ := s.GetRun(ctx, c1.ID)
		if got.RootRunID != root.ID || got.ParentRunID != root.ID || got.Depth != 1 || got.OutputsJSON != "{}" {
			t.Fatalf("lineage %+v", got)
		}
		// The budget counts every run of the tree, the root included.
		if _, _, err := child("allow", 3); err != nil {
			t.Fatal(err)
		}
		if _, _, err := child("allow", 3); !errors.Is(err, ErrRootBudget) {
			t.Fatalf("budget: %v", err)
		}
		if tree, _ := s.ListRuns(ctx, RunFilter{RootRunID: root.ID}); len(tree) != 3 {
			t.Fatalf("tree %d", len(tree))
		}
		// forbid sees the active children and starts nothing.
		if _, out, err := child("forbid", 10); err != nil || out.Blocking == nil {
			t.Fatalf("forbid %+v %v", out, err)
		}
		// A missing step cannot be booked, and the child is rolled back.
		r := &Run{WorkflowID: childWF.ID, WorkflowName: childWF.Name, WorkflowVersion: 1, RootRunID: root.ID, ParentRunID: root.ID}
		if _, err := s.EnqueueCall(ctx, r, nil, CallBooking{ParentRunID: root.ID, StepID: "nope", MaxRunsPerRoot: 10}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no step: %v", err)
		}
		if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("the child was not rolled back")
		}

		// replace leaves runs a parent waits on alone.
		standalone := &Run{WorkflowID: childWF.ID, WorkflowName: childWF.Name, WorkflowVersion: 1, TriggerKind: "manual"}
		if _, err := s.EnqueueRun(ctx, standalone, nil); err != nil {
			t.Fatal(err)
		}
		rep := &Run{WorkflowID: childWF.ID, WorkflowName: childWF.Name, WorkflowVersion: 1, TriggerKind: "manual", Concurrency: "replace"}
		out, err := s.EnqueueRun(ctx, rep, nil)
		if err != nil || len(out.Replaced) != 1 || out.Replaced[0].ID != standalone.ID {
			t.Fatalf("replace %+v %v", out, err)
		}
		if got, _ := s.GetRun(ctx, c1.ID); got.Status != RunQueued || got.CancelRequestedAt != nil {
			t.Fatalf("a called run was replaced: %+v", got)
		}
		_, _ = s.RequestCancel(ctx, rep.ID)

		// The root parks; a child that finishes wakes it in the same
		// transaction, with its outputs.
		if err := s.ParkRun(ctx, root.ID, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimDueRun(ctx, "daemon"); !errors.Is(err, ErrNotFound) {
			t.Fatal("not due yet")
		}
		if err := s.FinishRunOutputs(ctx, c1.ID, RunSucceeded, "", `{"passed":"yes"}`); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetRun(ctx, c1.ID); got.OutputsJSON != `{"passed":"yes"}` {
			t.Fatalf("outputs %q", got.OutputsJSON)
		}
		if got, _ := s.GetRun(ctx, root.ID); got.WakeAt == nil {
			t.Fatal("the parent was not woken")
		}
		if claimed, err := s.ClaimDueRun(ctx, "daemon"); err != nil || claimed.ID != root.ID {
			t.Fatal(claimed, err)
		}
		if err := s.FinishRunOutputs(ctx, "nope", RunSucceeded, "", ""); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}

// Cancelling a run cancels its active descendants first: queued ones
// outright (waking their parents), running and waiting ones flagged.
func TestCancelDownTheTree(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, _ := s.ApplyWorkflow(ctx, "tree", "x", "{}", "a")
		mk := func(parent *Run) *Run {
			t.Helper()
			r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
			steps := []StepInit{{StepID: "call", Kind: "call"}}
			if parent == nil {
				if _, err := s.EnqueueRun(ctx, r, steps); err != nil {
					t.Fatal(err)
				}
				return r
			}
			r.RootRunID, r.ParentRunID, r.Depth = parent.RootRunID, parent.ID, parent.Depth+1
			if _, err := s.EnqueueCall(ctx, r, steps, CallBooking{ParentRunID: parent.ID, StepID: "call", MaxRunsPerRoot: 10}); err != nil {
				t.Fatal(err)
			}
			return r
		}
		claim := func(want *Run) {
			t.Helper()
			if got, err := s.ClaimRun(ctx, "daemon"); err != nil || got.ID != want.ID {
				t.Fatal(got, err)
			}
		}
		root := mk(nil)
		claim(root)
		mid := mk(root)
		claim(mid)
		leaf := mk(mid)
		if err := s.ParkRun(ctx, root.ID, nil); err != nil {
			t.Fatal(err)
		}
		if d, _ := s.ActiveDescendants(ctx, root.ID); len(d) != 2 || d[0].ID != mid.ID || d[1].ID != leaf.ID {
			t.Fatalf("descendants %+v", d)
		}
		if _, err := s.RequestCancel(ctx, root.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetRun(ctx, leaf.ID); got.Status != RunCancelled {
			t.Fatalf("queued leaf %s", got.Status)
		}
		if got, _ := s.GetRun(ctx, mid.ID); got.Status != RunRunning || got.CancelRequestedAt == nil {
			t.Fatalf("running mid %+v", got)
		}
		if got, _ := s.GetRun(ctx, root.ID); got.CancelRequestedAt == nil || got.WakeAt == nil {
			t.Fatalf("waiting root %+v", got)
		}
		// Cancelling again is harmless.
		if _, err := s.RequestCancel(ctx, root.ID); err != nil {
			t.Fatal(err)
		}
		// A cancelling run that parks to wait for its children is claimed
		// at its wake time, not at once.
		if _, err := s.ClaimDueRun(ctx, "daemon"); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(time.Hour)
		if err := s.ParkRun(ctx, root.ID, &later); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimDueRun(ctx, "daemon"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a cancelling run waiting for children was claimed: %v", err)
		}
		// The mid run finishing wakes it.
		if err := s.FinishRun(ctx, mid.ID, RunCancelled, "cancelled"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ClaimDueRun(ctx, "daemon"); err != nil || got.ID != root.ID {
			t.Fatal(got, err)
		}

		// A queued child cancelled by hand wakes its parked parent.
		other := mk(nil)
		claim(other)
		kid := mk(other)
		if err := s.ParkRun(ctx, other.ID, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RequestCancel(ctx, kid.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetRun(ctx, other.ID); got.WakeAt == nil || got.CancelRequestedAt != nil {
			t.Fatalf("parent %+v", got)
		}
	})
}

// A failure inside a call's transaction rolls it back: the booking on the
// parent's step, and the parent's wake when a child finishes.
func TestCallTransactionsRollBack(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, SQLite, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	w, _ := s.ApplyWorkflow(ctx, "tx", "x", "{}", "a")
	parent := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1}
	if _, err := s.EnqueueRun(ctx, parent, []StepInit{{StepID: "call", Kind: "call"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TRIGGER no_booking BEFORE UPDATE OF child_run_id ON step_runs WHEN NEW.child_run_id = 'boom' BEGIN SELECT RAISE(ABORT, 'booking refused'); END`,
		`CREATE TRIGGER no_wake BEFORE UPDATE OF wake_at ON runs WHEN OLD.status = 'waiting' AND NEW.wake_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'wake refused'); END`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	child := &Run{ID: "boom", WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, RootRunID: parent.ID, ParentRunID: parent.ID, Depth: 1}
	if _, err := s.EnqueueCall(ctx, child, nil, CallBooking{ParentRunID: parent.ID, StepID: "call", MaxRunsPerRoot: 10}); err == nil || !strings.Contains(err.Error(), "booking refused") {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, "boom"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the child survived its booking's failure")
	}
	child.ID = "kid"
	if _, err := s.EnqueueCall(ctx, child, nil, CallBooking{ParentRunID: parent.ID, StepID: "call", MaxRunsPerRoot: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.ParkRun(ctx, parent.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, child.ID, RunSucceeded, ""); err == nil || !strings.Contains(err.Error(), "wake refused") {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, child.ID); got.Status == RunSucceeded {
		t.Fatal("the child's end survived its parent's wake failing")
	}
	// Cancelling goes down the tree in one transaction: a child that
	// cannot be cancelled leaves its parent uncancelled too.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER no_cancel BEFORE UPDATE OF status ON runs WHEN OLD.id = 'kid' BEGIN SELECT RAISE(ABORT, 'cancel refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestCancel(ctx, parent.ID); err == nil || !strings.Contains(err.Error(), "cancel refused") {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, parent.ID); got.CancelRequestedAt != nil {
		t.Fatal("the parent was flagged although its child was not cancelled")
	}
}
