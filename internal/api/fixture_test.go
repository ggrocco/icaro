package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/crypto"
	"icaro/internal/integration"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/internal/paths"
	"icaro/internal/preview"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

// fakeContainers is the Docker edge for the session manager. Its state
// lives in one goroutine; calls are serialized through ops.
type fakeContainers struct {
	ops     chan func()
	running map[string]bool
	n       int
}

func newFakeContainers(ctx context.Context) *fakeContainers {
	f := &fakeContainers{ops: make(chan func()), running: map[string]bool{}}
	go func() {
		for {
			select {
			case op := <-f.ops:
				op()
			case <-ctx.Done():
				return
			}
		}
	}()
	return f
}

func (f *fakeContainers) do(fn func()) {
	done := make(chan struct{})
	f.ops <- func() { fn(); close(done) }
	<-done
}

func (f *fakeContainers) Ping(context.Context) error { return nil }

func (f *fakeContainers) CreateContainerOn(context.Context, docker.Spec, docker.NetSpec) (id string, err error) {
	f.do(func() { f.n++; id = fmt.Sprintf("c%d", f.n); f.running[id] = false })
	return id, nil
}

func (f *fakeContainers) StartContainer(_ context.Context, id string) error {
	f.do(func() { f.running[id] = true })
	return nil
}

func (f *fakeContainers) StopContainer(_ context.Context, id string, _ int) error {
	f.do(func() { f.running[id] = false })
	return nil
}

func (f *fakeContainers) RemoveContainer(_ context.Context, id string) error {
	f.do(func() { delete(f.running, id) })
	return nil
}

func (f *fakeContainers) Inspect(_ context.Context, id string) (st docker.State, err error) {
	f.do(func() { r, ok := f.running[id]; st = docker.State{Exists: ok, Running: r} })
	return st, nil
}

// fakeEgress and fakeImages fail when their err is set.
type fakeEgress struct{ err error }

func (f fakeEgress) Ensure(_ context.Context, profile string, _ []string) (container.Attachment, error) {
	return container.Attachment{Network: container.NetworkName(profile)}, f.err
}

type fakeImages struct{ err error }

func (f fakeImages) Resolve(_ context.Context, ref string) (string, error) {
	return ref + "@sha256:abc", f.err
}

// stubSchedules is the scheduler seam.
type stubSchedules struct {
	states []*store.CronState
	err    error
}

func (s stubSchedules) Next(context.Context) ([]*store.CronState, error) { return s.states, s.err }

// full is a daemon with every route group registered, reachable over a
// Unix socket (the trusted local principal) and over TCP (tokens only).
type full struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	svc   *service.Service
	bus   *events.Bus
	srv   *Server
	tcp   *httptest.Server
	local *http.Client
	repo  string
	cfg   *config.Config
	hub   *integration.Hub
}

func newFull(t *testing.T) *full {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// An explicit, empty config file and home keep the fixture hermetic
	// without t.Setenv, so tests using it can run in parallel.
	cfgFile := filepath.Join(t.TempDir(), "icaro.yaml")
	if err := os.WriteFile(cfgFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = t.TempDir()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.New(ctx)
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet, Events: bus})
	srv := New(svc, quiet)

	sec := secrets.New(st, key)
	homes := &creds.Homes{Layout: cfg.Layout(), Mode: creds.ModeCopy}
	mgr := session.New(session.Options{
		Store: st, Config: cfg, Engine: newFakeContainers(ctx), Egress: fakeEgress{}, Images: fakeImages{},
		Homes: homes, Log: quiet, MaxActive: 4, UID: 1234, GID: 1234, Tick: time.Hour,
	})
	go func() { _ = mgr.Run(ctx) }()
	srv.RegisterSessions(SessionDeps{Sessions: mgr, Secrets: sec, Egress: fakeEgress{}, Images: fakeImages{}, Homes: homes, Config: cfg})

	pm := preview.New(preview.Options{Layout: paths.New(t.TempDir()), HTTPPort: 18000, Debounce: time.Hour}, st, &stubCompose{running: map[string]int{}}, stubEngine{}, quiet)
	go func() { _ = pm.Run(ctx) }()
	RegisterPreviews(srv, pm)

	eng := memory.New(st, quiet, "test")
	host := mcp.NewServer("icaro", "test", "")
	memory.RegisterTools(host, eng)
	RegisterMemory(srv, eng, host)

	hub := integration.NewHub(integration.HubOptions{Dir: t.TempDir(), Store: st, Secrets: sec, Log: quiet})
	hub.SetEnqueuer(&recEngine{})
	hub.SetSpecSource(oneSpec{})
	srv.IntegrationRoutes(hub, sec, stubSchedules{}, "")

	srv.RegisterUI(UIOptions{Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}})

	tcp := httptest.NewServer(srv.Handler())
	t.Cleanup(tcp.Close)

	return &full{t: t, ctx: ctx, st: st, svc: svc, bus: bus, srv: srv, tcp: tcp, local: serveUnix(t, srv.Handler()), cfg: cfg, hub: hub}
}

// gitRepo returns a git checkout for session tests, created on first use
// (git is the slow part of the fixture).
func (f *full) gitRepo() string {
	f.t.Helper()
	if f.repo == "" {
		f.repo = filepath.Join(f.t.TempDir(), "repo")
		if err := os.MkdirAll(f.repo, 0o700); err != nil {
			f.t.Fatal(err)
		}
		gitInit(f.t, f.repo)
	}
	return f.repo
}

// serveUnix serves h on a Unix socket with ConnContext, like the daemon.
func serveUnix(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	// A short directory: macOS caps socket paths at 104 bytes.
	dir, err := os.MkdirTemp("", "ia")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h, ConnContext: ConnContext, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close(); _ = os.RemoveAll(dir) })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

// call sends a request over the local socket; body is JSON-encoded unless
// it is a string.
func (f *full) call(method, path string, body any) (int, string) {
	f.t.Helper()
	return doReq(f.t, f.local, method, "http://icaro"+path, body, nil)
}

func doReq(t *testing.T, c *http.Client, method, url string, body any, hdr http.Header) (int, string) {
	t.Helper()
	var rd io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewBufferString(b)
		ct = "text/yaml"
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// errBody decodes the error envelope.
func errBody(t *testing.T, body string) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not an error envelope: %q", body)
	}
	return e
}

var errBoom = errors.New("boom")

// blockedLayout is a state root whose directories cannot be created: the
// root is a regular file.
func blockedLayout(t *testing.T) paths.Layout {
	t.Helper()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return paths.New(file)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...) //nolint:gosec // test fixture
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}
