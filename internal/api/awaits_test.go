package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/store"
)

// Resolving an await by hand needs the write scope and records the
// caller; the run shows the step's await state.
func TestDeliverEndpoint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const yaml = "version: 1\nname: awaits\nsteps:\n  - {id: ci, await: {integration: github, event: push, key: k, match: event.sha}, timeout: 1h}\n"
	if v, err := e.admin.Validate(ctx, yaml); err != nil || !v.Valid || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0].Message, "daemon.public_url") {
		t.Fatalf("an await on an unreachable daemon warns: %+v %v", v, err)
	}
	if _, err := e.admin.ApplyWorkflow(ctx, yaml); err != nil {
		t.Fatal(err)
	}
	run, err := e.admin.Run(ctx, "awaits", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ClaimRun(ctx, "daemon"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.st.GetStepRun(ctx, run.ID, "ci")
	now := store.Now()
	st.Status, st.Attempt, st.StartedAt = store.StepWaiting, 1, &now
	if err := e.st.UpdateStepRun(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.OpenAwait(ctx, &store.StepAwait{RunID: run.ID, StepID: "ci", Integration: "github", Event: "push", Workflow: "awaits", Key: "k", Match: "event.sha", Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reader.DeliverAwait(ctx, run.ID, "ci", map[string]any{"sha": "k"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a read token cannot resolve an await: %v", err)
	}
	var apiErr *apiclient.Error
	if _, err := e.admin.DeliverAwait(ctx, run.ID, "ci", nil); !errorsAs(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("a payload is required: %v", err)
	}
	r, err := e.admin.DeliverAwait(ctx, run.ID, "ci", map[string]any{"sha": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Steps[0].Await; a == nil || a.State != store.AwaitMatched || a.Actor != "token:admin" || !strings.HasPrefix(a.DeliveryID, "manual:") || a.Key != "k" {
		t.Fatalf("%+v", r.Steps[0])
	}
	if _, err := e.admin.DeliverAwait(ctx, run.ID, "ci", map[string]any{}); !errorsAs(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("resolved once: %v", err)
	}
}
