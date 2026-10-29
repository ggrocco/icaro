package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"icaro/internal/workflow"
)

const rerunV1 = `
version: 1
name: rerun-me
triggers:
  - type: manual
steps:
  - { id: one, image: x, command: "echo ${{ trigger.who }}" }
`

const rerunV2 = `
version: 1
name: rerun-me
triggers:
  - type: manual
steps:
  - { id: one, image: x, command: "echo ${{ trigger.who }}" }
  - { id: two, image: x, needs: [one] }
`

// A re-run repeats the run it was asked for: the version that run used,
// not the workflow's current one, with the same trigger payload.
func TestRerunUsesThePinnedVersion(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.ApplyWorkflow(ctx, rerunV1, "t"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.StartRun(ctx, "rerun-me", map[string]any{"who": "icaro"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := svc.ApplyWorkflow(ctx, rerunV2, "t"); err != nil || info.Version != 2 {
		t.Fatal(info, err)
	}

	again, err := svc.RerunRun(ctx, first.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != 1 || again.RerunOf != first.ID || again.TriggerKind != first.TriggerKind || again.Actor != "bob" {
		t.Fatalf("%+v", again)
	}
	if len(again.Steps) != 1 || again.Steps[0].ID != "one" {
		t.Fatalf("the steps of version 1, not 2: %+v", again.Steps)
	}
	var trig map[string]any
	if err := json.Unmarshal(again.Trigger, &trig); err != nil || trig["who"] != "icaro" {
		t.Fatalf("same trigger: %s %v", again.Trigger, err)
	}

	// A run of the current version re-runs on it too.
	current, err := svc.StartRun(ctx, "rerun-me", map[string]any{"who": "x"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := svc.RerunRun(ctx, current.ID, "t"); err != nil || r.Version != 2 || len(r.Steps) != 2 {
		t.Fatalf("%+v %v", r, err)
	}

	if _, err := svc.RerunRun(ctx, "nope", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "rerun-me", Version: 99}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown version: %v", err)
	}
}

func TestRerunHonorsTheConcurrencyPolicy(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.ApplyWorkflow(ctx, policyWorkflow("forbid"), "t"); err != nil {
		t.Fatal(err)
	}
	run, err := svc.StartRun(ctx, "pol-forbid", nil, "t")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.RerunRun(ctx, run.ID, "t")
	var conflict *Conflict
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "concurrency forbid") {
		t.Fatalf("a re-run of an active forbid workflow is skipped: %v", err)
	}

	// A deleted workflow cannot be re-run, though its runs stay.
	if err := svc.DeleteWorkflow(ctx, "pol-forbid"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RerunRun(ctx, run.ID, "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted workflow: %v", err)
	}
}
