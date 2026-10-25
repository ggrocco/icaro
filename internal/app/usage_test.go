package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/config"
)

// writeTranscript puts one priced assistant message from now into the
// claude profile home.
func writeTranscript(t *testing.T, a *App) {
	t.Helper()
	dir := filepath.Join(a.Layout.Homes(), "claude", ".claude", "projects", "-workspace")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": time.Now().UTC().Format(time.RFC3339), "requestId": "req_1",
		"message": map[string]any{"id": "msg_1", "model": "claude-sonnet-4-20250514", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}},
	})
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func usageComponent(t *testing.T, a *App) *usageSnapshots {
	t.Helper()
	for _, c := range a.components {
		if u, ok := c.(*usageSnapshots); ok {
			return u
		}
	}
	t.Fatal("usage-snapshots is not wired")
	return nil
}

// TestUsageGetDoesNotWrite: the read-scope usage route only reads; the
// periodic job is what stores snapshots.
func TestUsageGetDoesNotWrite(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), nil)
	writeTranscript(t, a)
	ctx := context.Background()

	req := httptest.NewRequestWithContext(connCtx(t, "unix"), http.MethodGet, "/api/v1/usage", nil)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rows, err := a.Store.ListUsageSnapshots(ctx, ""); err != nil || len(rows) != 0 {
		t.Fatalf("GET wrote snapshots: %+v %v", rows, err)
	}

	u := usageComponent(t, a)
	if u.Name() != "usage-snapshots" {
		t.Fatal(u.Name())
	}
	old := usageSnapshotEvery
	usageSnapshotEvery = 10 * time.Millisecond
	t.Cleanup(func() { usageSnapshotEvery = old })
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- u.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := a.Store.ListUsageSnapshots(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Source == "profile:claude" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot saved: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A bad usage.timezone is logged by the job, never fatal to the daemon.
func TestUsageSnapshotsBadTimezone(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), func(c *config.Config) { c.Usage.Timezone = "Nowhere/Nope" })
	usageComponent(t, a).save(context.Background())
	if rows, err := a.Store.ListUsageSnapshots(context.Background(), ""); err != nil || len(rows) != 0 {
		t.Fatalf("%+v %v", rows, err)
	}
}
