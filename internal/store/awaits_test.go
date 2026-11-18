package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An await row opens once, resolves once by compare-and-set, and a match
// wakes its parked run; cancelling leaves resolved rows alone.
func TestAwaits(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, _ := s.ApplyWorkflow(ctx, "await", "x", "{}", "a")
		r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
		if _, err := s.EnqueueRun(ctx, r, []StepInit{{StepID: "ci", Idx: 0, Kind: "await"}, {StepID: "pr", Idx: 1, Kind: "await"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimRun(ctx, "daemon"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		a, err := s.OpenAwait(ctx, &StepAwait{RunID: r.ID, StepID: "ci", Integration: "github", Event: "check_suite.completed", Workflow: "await", Key: "abc", Deadline: deadline})
		if err != nil || a.State != AwaitWaiting || a.Key != "abc" || !a.Deadline.Equal(deadline) || a.DecidedAt != nil {
			t.Fatalf("%+v %v", a, err)
		}
		if again, _ := s.OpenAwait(ctx, &StepAwait{RunID: r.ID, StepID: "ci", Integration: "github", Event: "check_suite.completed", Key: "other", Deadline: deadline}); again.Key != "abc" {
			t.Fatalf("a resumed run keeps the first key: %+v", again)
		}
		if _, err := s.OpenAwait(ctx, &StepAwait{RunID: r.ID, StepID: "pr", Integration: "github", Event: "pull_request.opened", Workflow: "await", Key: "b", Deadline: deadline}); err != nil {
			t.Fatal(err)
		}
		if list, _ := s.ListWaitingAwaits(ctx, "github", "check_suite.completed"); len(list) != 1 || list[0].StepID != "ci" {
			t.Fatalf("%+v", list)
		}
		if err := s.ParkRun(ctx, r.ID, nil); err != nil {
			t.Fatal(err)
		}

		got, err := s.ResolveAwait(ctx, r.ID, "ci", AwaitMatched, "d1", `{"conclusion":"success"}`, "")
		if err != nil || got.State != AwaitMatched || got.DeliveryID != "d1" || got.PayloadJSON == "" || got.DecidedAt == nil {
			t.Fatalf("%+v %v", got, err)
		}
		if run, _ := s.GetRun(ctx, r.ID); run.WakeAt == nil {
			t.Fatal("a match wakes the parked run")
		}
		if _, err := s.ResolveAwait(ctx, r.ID, "ci", AwaitMatched, "d2", "{}", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("resolved once: %v", err)
		}
		if _, err := s.ResolveAwait(ctx, r.ID, "nope", AwaitMatched, "d2", "{}", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no such await: %v", err)
		}
		if list, _ := s.ListWaitingAwaits(ctx, "github", "check_suite.completed"); len(list) != 0 {
			t.Fatalf("a matched step is no candidate: %+v", list)
		}

		if err := s.CancelAwaits(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListAwaits(ctx, r.ID)
		if err != nil || len(list) != 2 || list[0].State != AwaitMatched || list[1].State != AwaitCancelled || list[1].DecidedAt == nil {
			t.Fatalf("%+v %v", list, err)
		}
		if _, err := s.ResolveAwait(ctx, r.ID, "pr", AwaitExpired, "", "", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("cancelled is final: %v", err)
		}
		if _, err := s.GetAwait(ctx, r.ID, "zz"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}

		// Deleting the run removes its awaits.
		if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM runs WHERE id = ?`), r.ID); err != nil {
			t.Fatal(err)
		}
		if list, _ := s.ListAwaits(ctx, r.ID); len(list) != 0 {
			t.Fatalf("%+v", list)
		}
	})
}

// Delivery events are listed oldest first from a point in time, narrowed
// to a generic webhook's workflow when asked, and pruned with their
// delivery.
func TestDeliveryEvents(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Now().UTC().Add(-2 * time.Hour)
		add := func(age time.Duration, workflow string, payloads ...string) *Delivery {
			t.Helper()
			d := &Delivery{Integration: "webhook", Workflow: workflow, Method: "POST", HeadersJSON: "{}", Verified: true, Status: DeliveryProcessed, ReceivedAt: t0.Add(age)}
			if err := s.CreateDelivery(ctx, d); err != nil {
				t.Fatal(err)
			}
			var evs []DeliveryEvent
			for i, p := range payloads {
				evs = append(evs, DeliveryEvent{DeliveryID: d.ID, Seq: i, Integration: "webhook", Event: "received", Workflow: workflow, PayloadJSON: p, ReceivedAt: d.ReceivedAt})
			}
			if err := s.AddDeliveryEvents(ctx, evs); err != nil {
				t.Fatal(err)
			}
			return d
		}
		if err := s.AddDeliveryEvents(ctx, nil); err != nil {
			t.Fatal(err)
		}
		old := add(0, "a", `{"n":0}`)
		add(time.Hour, "b", `{"n":1}`)
		add(90*time.Minute, "a", `{"n":2}`, `{"n":3}`)

		all, err := s.ListDeliveryEventsSince(ctx, "webhook", "received", "", t0.Add(time.Minute))
		if err != nil || len(all) != 3 || all[0].PayloadJSON != `{"n":1}` || all[2].Seq != 1 {
			t.Fatalf("%+v %v", all, err)
		}
		mine, _ := s.ListDeliveryEventsSince(ctx, "webhook", "received", "a", t0)
		if len(mine) != 3 || mine[0].DeliveryID != old.ID || mine[1].PayloadJSON != `{"n":2}` || mine[0].ReceivedAt.IsZero() {
			t.Fatalf("%+v", mine)
		}
		if none, _ := s.ListDeliveryEventsSince(ctx, "github", "received", "", t0); len(none) != 0 {
			t.Fatalf("%+v", none)
		}
		// A default received time is now.
		d := &Delivery{Integration: "github", Method: "POST", HeadersJSON: "{}", Verified: true, Status: DeliveryProcessed}
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
		if err := s.AddDeliveryEvents(ctx, []DeliveryEvent{{DeliveryID: d.ID, Integration: "github", Event: "push", PayloadJSON: "{}"}}); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ListDeliveryEventsSince(ctx, "github", "push", "", time.Now().Add(-time.Minute)); len(got) != 1 {
			t.Fatalf("%+v", got)
		}
		if err := s.AddDeliveryEvents(ctx, []DeliveryEvent{{DeliveryID: "missing", Integration: "github", Event: "push", PayloadJSON: "{}"}}); err == nil {
			t.Fatal("an event needs its delivery")
		}

		if _, err := s.PruneDeliveries(ctx, t0.Add(30*time.Minute), 0); err != nil {
			t.Fatal(err)
		}
		if left, _ := s.ListDeliveryEventsSince(ctx, "webhook", "received", "a", t0); len(left) != 2 {
			t.Fatalf("pruned with its delivery: %+v", left)
		}
	})
}
