package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/integration"
	"icaro/internal/paths"
	"icaro/internal/preview"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func TestPreviewInputErrors(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	dir := filepath.Join(t.TempDir(), "site")
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: x\n    ports: [\"80\"]\n    privileged: true\n"), 0o600)
	empty := t.TempDir()
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
		want         string
	}{
		{"POST", "/api/v1/previews/up", []byte(`{"path":`), 400, "invalid JSON body"},
		{"POST", "/api/v1/previews/up", map[string]any{}, 400, "path is required"},
		{"POST", "/api/v1/previews/down", map[string]any{}, 400, "path is required"},
		{"POST", "/api/v1/previews/remove", map[string]any{}, 400, "path is required"},
		{"POST", "/api/v1/previews/lint", map[string]any{}, 400, "path is required"},
		{"POST", "/api/v1/previews/down", map[string]any{"path": empty}, 404, ""},
		// Removing a path without a preview is idempotent.
		{"POST", "/api/v1/previews/remove", map[string]any{"path": empty}, 204, ""},
		{"POST", "/api/v1/previews/lint", map[string]any{"path": empty}, 400, "no compose file"},
		{"GET", "/api/v1/previews/missing", nil, 404, ""},
		{"GET", "/api/v1/previews/missing/logs", nil, 404, ""},
		{"POST", "/api/v1/previews/allow", []byte(`{"path":`), 400, "invalid JSON body"},
		{"POST", "/api/v1/previews/allow", map[string]any{"path": dir}, 400, "path and findings are required"},
		{"POST", "/api/v1/previews/allow", map[string]any{"path": dir, "findings": []string{"web/nope"}, "confirm": true}, 400, "web/nope"},
	} {
		code, body := f.call(tc.method, tc.path, tc.body)
		if code != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s %v = %d %s", tc.method, tc.path, tc.body, code, body)
		}
	}
}

// failingLogs is a compose edge whose logs cannot be read.
type failingLogs struct{ *stubCompose }

func (failingLogs) Logs(context.Context, string, int, io.Writer) error { return errBoom }

func TestPreviewLogsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	pm := preview.New(preview.Options{Layout: paths.New(t.TempDir()), HTTPPort: 18000, Debounce: time.Hour}, f.st,
		failingLogs{&stubCompose{running: map[string]int{}}}, stubEngine{}, quietLog())
	go func() { _ = pm.Run(f.ctx) }()
	s := New(f.svc, quietLog())
	RegisterPreviews(s, pm)
	c := serveUnix(t, s.Handler())

	dir := filepath.Join(t.TempDir(), "site")
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: x\n    ports: [\"80\"]\n"), 0o600)
	code, body := doReq(t, c, "POST", "http://icaro/api/v1/previews/up", map[string]any{"path": dir}, nil)
	var p PreviewInfo
	if code != 200 || !strings.Contains(body, `"id"`) {
		t.Fatalf("up: %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	// The handler already sent 200 text, so the failure is reported inline.
	code, body = doReq(t, c, "GET", "http://icaro/api/v1/previews/"+p.ID+"/logs?tail=5", nil, nil)
	if code != 200 || !strings.Contains(body, "(logs unavailable: boom)") {
		t.Fatalf("logs: %d %s", code, body)
	}
}

func TestMemoryInputErrors(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	big := make([]byte, 32<<20+1)
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
		want         string
	}{
		{"POST", "/api/v1/memory", []byte(`{"project":`), 400, "invalid JSON body"},
		{"POST", "/api/v1/memory/m1/accept", []byte(`{"project":`), 400, "invalid JSON body"},
		{"POST", "/api/v1/memory/m1/accept", map[string]any{"project": "bad id"}, 400, "project is required"},
		{"POST", "/api/v1/memory/ingest", []byte(`{"path":`), 400, "invalid JSON body"},
		{"POST", "/api/v1/memory/ingest", map[string]any{"path": filepath.Join(t.TempDir(), "missing")}, 400, ""},
		{"POST", "/api/v1/memory/ingest", map[string]any{"path": t.TempDir(), "project": "bad id"}, 400, "project is required"},
		{"POST", "/api/v1/memory/ingest", map[string]any{"path": filepath.Join(t.TempDir(), "missing"), "project": "pa"}, 400, ""},
		{"POST", "/api/v1/memory/import", big, 400, "import body exceeds 32 MiB"},
		{"POST", "/api/v1/memory/import", []byte(`{"project":"pa","extra":1}`), 400, "invalid JSON body"},
		{"POST", "/api/v1/memory/import", map[string]any{"format": "ai-memory", "data": "[]"}, 400, "project is required"},
	} {
		code, body := f.call(tc.method, tc.path, tc.body)
		if code != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %s", tc.method, tc.path, code, body)
		}
	}
}

