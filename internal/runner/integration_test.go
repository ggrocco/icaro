//go:build integration

package runner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"icaro/internal/runner/docker"
	"icaro/internal/store"
)

// These tests need a reachable Docker daemon: `make test-integration`.
func newDockerFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Ping(f.ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	f.runner.docker = eng
	f.runner.cfg.StepTimeout = 2 * time.Minute
	return f
}

func TestIntegrationHelloWorkflow(t *testing.T) {
	f := newDockerFixture(t)
	yaml := `
name: it-hello
steps:
  - name: greet
    run:
      image: alpine:3.20
      script: |
        echo "hello {{ inputs.who }}"
        echo "input: $(cat /icaro/input.json)"
        test "$ICARO_INPUT_WHO" = "{{ inputs.who }}"
        touch /workspace/from-greet
        (touch /rootfs-must-be-ro 2>/dev/null) && exit 9
        (unshare -r true 2>/dev/null) && exit 8
        printf '{"greeting":"hello {{ inputs.who }}"}' > /icaro/output.json
  - name: shout
    run:
      image: alpine:3.20
      command: ["sh", "-c", "test -f /workspace/from-greet && echo '{{ steps.greet.outputs.greeting }}' | tr a-z A-Z"]
    network: none
`
	run := f.enqueue(yaml, map[string]any{"who": "docker"})
	got := f.claimAndRun()
	if got.Status != store.RunSucceeded {
		steps := f.steps(run.ID)
		for _, st := range steps {
			b, _ := os.ReadFile(st.LogPath)
			t.Logf("step %s: %+v\n%s", st.Name, st, b)
		}
		t.Fatalf("run: %+v", got)
	}
	steps := f.steps(run.ID)
	if steps[0].OutputJSON != `{"greeting":"hello docker"}` {
		t.Fatalf("outputs: %s", steps[0].OutputJSON)
	}
	log0, _ := os.ReadFile(steps[0].LogPath)
	if !strings.Contains(string(log0), "hello docker") || !strings.Contains(string(log0), `input: {"who":"docker"}`) {
		t.Fatalf("log0: %s", log0)
	}
	log1, _ := os.ReadFile(steps[1].LogPath)
	if !strings.Contains(string(log1), "HELLO DOCKER") {
		t.Fatalf("log1: %s", log1)
	}
}

func TestIntegrationFailureAndTimeout(t *testing.T) {
	f := newDockerFixture(t)
	yaml := `
name: it-fail
steps:
  - name: fails
    run: { image: alpine:3.20, script: "echo boom >&2; exit 3" }
    continue_on_error: true
  - name: slow
    run: { image: alpine:3.20, script: "sleep 30" }
    timeout: 2s
`
	run := f.enqueue(yaml, nil)
	start := time.Now()
	got := f.claimAndRun()
	if got.Status != store.RunFailed || !strings.Contains(got.Error, "timed out") {
		t.Fatalf("run: %+v", got)
	}
	if time.Since(start) > 25*time.Second {
		t.Fatal("timeout did not kill the container promptly")
	}
	steps := f.steps(run.ID)
	if steps[0].Status != store.StepFailed || *steps[0].ExitCode != 3 {
		t.Fatalf("step0: %+v", steps[0])
	}
	log0, _ := os.ReadFile(steps[0].LogPath)
	if !strings.Contains(string(log0), "boom") {
		t.Fatalf("stderr captured: %s", log0)
	}
	if steps[1].Status != store.StepFailed {
		t.Fatalf("step1: %+v", steps[1])
	}
	// The killed container must be gone.
	ids, _ := f.runner.docker.ListByLabel(context.Background(), map[string]string{LabelRun: run.ID})
	if len(ids) != 0 {
		t.Fatalf("containers left behind: %v", ids)
	}
}
