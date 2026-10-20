//go:build integration

package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

// These tests need a reachable Docker daemon: `make test-integration`.
// Images come from mirror.gcr.io to avoid Docker Hub rate limits.
const itImage = "mirror.gcr.io/library/alpine:3.20"

func newDockerFixture(t *testing.T) (*fixture, *docker.Engine) {
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
	f.deps.Docker = eng
	f.cfg.StepTimeout = 2 * time.Minute
	// As in the app (Sessions.User): steps run as the daemon's user, or as
	// a non-root user when the daemon is root — never root, which has no
	// CAP_DAC_OVERRIDE to write the 0700 workspace.
	f.cfg.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if os.Getuid() == 0 {
		f.cfg.User = ""
	}
	return f, eng
}

func (f *fixture) waitLong(runID string, statuses ...string) *store.Run {
	f.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := f.st.GetRun(f.ctx, runID)
		for _, s := range statuses {
			if r.Status == s {
				return r
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatalf("run %s did not reach %v: %s", runID, statuses, f.dump(runID))
	return nil
}

func TestIntegrationDataFlowParallelAndSandbox(t *testing.T) {
	f, _ := newDockerFixture(t)
	yaml := `
version: 1
name: it-dag
steps:
  - id: greet
    image: ` + itImage + `
    input: { who: "${{ trigger.who }}" }
    command: |
      test "$ICARO_INPUT_WHO" = docker
      grep -q docker /icaro/input.json
      touch /workspace/from-greet
      (touch /rootfs-must-be-ro 2>/dev/null) && exit 9
      (unshare -r true 2>/dev/null) && exit 8
      (wget -q -T 2 -O /dev/null http://example.com 2>/dev/null) && exit 7
      printf '{"greeting":"hello %s"}' "$ICARO_INPUT_WHO" > /icaro/output.json
    outputs: { greeting: json:greeting }
  - id: left
    image: ` + itImage + `
    needs: [greet]
    command: sleep 2; date +%s
    outputs: { t: stdout:last-line }
  - id: right
    image: ` + itImage + `
    needs: [greet]
    command: sleep 2; date +%s
    outputs: { t: stdout:last-line }
  - id: shout
    image: ` + itImage + `
    needs: [left, right]
    command: |
      test -f /workspace/from-greet
      echo '${{ steps.greet.outputs.greeting }}' | tr a-z A-Z > /workspace/out.txt
    outputs: { loud: file:out.txt }
`
	run := f.enqueue(yaml, map[string]any{"who": "docker"})
	_, stop := f.start()
	defer stop()
	got := f.waitLong(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		for _, id := range []string{"greet", "left", "right", "shout"} {
			b, _ := os.ReadFile(f.step(run.ID, id).LogPath)
			t.Logf("%s log:\n%s", id, b)
		}
		t.Fatalf("run failed: %s %s", got.Error, f.dump(run.ID))
	}
	if v := f.outputs(run.ID, "shout")["loud"]; v != "HELLO DOCKER" {
		t.Fatalf("data flow: %v", v)
	}
	l, r := f.outputs(run.ID, "left")["t"].(string), f.outputs(run.ID, "right")["t"].(string)
	if l == "" || r == "" {
		t.Fatal("missing timestamps")
	}
	ls, rs := f.step(run.ID, "left"), f.step(run.ID, "right")
	if ls.StartedAt.Sub(*rs.StartedAt).Abs() > 1500*time.Millisecond || rs.FinishedAt.Before(*ls.StartedAt) || ls.FinishedAt.Before(*rs.StartedAt) {
		t.Fatalf("independent steps did not overlap: left %v-%v right %v-%v", ls.StartedAt, ls.FinishedAt, rs.StartedAt, rs.FinishedAt)
	}
}

func TestIntegrationCancelKillsContainer(t *testing.T) {
	f, eng := newDockerFixture(t)
	run := f.enqueue("version: 1\nname: it-cancel\nsteps:\n  - { id: long, image: "+itImage+", command: sleep 300 }\n", nil)
	e, stop := f.start()
	defer stop()
	var cid string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.step(run.ID, "long"); r.Status == store.StepRunning && r.ContainerID != "" {
			cid = r.ContainerID
			if st, _ := eng.Inspect(f.ctx, cid); st.Running {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cid == "" {
		t.Fatal("step never started")
	}
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	e.Cancel(run.ID)
	f.waitLong(run.ID, store.RunCancelled)
	if st, _ := eng.Inspect(f.ctx, cid); st.Exists {
		t.Fatalf("container %s still exists after cancel: %+v", cid, st)
	}
}

// The daemon restarts while a step runs: the container survives and the
// next engine re-attaches to it, finishing the run without a second
// container.
func TestIntegrationRestartReattach(t *testing.T) {
	f, eng := newDockerFixture(t)
	yaml := `
version: 1
name: it-reattach
steps:
  - id: long
    image: ` + itImage + `
    command: |
      echo before
      sleep 3
      echo after
      echo '{"v":"survived"}' > /icaro/output.json
    outputs: { v: json:v }
  - id: after
    image: ` + itImage + `
    needs: [long]
    command: test '${{ steps.long.outputs.v }}' = survived
`
	run := f.enqueue(yaml, nil)
	_, stop := f.start()
	var cid string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.step(run.ID, "long"); r.ContainerID != "" {
			if st, _ := eng.Inspect(f.ctx, r.ContainerID); st.Running {
				cid = r.ContainerID
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cid == "" {
		t.Fatal("step never started")
	}
	time.Sleep(500 * time.Millisecond)
	stop()
	if st, _ := eng.Inspect(f.ctx, cid); !st.Running {
		t.Fatal("daemon shutdown must not stop the step container")
	}
	if r, _ := f.st.GetRun(f.ctx, run.ID); r.Status != store.RunRunning {
		t.Fatalf("run status after shutdown: %s", r.Status)
	}

	_, stop2 := f.start()
	defer stop2()
	got := f.waitLong(run.ID, store.RunSucceeded, store.RunFailed)
	if got.Status != store.RunSucceeded {
		t.Fatalf("after re-attach: %s %s", got.Error, f.dump(run.ID))
	}
	long := f.step(run.ID, "long")
	if long.ContainerID != cid || long.Attempt != 1 {
		t.Fatalf("re-attach must reuse the container: %+v (was %s)", long, cid)
	}
	b, _ := os.ReadFile(long.LogPath)
	if strings.Count(string(b), "before") != 1 || !strings.Contains(string(b), "after") {
		t.Fatalf("log after re-attach: %q", b)
	}
}
