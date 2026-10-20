package engine

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

// loadDAG is a five-step diamond: s-a → (s-b, s-c) → s-d → s-e.
const loadDAG = `
version: 1
name: load-dag
steps:
  - id: s-a
    image: alpine:3.20
    command: echo a
  - id: s-b
    image: alpine:3.20
    needs: [s-a]
    command: echo b
  - id: s-c
    image: alpine:3.20
    needs: [s-a]
    command: echo c
  - id: s-d
    image: alpine:3.20
    needs: [s-b, s-c]
    command: echo d
  - id: s-e
    image: alpine:3.20
    needs: [s-d]
    command: echo e
`

// TestLoadManyRuns drives loadRuns DAG runs through the real engine with
// the fake Docker client (Phase 11 load test; -tags load runs the full
// size). It checks every run ends succeeded with every step run exactly
// once, and that goroutines return to their baseline afterwards.
func TestLoadManyRuns(t *testing.T) {
	f := newFixture(t)
	f.cfg.Concurrency = 8
	f.docker.OnStart = func(*docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Stdout: "ok\n", Duration: time.Millisecond}
	}
	runtime.GC()
	baseG := runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	runs := make([]*store.Run, loadRuns)
	for i := range runs {
		runs[i] = f.enqueue(loadDAG, map[string]any{"i": i})
	}
	start := time.Now()
	_, stop := f.start()
	deadline := time.Now().Add(loadTimeout)
	for _, r := range runs {
		for {
			cur, err := f.st.GetRun(f.ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cur.Status == store.RunSucceeded {
				break
			}
			if cur.Status == store.RunFailed || cur.Status == store.RunCancelled || time.Now().After(deadline) {
				t.Fatalf("run %s: %s %s", r.ID, cur.Status, f.dump(r.ID))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	elapsed := time.Since(start)
	stop()

	steps := len(runs) * 5
	if got := len(f.docker.Containers); got != steps {
		t.Fatalf("containers = %d, want %d (each step exactly once)", got, steps)
	}
	perStep := map[string]int{}
	for _, c := range f.docker.Containers {
		perStep[c.Spec.Labels[LabelRun]+"/"+c.Spec.Labels[LabelStep]]++
		if !c.Removed {
			t.Fatalf("container %s left behind", c.ID)
		}
	}
	for k, n := range perStep {
		if n != 1 {
			t.Fatalf("%s started %d times", k, n)
		}
	}

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	// Goroutines settle back to the baseline once the engine stops (the
	// fixture's event bus is still alive, so allow a small slack).
	var g int
	for i := 0; i < 100; i++ {
		if g = runtime.NumGoroutine(); g <= baseG+5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if g > baseG+5 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines %d -> %d after the engine stopped:\n%s", baseG, g, firstStacks(string(buf[:n]), 4))
	}
	heap := int64(after.HeapInuse) - int64(before.HeapInuse) //nolint:gosec // heap sizes are far below 2^63
	allocPerStep := (after.TotalAlloc - before.TotalAlloc) / uint64(steps)
	t.Logf("load: %d runs x 5 steps, concurrency %d: %s (%.0f steps/s); goroutines %d -> %d; retained heap delta %d KiB; %d KiB allocated per step",
		len(runs), f.cfg.Concurrency, elapsed.Round(time.Millisecond), float64(steps)/elapsed.Seconds(), baseG, g, heap/1024, allocPerStep/1024)
	if heap > loadHeapBudget {
		t.Fatalf("heap grew %d KiB, over the %d KiB budget", heap/1024, loadHeapBudget/1024)
	}
}

func firstStacks(all string, n int) string {
	parts := strings.Split(all, "\n\n")
	if len(parts) > n {
		parts = append(parts[:n], fmt.Sprintf("... %d more", len(parts)-n))
	}
	return strings.Join(parts, "\n\n")
}
