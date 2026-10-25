package app

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/doctor"
	"icaro/internal/session"
	"icaro/internal/store"
)

// fakeDocker serves the slice of the Docker Engine API the app's doctor
// checks use, on a Unix socket. It is the external-HTTP edge, so faking
// it here is allowed (05-engineering "Surgical mocks").
type fakeDocker struct {
	images     map[string]string // ref or digest -> repo digest
	pulls      atomic.Int32
	removed    atomic.Int32
	pingFail   atomic.Bool
	removeFail atomic.Bool
}

var apiVersion = regexp.MustCompile(`^/v[0-9.]+`)

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := apiVersion.ReplaceAllString(r.URL.Path, "")
	w.Header().Set("Api-Version", "1.51")
	notFound := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"not found"}`)
	}
	switch {
	case p == "/_ping":
		if f.pingFail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "OK")
	case strings.HasPrefix(p, "/images/create") && r.Method == http.MethodPost:
		f.pulls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"pulled"}`+"\n")
	case strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		d, ok := f.images[ref]
		if !ok {
			notFound()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"sha256:1","RepoDigests":["`+d+`"]}`)
	case strings.HasPrefix(p, "/containers/") && r.Method == http.MethodDelete:
		if f.removeFail.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"engine busy"}`)
			return
		}
		f.removed.Add(1)
		w.WriteHeader(http.StatusNoContent)
	default:
		notFound()
	}
}

