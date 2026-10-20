package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/container/docker"
	"icaro/internal/crypto"
	"icaro/internal/paths"
	"icaro/internal/preview"
	"icaro/internal/service"
	"icaro/internal/store"
)

// stubCompose and stubEngine stand in for Docker at the edge.
type stubCompose struct{ running map[string]int }

func (c *stubCompose) Config(_ context.Context, _ string, files []string) ([]byte, error) {
	return os.ReadFile(files[0])
}
func (c *stubCompose) Up(_ context.Context, p, _ string, _ []string) error {
	c.running[p] = 1
	return nil
}
func (c *stubCompose) Down(_ context.Context, p string) error           { c.running[p] = 0; return nil }
func (c *stubCompose) Running(_ context.Context, p string) (int, error) { return c.running[p], nil }
func (c *stubCompose) Logs(_ context.Context, p string, _ int, w io.Writer) error {
	_, err := io.WriteString(w, "log line from "+p)
	return err
}

type stubEngine struct{}

func (stubEngine) EnsureImage(context.Context, string) error { return nil }
func (stubEngine) EnsureNetwork(context.Context, string, map[string]string) (bool, error) {
	return false, nil
}
func (stubEngine) NetworkExists(context.Context, string) (bool, error) { return true, nil }
func (stubEngine) CreateProxy(context.Context, docker.ProxySpec) (string, error) {
	return "p", nil
}
func (stubEngine) StartContainer(context.Context, string) error { return nil }
func (stubEngine) Inspect(context.Context, string) (docker.State, error) {
	return docker.State{}, nil
}
func (stubEngine) RemoveContainer(context.Context, string) error { return nil }
func (stubEngine) Restart(context.Context, string) error         { return nil }

func TestPreviewEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := paths.New(t.TempDir())
	m := preview.New(preview.Options{Layout: l, HTTPPort: 18000, Debounce: time.Hour}, st, &stubCompose{running: map[string]int{}}, stubEngine{}, quiet)
	go func() { _ = m.Run(ctx) }()
	key, _ := crypto.NewKey()
	srv := New(service.New(service.Options{Store: st, Key: key, Log: quiet}), quiet)
	RegisterPreviews(srv, m)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	mk := func(name, scope string) *apiclient.Client {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(hs.URL, plain)
	}
	admin, reader := mk("admin", store.ScopeAdmin), mk("reader", store.ScopeRead)

	dir := filepath.Join(t.TempDir(), "site")
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: x\n    ports: [\"80\"]\n    privileged: true\n"), 0o600)

	if _, err := reader.PreviewUp(ctx, dir, false); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("reader up: %v", err)
	}
	p, err := admin.PreviewUp(ctx, dir, false)
	if err != nil || p.Status != store.PreviewBlocked || !strings.Contains(p.Curl, "Host: site.localhost") {
		t.Fatalf("up: %+v %v", p, err)
	}
	res, err := reader.PreviewLint(ctx, dir)
	if err != nil || len(res.Blocked) != 1 || res.Blocked[0].ID != "web/privileged" {
		t.Fatalf("lint: %+v %v", res, err)
	}
	if _, err := admin.PreviewAllow(ctx, dir, []string{"web/privileged"}); err != nil {
		t.Fatal(err)
	}
	if p, err = admin.PreviewUp(ctx, dir, true); err != nil || p.Status != store.PreviewRunning {
		t.Fatalf("after allow: %+v %v", p, err)
	}
	list, err := reader.ListPreviews(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	got, err := reader.GetPreview(ctx, p.ID)
	if err != nil || got.WorktreePath != p.WorktreePath {
		t.Fatalf("get: %+v %v", got, err)
	}
	logs, err := reader.PreviewLogs(ctx, p.ID, 5)
	if err != nil || !strings.Contains(string(logs), p.ComposeProject) {
		t.Fatalf("logs: %s %v", logs, err)
	}
	if d, err := admin.PreviewDown(ctx, dir); err != nil || d.Status != store.PreviewStopped {
		t.Fatalf("down: %+v %v", d, err)
	}
	if err := admin.PreviewRemove(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetPreview(ctx, p.ID); !apiclient.IsNotFound(err) {
		t.Fatalf("after remove: %v", err)
	}
	if _, err := admin.PreviewUp(ctx, t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("no compose file: %v", err)
	}
	if err := admin.Do(ctx, "POST", "/api/v1/previews/allow", map[string]any{"path": dir, "findings": []string{"x"}}, nil); err == nil {
		t.Fatal("allow without confirm accepted")
	}
}
