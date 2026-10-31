package api

import (
	"context"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/store"
)

func TestApprovalEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.admin.ApplyWorkflow(ctx, "version: 1\nname: gated\nsteps:\n  - {id: gate, approval: {message: \"ship?\"}}\n"); err != nil {
		t.Fatal(err)
	}
	park := func() string {
		run, err := e.admin.Run(ctx, "gated", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.ClaimRun(ctx, "daemon"); err != nil {
			t.Fatal(err)
		}
		st, _ := e.st.GetStepRun(ctx, run.ID, "gate")
		now := store.Now()
		st.Status, st.Attempt, st.StartedAt = store.StepWaiting, 1, &now
		if err := e.st.UpdateStepRun(ctx, st); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.OpenApproval(ctx, run.ID, "gate", "ship?"); err != nil {
			t.Fatal(err)
		}
		if err := e.st.ParkRun(ctx, run.ID, nil); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}

	id := park()
	if _, err := e.reader.DecideApproval(ctx, id, "gate", true, ""); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a read token cannot approve: %v", err)
	}
	r, err := e.admin.DecideApproval(ctx, id, "gate", true, "lgtm")
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Steps[0].Approval; r.Status != store.RunWaiting || a == nil || a.Decision != store.ApprovalApproved || a.Actor != "token:admin" || a.Comment != "lgtm" {
		t.Fatalf("%+v %+v", r, a)
	}
	var apiErr *apiclient.Error
	if _, err := e.admin.DecideApproval(ctx, id, "gate", false, ""); !errorsAs(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("decided twice: %v", err)
	}
	if _, err := e.admin.DecideApproval(ctx, id, "nope", true, ""); !apiclient.IsNotFound(err) {
		t.Fatalf("no such step: %v", err)
	}

	id = park()
	r, err = e.admin.DecideApproval(ctx, id, "gate", false, "")
	if err != nil || r.Steps[0].Approval.Decision != store.ApprovalRejected {
		t.Fatalf("%v %+v", err, r)
	}
}