// shortDir is a temp dir with a short path: Unix socket paths are capped
// at 104 bytes on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ica")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// startFakeDocker serves f on a Unix socket and returns a DOCKER_HOST.
func startFakeDocker(t *testing.T, f *fakeDocker) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sock
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testConfig loads the default config for a fresh home. The master key
// comes from ICARO_MASTER_KEY so no test touches the OS key store.
func testConfig(t *testing.T, dockerHost string) *config.Config {
	t.Helper()
	home := shortDir(t)
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	t.Setenv(crypto.MasterKeyEnv, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = home
	cfg.Docker.Host = dockerHost
	cfg.Daemon.UIListen = "127.0.0.1:0"
	return cfg
}

func newTestApp(t *testing.T, dockerHost string, mut func(*config.Config)) *App {
	t.Helper()
	cfg := testConfig(t, dockerHost)
	if mut != nil {
		mut(cfg)
	}
	a, err := New(context.Background(), cfg, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func checkNames(cs []doctor.Check) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func findCheck(t *testing.T, cs []doctor.Check, name string) doctor.Check {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %v", name, checkNames(cs))
	return doctor.Check{}
}

func TestNewWiresEveryArea(t *testing.T) {
	host := startFakeDocker(t, &fakeDocker{})
	a := newTestApp(t, host, nil)

	if a.Store == nil || a.Key == nil || a.Docker == nil || a.Secrets == nil || a.Sessions == nil ||
		a.Egress == nil || a.Images == nil || a.Homes == nil || a.Service == nil || a.API == nil ||
		a.Previews == nil || a.Memory == nil || a.MCP == nil || a.Workflows.Engine == nil || a.Workflows.Events == nil {
		t.Fatalf("an area is not wired: %+v", a)
	}
	if a.KeyInfo.Backend == "" {
		t.Error("key backend not recorded")
	}
	if IntegrationHub(a) == nil || Scheduler(a) == nil {
		t.Error("integrations area not registered")
	}
	// The glue wirer hands the session spine and the hub to the engine.
	if a.Workflows.Sessions == nil || a.Workflows.Egress == nil || a.Workflows.Actions == nil || a.Workflows.Catalog == nil {
		t.Errorf("workflow collaborators missing: %+v", a.Workflows)
	}
	if a.Handler() == nil {
		t.Error("no handler")
	}
	names := strings.Join(checkNames(a.Checks()), ",")
	for _, want := range []string{"docker", "database migrations", "master key", "orphaned sessions", "pinned images", "preview proxy network"} {
		if !strings.Contains(names, want) {
			t.Errorf("check %q missing from %s", want, names)
		}
	}
	// The workflow step user is the session spine's user.
	if got := stepUser(a); got != a.Sessions.User() {
		t.Errorf("stepUser = %q, want the session user %q", got, a.Sessions.User())
	}
}

func TestNewWithoutPreviews(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), func(c *config.Config) { c.Previews.Enabled = false })
	if a.Previews != nil {
		t.Error("previews wired although disabled")
	}
	if PreviewChecks(context.Background(), a.Cfg, a.Store, a.Docker) != nil {
		t.Error("preview checks registered although disabled")
	}
}

func TestNewErrors(t *testing.T) {
	host := startFakeDocker(t, &fakeDocker{})
	cases := []struct {
		name string
		mut  func(t *testing.T, c *config.Config)
		want string
	}{
		{"home is a file", func(t *testing.T, c *config.Config) {
			f := filepath.Join(c.Home, "file")
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			c.Home = f
		}, "not a directory"},
		{"bad master key", func(t *testing.T, c *config.Config) {
			t.Setenv(crypto.MasterKeyEnv, "short")
		}, "key"},
		{"bad docker host", func(_ *testing.T, c *config.Config) { c.Docker.Host = "::not a host" }, "host"},
		{"unknown workflow profile", func(_ *testing.T, c *config.Config) { c.Workflows.Profile = "nope" }, `wire workflows: workflows.profile "nope" is not a profile`},
		{"bad database driver", func(_ *testing.T, c *config.Config) { c.Database.Driver = "oracle" }, "oracle"},
	}
	// One home for every case: migrations run once, later cases find the
	// schema current.
	base := testConfig(t, host)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			tc.mut(t, &cfg)
			a, err := New(context.Background(), &cfg, discardLog())
			if err == nil {
				_ = a.Close()
				t.Fatal("New succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	// Docker is down: gated components wait instead of failing the daemon.
	f := &fakeDocker{}
	f.pingFail.Store(true)
	a := newTestApp(t, startFakeDocker(t, f), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// Close is idempotent: the closers are dropped.
	if err := a.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

type comp struct {
	name string
	run  func(ctx context.Context) error
}

func (c comp) Name() string                  { return c.name }
func (c comp) Run(ctx context.Context) error { return c.run(ctx) }

func TestRunFirstErrorCancelsTheRest(t *testing.T) {
	boom := errors.New("boom")
	a := &App{}
	a.Add(comp{"bad", func(context.Context) error { return boom }})
	a.Add(comp{"waits", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})
	err := a.Run(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "bad: boom") {
		t.Fatalf("Run = %v, want the failing component's error", err)
	}
	// Errors after cancellation are shutdown noise, not failures.
	a = &App{}
	a.Add(comp{"late", func(ctx context.Context) error { <-ctx.Done(); return errors.New("closed") }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run after cancel = %v", err)
	}
}

func TestCloseReverseOrderJoinsErrors(t *testing.T) {
	var order []int
	e1, e2 := errors.New("one"), errors.New("two")
	a := &App{}
	a.AddCloser(func() error { order = append(order, 1); return e1 })
	a.AddCloser(func() error { order = append(order, 2); return nil })
	a.AddCloser(func() error { order = append(order, 3); return e2 })
	err := a.Close()
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("Close = %v", err)
	}
	if len(order) != 3 || order[0] != 3 || order[2] != 1 {
		t.Fatalf("order = %v, want reverse", order)
	}
}

func TestSortedWirersKeepsRegistrationOrderWithinAnOrder(t *testing.T) {
	saved := wirers
	t.Cleanup(func() { wirers = saved })
	wirers = nil
	noop := func(context.Context, *App) error { return nil }
	RegisterWirer(20, "b", noop)
	RegisterWirer(10, "a", noop)
	RegisterWirer(20, "c", noop)
	var got []string
	for _, w := range sortedWirers() {
		got = append(got, w.name)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("order = %v", got)
	}
	if len(wirers) != 3 || wirers[0].name != "b" {
		t.Error("sortedWirers reordered the registry")
	}
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func TestDockerGated(t *testing.T) {
	ran := false
	d := &dockerGated{name: "x", docker: pinger{}, log: discardLog(), run: func(context.Context) error { ran = true; return nil }}
	if d.Name() != "x" {
		t.Errorf("Name = %q", d.Name())
	}
	if err := d.Run(context.Background()); err != nil || !ran {
		t.Fatalf("Run = %v, ran = %v", err, ran)
	}
	// Docker down: wait (and warn once) until the context ends.
	ran = false
	d.docker = pinger{errors.New("down")}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil || ran {
		t.Fatalf("Run while down = %v, ran = %v", err, ran)
	}
}

func run(t *testing.T, c doctor.Check) doctor.Result {
	t.Helper()
	return c.Run(context.Background())
}

func TestBaseChecks(t *testing.T) {
	cfg := testConfig(t, "")
	ctx := context.Background()
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if got := checkNames(BaseChecks(cfg, nil, nil)); len(got) != 2 {
		t.Errorf("without store and engine: %v", got)
	}

	cs := BaseChecks(cfg, st, pinger{errors.New("refused")})
	if r := run(t, findCheck(t, cs, "docker")); r.Severity != doctor.Error || !strings.Contains(r.Detail, "refused") {
		t.Errorf("docker down: %+v", r)
	}
	cs = BaseChecks(cfg, st, pinger{})
	if r := run(t, findCheck(t, cs, "docker")); r.Severity != doctor.OK {
		t.Errorf("docker up: %+v", r)
	}

	// A fresh database has pending migrations; the fix applies them.
	mig := findCheck(t, cs, "database migrations")
	r := run(t, mig)
	if r.Severity != doctor.Error || r.Fix == nil || !strings.Contains(r.Detail, "pending migration") {
		t.Fatalf("fresh db: %+v", r)
	}
	if _, err := r.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if r := run(t, mig); r.Severity != doctor.OK {
		t.Errorf("after fix: %+v", r)
	}
	_ = st.Close()
	if r := run(t, mig); r.Severity != doctor.Error {
		t.Errorf("closed db: %+v", r)
	}
}

var (
	okDigest   = "img@sha256:" + strings.Repeat("a", 64)
	goneDigest = "img@sha256:" + strings.Repeat("b", 64)
)

func TestSessionChecks(t *testing.T) {
	f := &fakeDocker{images: map[string]string{okDigest: okDigest}}
	a := newTestApp(t, startFakeDocker(t, f), nil)
	ctx := context.Background()

	// Master key: ICARO_MASTER_KEY is a valid backend, a malformed one is not.
	key := findCheck(t, SessionChecks(a.Cfg, nil, nil), "master key")
	if r := run(t, key); r.Severity != doctor.OK || !strings.Contains(r.Detail, "stored in") {
		t.Errorf("master key: %+v", r)
	}
	t.Setenv(crypto.MasterKeyEnv, "short")
	if r := run(t, key); r.Severity != doctor.Error {
		t.Errorf("bad master key: %+v", r)
	}
	if n := len(SessionChecks(a.Cfg, nil, nil)); n != 1 {
		t.Errorf("without a store: %d checks", n)
	}

	cs := SessionChecks(a.Cfg, a.Store, a.Docker)
	orphans := findCheck(t, cs, "orphaned sessions")
	if r := run(t, orphans); r.Severity != doctor.OK {
		t.Errorf("no orphans: %+v", r)
	}
	proj, err := a.Store.UpsertProject(ctx, &store.Project{Name: "p", RootPath: shortDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	row := &store.SessionRow{Kind: "agent", ProjectID: proj.ID, Profile: "shell", Branch: "b", WorktreePath: "/w", ContainerID: "c1", Status: session.StatusOrphaned, Error: "container gone"}
	if err := a.Store.CreateSession(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Doctor lists a second orphan that the daemon restarts before --fix
	// runs: the fix must leave its container alone.
	revived := &store.SessionRow{Kind: "agent", ProjectID: proj.ID, Profile: "shell", Branch: "r", WorktreePath: "/r", ContainerID: "c2", Status: session.StatusOrphaned, Error: "daemon stopped"}
	if err := a.Store.CreateSession(ctx, revived); err != nil {
		t.Fatal(err)
	}
	r := run(t, orphans)
	if ok, err := a.Store.TransitionSession(ctx, revived.ID, session.StatusRunning, session.StatusOrphaned); err != nil || !ok {
		t.Fatal(err)
	}
	f.removeFail.Store(true)
	if _, err := r.Fix(ctx); err == nil || !strings.Contains(err.Error(), "engine busy") {
		t.Fatalf("fix with a failing engine = %v", err)
	}
	f.removeFail.Store(false)
	if r.Severity != doctor.Warning || !strings.Contains(r.Detail, row.ID+" (container gone)") || r.Fix == nil {
		t.Fatalf("orphan: %+v", r)
	}
	if msg, err := r.Fix(ctx); err != nil || !strings.Contains(msg, "cleaned up 1") || !strings.Contains(msg, "skipped 1 no longer orphaned") {
		t.Fatalf("fix = %q, %v", msg, err)
	}
	if f.removed.Load() != 1 {
		t.Errorf("only the orphan's container is removed (%d)", f.removed.Load())
	}
	if got, _ := a.Store.GetSession(ctx, revived.ID); got.Status != session.StatusRunning || got.ContainerID != "c2" {
		t.Errorf("the restarted session was touched: %+v", got)
	}
	if r := run(t, orphans); r.Severity != doctor.OK {
		t.Errorf("after fix: %+v", r)
	}
	// Without an engine there is nothing to fix with.
	if err := a.Store.CreateSession(ctx, &store.SessionRow{Kind: "agent", ProjectID: proj.ID, Profile: "shell", Branch: "b2", WorktreePath: "/w2", Status: session.StatusOrphaned}); err != nil {
		t.Fatal(err)
	}
	if r := run(t, findCheck(t, SessionChecks(a.Cfg, a.Store, nil), "orphaned sessions")); r.Fix != nil {
		t.Error("fix offered without an engine")
	}

	pinned := findCheck(t, cs, "pinned images")
	if r := run(t, pinned); r.Severity != doctor.OK {
		t.Errorf("no pins: %+v", r)
	}
	if err := a.Store.PinImage(ctx, "img:1", okDigest); err != nil {
		t.Fatal(err)
	}
	if r := run(t, pinned); r.Severity != doctor.OK {
		t.Errorf("present pin: %+v", r)
	}
	if err := a.Store.PinImage(ctx, "img:2", goneDigest); err != nil {
		t.Fatal(err)
	}
	r = run(t, pinned)
	if r.Severity != doctor.Error || !strings.Contains(r.Detail, "img:2: pinned digest is missing locally") || r.Fix == nil {
		t.Fatalf("missing pin: %+v", r)
	}
	if msg, err := r.Fix(ctx); err != nil || !strings.Contains(msg, "re-pulled 1") || f.pulls.Load() != 1 {
		t.Fatalf("fix = %q, %v (pulls %d)", msg, err, f.pulls.Load())
	}
	// A locally built image cannot be re-pulled.
	if err := a.Store.PinImage(ctx, "img:2", "sha256:local"); err != nil {
		t.Fatal(err)
	}
	r = run(t, pinned)
	if _, err := r.Fix(ctx); err == nil || !strings.Contains(err.Error(), "built locally") {
		t.Errorf("fix of a local image = %v", err)
	}

	f.pingFail.Store(true)
	if r := run(t, pinned); r.Severity != doctor.Skipped {
		t.Errorf("docker down: %+v", r)
	}
	_ = a.Store.Close()
	f.pingFail.Store(false)
	if r := run(t, pinned); r.Severity != doctor.Error {
		t.Errorf("closed store, pins: %+v", r)
	}
	if r := run(t, orphans); r.Severity != doctor.Error {
		t.Errorf("closed store, orphans: %+v", r)
	}
}

func TestPreviewChecksAndManager(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), func(c *config.Config) {
		c.Previews.Roots = []string{"/extra"}
	})
	cs := PreviewChecks(context.Background(), a.Cfg, a.Store, a.Docker)
	if got := strings.Join(checkNames(cs), ","); !strings.Contains(got, "preview proxy network") || !strings.Contains(got, "preview compose lint") || !strings.Contains(got, "preview DNS") {
		t.Errorf("checks = %s", got)
	}
	// No network and no proxy yet: a fresh install is fine.
	if r := run(t, findCheck(t, cs, "preview proxy network")); r.Severity != doctor.OK {
		t.Errorf("network: %+v", r)
	}
	if n := len(PreviewChecks(context.Background(), a.Cfg, nil, nil)); n != 1 {
		t.Errorf("without store and engine: %d checks", n)
	}
	// An explicit DOCKER_HOST also reaches docker compose.
	if NewPreviewManager(a.Cfg, a.Store, a.Docker, discardLog()) == nil {
		t.Fatal("no manager")
	}
	a.Cfg.Docker.Host = ""
	if NewPreviewManager(a.Cfg, a.Store, a.Docker, discardLog()) == nil {
		t.Fatal("no manager without DOCKER_HOST")
	}
}

func TestStepUserWithoutSessions(t *testing.T) {
	if got := stepUser(&App{}); got != containerUser() {
		t.Errorf("stepUser = %q, want %q", got, containerUser())
	}
	u := containerUser()
	if os.Getuid() == 0 && u != "" {
		t.Errorf("root daemon got user %q", u)
	}
}

func TestBuildVersion(t *testing.T) {
	if v := buildVersion(); v == "" {
		t.Error("empty version")
	}
}

func TestSetWorkflowEngineWithoutIntegrations(t *testing.T) {
	// An App that never ran the integrations wirer is left alone.
	a := &App{}
	SetWorkflowEngine(a, nil, nil)
	if IntegrationHub(a) != nil || Scheduler(a) != nil {
		t.Error("integrations area appeared")
	}
}

func TestDockerHint(t *testing.T) {
	if dockerHint() == "" {
		t.Error("empty hint")
	}
}
