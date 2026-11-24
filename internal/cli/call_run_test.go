package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"icaro/internal/service"
)

func TestPrintCallStepAndLineage(t *testing.T) {
	run := &service.RunInfo{ID: "c1", Workflow: "run-tests", Version: 2, Status: "succeeded", TriggerKind: "call",
		RootRunID: "r1", ParentRunID: "p1", Depth: 2, Outputs: json.RawMessage(`{ "passed": "yes" }`),
		Steps: []service.StepInfo{
			{ID: "inner", Kind: "call", Status: "succeeded", Attempt: 1, Outputs: json.RawMessage(`{}`),
				Call: &service.CallStepInfo{Workflow: "lint", RunID: "k1", Status: "succeeded", Version: 3}},
			{ID: "later", Kind: "call", Status: "pending", Outputs: json.RawMessage(`{}`), Call: &service.CallStepInfo{Workflow: "lint"}},
		}}
	var b strings.Builder
	printRun(&b, run)
	out := b.String()
	for _, want := range []string{"called by run p1 (root r1, depth 2)", `outputs: {"passed":"yes"}`,
		"call step inner: workflow lint v3, child run k1 (succeeded)\n  icaro runs get k1", "call step later: workflow lint, not started"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printRun(&b, &service.RunInfo{ID: "r", Outputs: json.RawMessage(`{}`)})
	if strings.Contains(b.String(), "outputs:") || strings.Contains(b.String(), "called by") {
		t.Fatal(b.String())
	}
	if compactJSON(json.RawMessage("not json")) != "not json" {
		t.Fatal("compactJSON keeps what it cannot compact")
	}
}

func TestTreeOrder(t *testing.T) {
	runs := []*service.RunInfo{ // newest first, as the API lists them
		{ID: "c2", ParentRunID: "r", CreatedAt: "4"},
		{ID: "g1", ParentRunID: "c1", CreatedAt: "3"},
		{ID: "c1", ParentRunID: "r", CreatedAt: "2"},
		{ID: "r", CreatedAt: "1"},
		{ID: "orphan", ParentRunID: "gone", CreatedAt: "0"},
	}
	var ids []string
	for _, r := range treeOrder(runs) {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "orphan,r,c1,g1,c2" {
		t.Fatal(got)
	}
}
