package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/store"
)

func TestAgentStepsProvider(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	w, _ := st.ApplyWorkflow(ctx, "review", "x", "{}", "a")
	r := &store.Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual"}
	if err := st.CreateRun(ctx, r, []store.StepInit{{StepID: "sub", Idx: 0, Kind: "agent"}, {StepID: "api", Idx: 1, Kind: "agent"}, {StepID: "zero", Idx: 2, Kind: "agent"}}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	for id, u := range map[string]store.StepUsage{"sub": {Input: 100, Output: 20}, "api": {Input: 50, Output: 5, CacheRead: 7, CostMicro: 12500}, "zero": {}} {
		row, _ := st.GetStepRun(ctx, r.ID, id)
		row.StartedAt, row.Profile, row.Usage = &day, "reviewer", u
		if err := st.UpdateStepRun(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := (&AgentSteps{Store: st}).Collect(ctx, day.Add(-time.Hour), time.UTC)
	if err != nil || len(recs) != 2 {
		t.Fatalf("%+v %v", recs, err)
	}
	by := map[string]Record{}
	for _, rec := range recs {
		by[rec.Source] = rec
	}
	sub := by["review run "+r.ID+" step sub"]
	api := by["review run "+r.ID+" step api"]
	if sub.InputTokens != 100 || sub.CostUSD != nil || sub.Day != "2026-11-10" || sub.Model != "profile reviewer" || sub.Provider != "agent-steps" {
		t.Fatalf("subscription step %+v", sub)
	}
	if api.CostUSD == nil || *api.CostUSD != 0.0125 || api.CacheReadTokens != 7 {
		t.Fatalf("inject step %+v", api)
	}
	// The report includes them when it has a store.
	rep, err := BuildReport(ctx, ReportOptions{Store: st, Since: "2026-11-01", Timezone: "UTC", Now: day.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rec := range rep.Records {
		if rec.Provider == "agent-steps" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%+v", rep.Records)
	}
	_ = st.Close()
	if _, err := (&AgentSteps{Store: st}).Collect(ctx, day, time.UTC); err == nil {
		t.Fatal("closed store")
	}
}