func TestIntegrationRouteEdges(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	if code, body := f.call("GET", "/api/v1/schema/integration-manifest", nil); code != 200 || !strings.Contains(body, `"$schema"`) {
		t.Fatalf("schema: %d %.80s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/integrations/reload", nil); code != 200 || !strings.Contains(body, `"loaded_at"`) {
		t.Fatalf("reload: %d %s", code, body)
	}
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
		want         string
	}{
		{"GET", "/api/v1/integrations/nope", nil, 404, ""},
		{"PUT", "/api/v1/integrations/acme/manifest", " ", 400, "empty body"},
		{"POST", "/api/v1/packs/nope/push", nil, 400, ""},
		{"GET", "/api/v1/actions/nope.nothing/schema", nil, 404, ""},
		{"POST", "/api/v1/credentials", []byte(`{"name":`), 400, "invalid JSON body"},
		{"GET", "/api/v1/credentials/plan?integration=nope&name=token", nil, 404, ""},
		{"POST", "/api/v1/deliveries/nope/redeliver", nil, 404, ""},
		{"POST", "/api/v1/webhook-tokens/nope/rotate", nil, 404, ""},
		{"GET", "/api/v1/schedules", nil, 200, `"schedules": []`},
	} {
		code, body := f.call(tc.method, tc.path, tc.body)
		if code != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %s", tc.method, tc.path, code, body)
		}
	}
}

func TestManifestEditStorageFailure(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	key, _ := crypto.NewKey()
	sec := secrets.New(f.st, key)
	// The local pack cannot be created under a regular file.
	hub := integration.NewHub(integration.HubOptions{Dir: blockedLayout(t).Root, Store: f.st, Secrets: sec, Log: quietLog()})
	s := New(f.svc, quietLog())
	s.IntegrationRoutes(hub, sec, nil, "")
	c := serveUnix(t, s.Handler())
	code, body := doReq(t, c, "PUT", "http://icaro/api/v1/integrations/acme/manifest", "version: 1\nname: acme\n", nil)
	if code != 500 || errBody(t, body).Error != "internal error" {
		t.Fatalf("%d %s", code, body)
	}
	// Without a scheduler there is no schedules route.
	if code, _ := doReq(t, c, "GET", "http://icaro/api/v1/schedules", nil, nil); code != 404 {
		t.Fatalf("schedules: %d", code)
	}
}

// webhookSpec is a spec source with one webhook-triggered workflow.
type webhookSpec struct{}

func (webhookSpec) ActiveSpecs(context.Context) ([]*workflow.Spec, error) {
	return []*workflow.Spec{{Version: 1, Name: "deploy", Triggers: []workflow.TriggerSpec{{Integration: "github", Event: "push"}, {Integration: "webhook"}}}}, nil
}

func TestWebhookTokensAndSchedules(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	key, _ := crypto.NewKey()
	sec := secrets.New(f.st, key)
	hub := integration.NewHub(integration.HubOptions{Dir: t.TempDir(), Store: f.st, Secrets: sec, Log: quietLog()})
	hub.SetSpecSource(webhookSpec{})
	fired := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	states := []*store.CronState{
		{Workflow: "nightly", Idx: 0, Expr: "0 3 * * *", NextAt: fired.Add(24 * time.Hour), LastFiredAt: &fired},
		{Workflow: "hourly", Idx: 1, Expr: "0 * * * *", NextAt: fired.Add(time.Hour)},
	}
	s := New(f.svc, quietLog())
	s.IntegrationRoutes(hub, sec, stubSchedules{states: states}, "https://icaro.example.com/")
	c := serveUnix(t, s.Handler())

	code, body := doReq(t, c, "GET", "http://icaro/api/v1/webhook-tokens", nil, nil)
	if code != 200 || !strings.Contains(body, `"workflow": "deploy"`) || !strings.Contains(body, `"url": "https://icaro.example.com/hooks/webhook/`) {
		t.Fatalf("tokens: %d %s", code, body)
	}
	code, rotated := doReq(t, c, "POST", "http://icaro/api/v1/webhook-tokens/deploy/rotate", nil, nil)
	if code != 200 || rotated == body || !strings.Contains(rotated, `"path": "/hooks/webhook/`) {
		t.Fatalf("rotate: %d %s", code, rotated)
	}
	code, body = doReq(t, c, "GET", "http://icaro/api/v1/schedules", nil, nil)
	if code != 200 || !strings.Contains(body, `"last_fired_at": "2026-01-02T03:04:05Z"`) || strings.Count(body, "last_fired_at") != 1 ||
		!strings.Contains(body, `"cron": "0 * * * *"`) {
		t.Fatalf("schedules: %d %s", code, body)
	}

	s2 := New(f.svc, quietLog())
	s2.IntegrationRoutes(hub, sec, stubSchedules{err: errBoom}, "")
	c2 := serveUnix(t, s2.Handler())
	if code, body := doReq(t, c2, "GET", "http://icaro/api/v1/schedules", nil, nil); code != 500 || errBody(t, body).Error != "internal error" {
		t.Fatalf("schedules error: %d %s", code, body)
	}
}

