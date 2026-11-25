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
const itImage = "mirror.gcr.io/library/alpine:3.24"

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

// Call steps against real Docker (ADR 0038): lineage through a three-deep
// chain on one slot, a daemon restart while the leaf runs, and a cancel
// of the root that stops the whole tree.
func TestIntegrationCallTree(t *testing.T) {
	f, _ := newDockerFixture(t)
	f.cfg.MaxRuns = 1
	f.calls()
	leaf := `
version: 1
name: leaf
triggers: [{type: call}]
outputs:
  word: ${{ steps.say.outputs.word }}
steps:
  - id: say
    image: ` + itImage + `
    command: sleep ${{ trigger.secs }} && printf '{"word":"hi-%s"}' "${{ trigger.secs }}" > /icaro/output.json
    outputs: {word: "json:word"}
`
	mid := `
version: 1
name: mid
triggers: [{type: call}]
outputs:
  word: ${{ steps.inner.outputs.word }}
steps:
  - id: inner
    call: {workflow: leaf, input: {secs: "${{ trigger.secs }}"}}
`
	top := `
version: 1
name: top
steps:
  - id: outer
    call: {workflow: mid, input: {secs: "${{ trigger.secs }}"}}
  - id: echo
    needs: [outer]
    image: ` + itImage + `
    command: echo ${{ steps.outer.outputs.word }}
    outputs: {line: "stdout:last-line"}
`
	f.apply(leaf)
	f.apply(mid)

	// Lineage, and a restart while the leaf's container runs.
	run := f.enqueue(top, map[string]any{"secs": "3"})
	_, stop := f.start()
	f.waitChild(run.ID, "outer", store.RunWaiting)
	stop()
	_, stop = f.start()
	got := f.waitLong(run.ID, store.RunSucceeded, store.RunFailed)
	stop()
	if got.Status != store.RunSucceeded || f.outputs(run.ID, "echo")["line"] != "hi-3" {
		t.Fatalf("%s %v %s", got.Status, f.outputs(run.ID, "echo"), f.dump(run.ID))
	}
	tree := f.tree(run.ID)
	if len(tree) != 3 || tree[1].Depth != 1 || tree[2].Depth != 2 || tree[2].ParentRunID != tree[1].ID || tree[2].RootRunID != run.ID {
		t.Fatalf("tree %+v", tree)
	}

	// Cancelling the root stops the tree, the leaf's container included.
	run = f.enqueue(top, map[string]any{"secs": "120"})
	e, stop := f.start()
	defer stop()
	midID := f.waitChild(run.ID, "outer", store.RunWaiting)
	leafID := f.waitChild(midID, "inner", store.RunRunning)
	f.waitStep(leafID, "say", func(r *store.StepRun) bool { return r.Status == store.StepRunning && r.ContainerID != "" })
	if _, err := f.st.RequestCancel(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	e.Wake()
	root := f.waitLong(run.ID, store.RunCancelled)
	for _, id := range []string{midID, leafID} {
		r := f.waitLong(id, store.RunCancelled)
		if r.FinishedAt.After(*root.FinishedAt) {
			t.Fatalf("%s finished after the root", id)
		}
	}
}

// With real containers: a step pushes a sha, the await parks the run with
// no container left, the daemon restarts, the event for that sha is
// matched (as pipeline step 6b does), and the next container reads its
// payload.
func TestIntegrationAwaitAcrossRestart(t *testing.T) {
	f, _ := newDockerFixture(t)
	yaml := `
version: 1
name: it-await
steps:
  - id: push
    image: ` + itImage + `
    command: echo '{"sha":"c0ffee"}' > /icaro/output.json
    outputs: { sha: json:sha }
  - id: ci
    needs: [push]
    await: {integration: github, event: check_suite.completed, key: "${{ steps.push.outputs.sha }}", match: event.head_sha}
    timeout: 10m
  - id: after
    image: ` + itImage + `
    needs: [ci]
    command: test '${{ steps.ci.outputs.conclusion }}' = success
`
	run := f.enqueue(yaml, nil)
	_, stop := f.start()
	f.waitLong(run.ID, store.RunWaiting)
	stop()
	_, stop2 := f.start()
	defer stop2()
	if _, err := f.st.ResolveAwait(f.ctx, run.ID, "ci", store.AwaitMatched, "d1", `{"head_sha":"c0ffee","conclusion":"success"}`, "integration:github"); err != nil {
		t.Fatal(err)
	}
	if got := f.waitLong(run.ID, store.RunSucceeded, store.RunFailed); got.Status != store.RunSucceeded {
		t.Fatalf("%s %s", got.Error, f.dump(run.ID))
	}
}
