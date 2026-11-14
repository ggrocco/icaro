package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/workflow"
)

func TestAgentStepOnRun(t *testing.T) {
	svc, _ := newService(t)
	svc.agents = &workflow.AgentOptions{Profiles: []workflow.AgentProfile{{Name: "reviewer", Agent: "claude"}}}
	ctx := context.Background()
	y := `version: 1
name: review
triggers: [{type: manual}]
steps:
  - id: review
    agent:
      prompt: "Review it"
      params: {model: sonnet}
  - id: quiet
    agent: {profile: gone, prompt: x, memory: false}
`
	// "gone" is not listed: the workflow cannot be saved through the
	// service, so store it directly (it was listed when it was saved).
	if _, issues, _ := svc.ValidateWorkflow(ctx, y); len(issues) == 0 {
		t.Fatal("an unlisted profile validated")
	}
	svc.agents.Profiles = append(svc.agents.Profiles, workflow.AgentProfile{Name: "gone", Agent: "codex"})
	if _, err := svc.ApplyWorkflow(ctx, y, "alice"); err != nil {
		t.Fatal(err)
	}
	svc.agents.Profiles = svc.agents.Profiles[:1]
	run, err := svc.StartRun(ctx, "review", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	row, _ := svc.store.GetStepRun(ctx, run.ID, "review")
	row.Profile, row.ImageDigest, row.LogPath = "reviewer", "reg/a@sha256:1", filepath.Join(t.TempDir(), "review.1.log")
	row.Usage.Input, row.Usage.Output, row.Usage.CostMicro = 10, 3, 2500
	_ = os.WriteFile(strings.TrimSuffix(row.LogPath, ".log")+".prompt.md", []byte("Review it"), 0o600)
	if err := svc.store.UpdateStepRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	info, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := info.Steps[0].Agent
	if a == nil || a.Profile != "reviewer" || a.CLI != "claude" || a.ImageDigest != "reg/a@sha256:1" || a.Params["model"] != "sonnet" || !a.Memory ||
		a.Prompt != "Review it" || a.Usage.Input != 10 || a.Usage.CostUSD == nil || *a.Usage.CostUSD != 0.0025 {
		t.Fatalf("%+v", a)
	}
	// A step that has not run: its profile from the spec, no CLI once the
	// profile is no longer listed, no cost.
	q := info.Steps[1].Agent
	if q == nil || q.Profile != "gone" || q.CLI != "" || q.Memory || q.Usage.CostUSD != nil || q.Prompt != "" {
		t.Fatalf("%+v", q)
	}
	if got := svc.AgentProfiles(); len(got) != 1 || got[0].Name != "reviewer" {
		t.Fatal(got)
	}
	spec, _, _ := svc.ValidateWorkflow(ctx, strings.Replace(y, "[{type: manual}]", "[{integration: github, event: issues.opened}]", 1))
	if ws := svc.Warnings(ctx, spec); len(ws) != 0 {
		t.Fatal("no trigger reference, no warning")
	}
}