func TestMapIntegrationErr(t *testing.T) {
	for _, err := range []error{integration.ErrNotFound, store.ErrNotFound, fmt.Errorf("x: %w", secrets.ErrNotFound)} {
		if !errors.Is(mapIntegrationErr(err), service.ErrNotFound) {
			t.Fatalf("%v", err)
		}
	}
	if err := mapIntegrationErr(context.Canceled); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	var br *service.BadRequest
	if !errors.As(mapIntegrationErr(errBoom), &br) || br.Msg != "boom" {
		t.Fatal("bad request")
	}
	if got := orEmptySlice[int](nil); got == nil || len(got) != 0 {
		t.Fatal("orEmptySlice(nil)")
	}
}

func TestUIRouteEdges(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	// A link minted over the socket is a one-time code; Open is optional.
	code, body := f.call("POST", "/api/v1/ui/link", nil)
	if code != 200 || !strings.Contains(body, `"url": "/ui/?token=`) || !strings.Contains(body, `"expires_in_seconds"`) {
		t.Fatalf("link: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/ui/login", []byte(`{"token":`)); code != 403 {
		t.Fatalf("login without CSRF: %d %s", code, body)
	}
	csrf := http.Header{CSRFHeader: {"1"}}
	if code, body := doReq(t, f.local, "POST", "http://icaro/api/v1/ui/login", []byte(`{"token":`), csrf); code != 400 || !strings.Contains(body, "invalid JSON body") {
		t.Fatalf("login bad JSON: %d %s", code, body)
	}
	if code, _ := f.call("POST", "/api/v1/ui/logout", nil); code != 403 {
		t.Fatalf("logout without CSRF: %d", code)
	}
	// Without a cookie, logout still expires it.
	if code, _ := doReq(t, f.local, "POST", "http://icaro/api/v1/ui/logout", nil, csrf); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}

	for _, tc := range []struct {
		name string
		open func() (string, error)
		code int
		want string
	}{
		{"open fails", func() (string, error) { return "", errBoom }, 500, "internal error"},
		{"open", func() (string, error) { return "http://127.0.0.1:8787/", nil }, 200, `"url": "http://127.0.0.1:8787/ui/?token=`},
	} {
		s := New(f.svc, quietLog())
		s.RegisterUI(UIOptions{Assets: fstest.MapFS{}, Open: tc.open})
		c := serveUnix(t, s.Handler())
		if code, body := doReq(t, c, "POST", "http://icaro/api/v1/ui/link", nil, nil); code != tc.code || !strings.Contains(body, tc.want) {
			t.Fatalf("%s: %d %s", tc.name, code, body)
		}
	}

	// A build without index.html serves its README with a 503.
	s := New(f.svc, quietLog())
	s.RegisterUI(UIOptions{Assets: fstest.MapFS{"README.txt": {Data: []byte("run make web")}}})
	c := serveUnix(t, s.Handler())
	if code, body := doReq(t, c, "GET", "http://icaro/ui/", nil, nil); code != http.StatusServiceUnavailable || body != "run make web" {
		t.Fatalf("no index: %d %q", code, body)
	}
	// Without options the embedded build is served.
	s = New(f.svc, quietLog())
	s.RegisterUI(UIOptions{})
	c = serveUnix(t, s.Handler())
	if code, _ := doReq(t, c, "GET", "http://icaro/ui/", nil, nil); code != 200 && code != http.StatusServiceUnavailable {
		t.Fatalf("embedded: %d", code)
	}
}

func TestProfilesAndSecretsEdges(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	f.cfg.Profiles["bare"] = config.ProfileConfig{Image: "alpine"}
	code, body := f.call("GET", "/api/v1/profiles", nil)
	if code != 200 || !strings.Contains(body, `"name": "bare"`) || strings.Contains(body, `"egress": null`) {
		t.Fatalf("profiles: %d %s", code, body)
	}
	if code, _ := f.call("PUT", "/api/v1/secrets/gh", []byte(`{"value":`)); code != 400 {
		t.Fatalf("bad JSON: %d", code)
	}
	if code, _ := f.call("DELETE", "/api/v1/secrets/nope", nil); code != 404 {
		t.Fatalf("delete missing: %d", code)
	}
}
