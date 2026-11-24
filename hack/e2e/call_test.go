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

// TestCallSteps is ADR 0038 against a real daemon and Docker: a caller
// runs a callee as a child run and reads its workflow output, the CLI
// shows both runs linked and lists the tree, and a cycle is refused.
func TestCallSteps(t *testing.T) {
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
	home, err := os.MkdirTemp("/tmp", "icecall-")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "config", "icaro.yaml"), "version: 1\nworkflows:\n  max_runs: 1\npreviews:\n  enabled: false\n")
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
		_ = command("docker", "network", "rm", "icaro-egress-"+container.Namespace(home)+"-workflow").Run()
		_ = os.RemoveAll(home)
	})
	must("init")
	must("serve", "--background")

	callee := filepath.Join(home, "callee.yaml")
	writeFile(t, callee, `version: 1
name: greet
triggers: [{type: call}]
outputs:
  line: ${{ steps.say.outputs.line }}
steps:
  - id: say
    image: mirror.gcr.io/library/busybox:1.36
    command: echo "hello ${{ trigger.who }}"
    outputs: {line: "stdout:last-line"}
`)
	caller := filepath.Join(home, "caller.yaml")
	writeFile(t, caller, `version: 1
name: welcome
triggers: [{type: manual}]
steps:
  - id: greet
    call: {workflow: greet, input: {who: "${{ trigger.who }}"}}
  - id: shout
    needs: [greet]
    image: mirror.gcr.io/library/busybox:1.36
    command: echo "${{ steps.greet.outputs.line }}!"
    outputs: {line: "stdout:last-line"}
`)
	must("workflow", "apply", callee)
	must("workflow", "apply", caller)
	if out := must("workflow", "get", "greet"); !strings.Contains(out, "# called by: welcome") {
		t.Fatalf("callers:\n%s", out)
	}
	out, err := run("run", "welcome", "--set", "who=ana", "--wait", "--json")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	id := lastJSONID(out)
	var r struct {
		Status string `json:"status"`
		Steps  []struct {
			ID      string         `json:"id"`
			Outputs map[string]any `json:"outputs"`
			Call    *struct {
				Workflow string `json:"workflow"`
				RunID    string `json:"run_id"`
				Status   string `json:"status"`
				Version  int    `json:"version"`
			} `json:"call"`
		} `json:"steps"`
	}
	got := must("runs", "get", id, "--json")
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatal(err, got)
	}
	if r.Status != "succeeded" || r.Steps[1].Outputs["line"] != "hello ana!" {
		t.Fatalf("run:\n%s", got)
	}
	c := r.Steps[0].Call
	if c == nil || c.Workflow != "greet" || c.Status != "succeeded" || c.Version != 1 {
		t.Fatalf("call:\n%s", got)
	}
	if out := must("runs", "get", id); !strings.Contains(out, "call step greet: workflow greet v1, child run "+c.RunID) {
		t.Fatalf("runs get:\n%s", out)
	}
	if out := must("runs", "get", c.RunID); !strings.Contains(out, "called by run "+id) || !strings.Contains(out, `outputs: {"line":"hello ana"}`) {
		t.Fatalf("child:\n%s", out)
	}
	if out := must("runs", "list", "--root", id); !strings.Contains(out, "  greet") || !strings.Contains(out, "welcome") {
		t.Fatalf("tree:\n%s", out)
	}

	// A cycle is refused with its path.
	cyc := filepath.Join(home, "cycle.yaml")
	writeFile(t, cyc, `version: 1
name: greet
triggers: [{type: call}]
steps:
  - id: back
    call: {workflow: welcome, input: {who: x}}
`)
	if out, err := run("workflow", "apply", cyc); err == nil || !strings.Contains(out, "welcome has no call trigger") {
		t.Fatalf("apply a call to an uncallable workflow: %v\n%s", err, out)
	}
	writeFile(t, caller, strings.Replace(mustRead(t, caller), "triggers: [{type: manual}]", "triggers: [{type: manual}, {type: call}]", 1))
	must("workflow", "apply", caller)
	if out, err := run("workflow", "apply", cyc); err == nil || !strings.Contains(out, "call cycle: greet → welcome → greet") {
		t.Fatalf("cycle: %v\n%s", err, out)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
