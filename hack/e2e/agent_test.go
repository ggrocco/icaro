//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
)

// TestAgentSteps is ADR 0037 against a real daemon and Docker: two agent
// steps run the profile's (fake) claude CLI under per-step home layers,
// the profile's image digest and the step's memory setting; a later
// image step reads their declared outputs and their built-in result.
func TestAgentSteps(t *testing.T) {
	bin, err := filepath.Abs(envOr("ICARO_E2E_BIN", "../../bin/icaro"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip(bin + " not built; run make build")
	}
	if err := command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	buildFakeAgent(t, bin)
	home, err := os.MkdirTemp("/tmp", "iceag-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "config", "icaro.yaml"), `version: 1
profiles:
  reviewer:
    agent: claude
    image: `+FakeAgentImage+`
    egress: []
  injector:
    agent: claude
    image: `+FakeAgentImage+`
    egress: []
    inject: {anthropic: anthropic-key}
workflows:
  agent_profiles: [reviewer, injector]
previews:
  enabled: false
`)
	env := append(os.Environ(), "ICARO_HOME="+home, "ICARO_PASSWORD=e2e-password", "ICARO_CONFIG=")
	run := func(args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test binary
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("icaro %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	t.Cleanup(func() {
		if pid, err := os.ReadFile(filepath.Join(home, "run", "icaro.pid")); err == nil {
			stopDaemon(strings.TrimSpace(string(pid)))
		}
		ns := container.Namespace(home)
		for _, p := range []string{"reviewer", "workflow"} {
			_ = command("docker", "network", "rm", "icaro-egress-"+ns+"-"+p).Run()
		}
		_ = os.RemoveAll(home)
	})
	must("init")
	must("serve", "--background")

	wf := filepath.Join(home, "agents.yaml")
	writeFile(t, wf, `version: 1
name: agents
triggers: [{type: manual}]
steps:
  - id: first
    agent: {prompt: "first task"}
    outputs: {summary: "json:summary"}
  - id: second
    needs: [first]
    agent:
      prompt: "after ${{ steps.first.outputs.result }}"
      memory: false
    outputs: {summary: "json:summary"}
  - id: check
    needs: [second]
    image: mirror.gcr.io/library/busybox:1.36
    command: cat /workspace/agent.txt && echo "${{ steps.first.outputs.summary }} | ${{ steps.second.outputs.summary }} | ${{ steps.first.outputs.result }}"
    outputs: {line: "stdout:last-line"}
`)
	must("workflow", "apply", wf)
	out, err := run("run", "agents", "--wait", "--json")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	var r struct {
		ID    string `json:"id"`
		Steps []struct {
			ID      string         `json:"id"`
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
			Agent   *struct {
				Profile     string `json:"profile"`
				CLI         string `json:"cli"`
				ImageDigest string `json:"image_digest"`
				Prompt      string `json:"prompt"`
				Usage       struct {
					Input int64 `json:"input"`
				} `json:"usage"`
			} `json:"agent"`
		} `json:"steps"`
	}
	got := must("runs", "get", strings.TrimSpace(lastJSONID(out)), "--json")
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	steps := map[string]int{}
	for i, s := range r.Steps {
		steps[s.ID] = i
	}
	first, second, check := r.Steps[steps["first"]], r.Steps[steps["second"]], r.Steps[steps["check"]]
	// Each agent step started from the profile's seed; only the first had
	// memory.
	if first.Outputs["summary"] != "home clean, memory on" || second.Outputs["summary"] != "home clean, memory off" {
		t.Fatalf("summaries %v %v", first.Outputs, second.Outputs)
	}
	if first.Outputs["result"] != "Done: home clean, memory on." {
		t.Fatalf("result %v", first.Outputs)
	}
	if line := check.Outputs["line"]; line != "home clean, memory on | home clean, memory off | Done: home clean, memory on." {
		t.Fatalf("a later step reads outputs and result: %v", line)
	}
	a := first.Agent
	if a == nil || a.Profile != "reviewer" || a.CLI != "claude" || !strings.Contains(a.ImageDigest, "sha256:") || a.Prompt != "first task" || a.Usage.Input != 200 {
		t.Fatalf("agent info %+v", a)
	}
	if second.Agent == nil || second.Agent.ImageDigest != a.ImageDigest || !strings.HasPrefix(second.Agent.Prompt, "after Done") {
		t.Fatalf("second %+v", second.Agent)
	}
	// The step sessions are gone with their home layers.
	if out := must("session", "list", "--json"); strings.Contains(out, `"kind": "step"`) {
		t.Fatalf("step sessions left:\n%s", out)
	}
	if out := must("usage", "--since", "1d"); !strings.Contains(out, "agents run "+r.ID+" step first") {
		t.Fatalf("usage lacks the agent step:\n%s", out)
	}

	// With an inject profile the key stays on the host: the CLI sees a
	// capability token, and no container, log, row or response has the key.
	const key = "sk-ant-api03-E2E-AGENT-STEP-KEY-0123456789abcdefghijklmn"
	cmd := exec.Command(bin, "secret", "set", "anthropic-key", "--host", "api.anthropic.com") //nolint:gosec // test binary
	cmd.Env, cmd.Stdin = env, strings.NewReader(key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("secret set: %v\n%s", err, out)
	}
	inj := filepath.Join(home, "inject.yaml")
	writeFile(t, inj, `version: 1
name: injected
triggers: [{type: manual}]
steps:
  - id: review
    agent: {profile: injector, prompt: "review with an injected key"}
    outputs: {key: "json:key", env: "json:env"}
`)
	must("workflow", "apply", inj)
	out, err = run("run", "injected", "--wait", "--json")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	id := lastJSONID(out)
	got = must("runs", "get", id, "--json")
	if strings.Contains(got, key) || !strings.Contains(got, `"key": "icaro-injected."`) || !strings.Contains(got, `"env": "0"`) {
		t.Fatalf("inject run:\n%s", got)
	}
	logs := must("runs", "logs", id)
	if strings.Contains(logs, key) {
		t.Fatalf("the key reached the log:\n%s", logs)
	}
	db, err := os.ReadFile(filepath.Join(home, "config", "icaro.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte(key)) {
		t.Fatal("the key is in the database in the clear")
	}
}

// lastJSONID returns the "id" of the run in icaro run --json output.
func lastJSONID(out string) string {
	var v struct {
		ID string `json:"id"`
	}
	if i := strings.LastIndex(out, "\n{"); i >= 0 {
		out = out[i+1:]
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &v)
	return v.ID
}
