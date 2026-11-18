package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"icaro/internal/service"
)

func TestPrintAgentStep(t *testing.T) {
	cost := 0.0125
	run := &service.RunInfo{ID: "r1", Workflow: "review", Version: 1, Status: "succeeded", Steps: []service.StepInfo{
		{ID: "review", Kind: "agent", Status: "succeeded", Attempt: 1, Outputs: json.RawMessage(`{"result":"ok"}`), Agent: &service.AgentStepInfo{
			Profile: "reviewer", CLI: "claude", ImageDigest: "reg/a@sha256:1", Params: map[string]any{"model": "sonnet", "max_turns": 30},
			Memory: false, Prompt: "Review\nit", Usage: service.AgentUsageInfo{Input: 10, Output: 3, CacheRead: 2, CostUSD: &cost},
		}},
		{ID: "later", Kind: "agent", Status: "pending", Outputs: json.RawMessage(`{}`), Agent: &service.AgentStepInfo{Profile: "gone", Memory: true}},
	}}
	var b strings.Builder
	printRun(&b, run)
	out := b.String()
	for _, want := range []string{"agent step review: profile reviewer (claude), image reg/a@sha256:1", "params: max_turns=30 model=sonnet", "memory: off",
		"tokens: 12 input, 3 output, ~$0.0125 (estimate)", "prompt:\n    Review\n    it", "agent step later: profile gone (?)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "agent step later: profile gone (?), image") || strings.Count(out, "tokens:") != 1 {
		t.Fatal(out)
	}
}
