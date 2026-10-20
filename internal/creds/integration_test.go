//go:build integration

package creds

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"icaro/internal/container/docker"
	"icaro/internal/paths"
)

const testImage = "mirror.gcr.io/library/busybox:1.36"

// TestOverlayHomesConcurrent is spike S2's pass criterion: two concurrent
// sessions share the seeded login yet write to isolated layers with no
// corruption of each other's ~/.claude.
func TestOverlayHomesConcurrent(t *testing.T) {
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := eng.Ping(ctx); err != nil {
		t.Skipf("docker: %v", err)
	}
	if err := eng.EnsureImage(ctx, testImage); err != nil {
		t.Fatal(err)
	}
	h := &Homes{Layout: paths.New(t.TempDir()), Mode: ModeOverlay, Volumes: eng}
	seed, _ := h.EnsureSeed("claude")
	_ = os.MkdirAll(filepath.Join(seed, ".claude"), 0o777)
	_ = os.WriteFile(filepath.Join(seed, ".claude", ".credentials.json"), []byte("LOGIN"), 0o644)

	var wg sync.WaitGroup
	outs := make([]string, 2)
	errs := make([]error, 2)
	for i, id := range []string{"ITA", "ITB"} {
		m, err := h.Prepare(ctx, "claude", id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Release(ctx, "claude", id) })
		wg.Add(1)
		go func(i int, vol, id string) {
			defer wg.Done()
			script := "cat /h/.claude/.credentials.json; for n in 1 2 3 4 5 6 7 8 9 10; do echo " + id + "-$n >> /h/.claude/history; done; sleep 1; cat /h/.claude/history"
			var out bytes.Buffer
			cmd := exec.Command("docker", "run", "--rm", "-v", vol+":/h", testImage, "sh", "-c", script)
			cmd.Stdout, cmd.Stderr = &out, &out
			errs[i] = cmd.Run()
			outs[i] = out.String()
		}(i, m.Volume, id)
	}
	wg.Wait()
	for i, id := range []string{"ITA", "ITB"} {
		if errs[i] != nil {
			t.Fatalf("%s: %v %s", id, errs[i], outs[i])
		}
		if !strings.HasPrefix(outs[i], "LOGIN") {
			t.Fatalf("%s did not see the seeded login: %q", id, outs[i])
		}
		other := map[string]string{"ITA": "ITB", "ITB": "ITA"}[id]
		if strings.Count(outs[i], id+"-") != 10 || strings.Contains(outs[i], other) {
			t.Fatalf("%s home corrupted: %q", id, outs[i])
		}
	}
	if _, err := os.Stat(filepath.Join(seed, ".claude", "history")); !os.IsNotExist(err) {
		t.Fatal("sessions wrote into the golden seed")
	}
}
