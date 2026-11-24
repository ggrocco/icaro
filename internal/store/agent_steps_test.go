package store

import (
	"context"
	"testing"
	"time"
)

func TestAgentStepFieldsAndUsage(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, _ := s.ApplyWorkflow(ctx, "wf", "x", "{}", "a")
		r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
		if err := s.CreateRun(ctx, r, []StepInit{{StepID: "review", Idx: 0, Kind: "agent"}, {StepID: "post", Idx: 1, Kind: "image"}}); err != nil {
			t.Fatal(err)
		}
		row, _ := s.GetStepRun(ctx, r.ID, "review")
		started := time.Now().Add(-time.Minute)
		row.Status, row.StartedAt, row.Profile, row.ImageDigest = StepSucceeded, &started, "reviewer", "reg/img@sha256:1"
		row.Usage = StepUsage{Input: 10, Output: 2, CacheRead: 3, CacheWrite: 4, CostMicro: 500}
		if err := s.UpdateStepRun(ctx, row); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetStepRun(ctx, r.ID, "review")
		if err != nil || got.Profile != "reviewer" || got.ImageDigest != "reg/img@sha256:1" || got.Usage != row.Usage {
			t.Fatalf("%+v %v", got, err)
		}
		// An image step without a profile is not an agent step's usage.
		post, _ := s.GetStepRun(ctx, r.ID, "post")
		post.StartedAt = &started
		_ = s.UpdateStepRun(ctx, post)
		us, err := s.ListAgentStepUsage(ctx, started.Add(-time.Second))
		if err != nil || len(us) != 1 || us[0].Workflow != "wf" || us[0].StepID != "review" || us[0].Usage.CostMicro != 500 || us[0].StartedAt.Unix() != started.Unix() {
			t.Fatalf("%+v %v", us, err)
		}
		if us, _ := s.ListAgentStepUsage(ctx, time.Now().Add(time.Hour)); len(us) != 0 {
			t.Fatal("since")
		}
	})
}
