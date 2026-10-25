package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/doctor"
	"icaro/internal/paths"
	"icaro/internal/store"
)

var errBoom = errors.New("boom")

// failStore is the real store with an error injected per method name.
type failStore struct {
	*store.Store
	mu   sync.Mutex
	fail map[string]error
}

func (s *failStore) set(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = err
}

func (s *failStore) err(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail[method]
}

func (s *failStore) CreatePreview(ctx context.Context, p *store.PreviewInstance) error {
	if err := s.err("CreatePreview"); err != nil {
		return err
	}
	return s.Store.CreatePreview(ctx, p)
}
func (s *failStore) UpdatePreview(ctx context.Context, p *store.PreviewInstance) error {
	if err := s.err("UpdatePreview"); err != nil {
		return err
	}
	return s.Store.UpdatePreview(ctx, p)
}
func (s *failStore) GetPreviewByPath(ctx context.Context, path string) (*store.PreviewInstance, error) {
	if err := s.err("GetPreviewByPath"); err != nil {
		return nil, err
	}
	return s.Store.GetPreviewByPath(ctx, path)
}
func (s *failStore) ListPreviews(ctx context.Context) ([]*store.PreviewInstance, error) {
	if err := s.err("ListPreviews"); err != nil {
		return nil, err
	}
	return s.Store.ListPreviews(ctx)
}
func (s *failStore) AllowPreviewFinding(ctx context.Context, path, id, actor string) error {
	if err := s.err("AllowPreviewFinding"); err != nil {
		return err
	}
	return s.Store.AllowPreviewFinding(ctx, path, id, actor)
}
func (s *failStore) PreviewAllowances(ctx context.Context, path string) (map[string]bool, error) {
	if err := s.err("PreviewAllowances"); err != nil {
		return nil, err
	}
	return s.Store.PreviewAllowances(ctx, path)
}
func (s *failStore) ClearPreviewAllowances(ctx context.Context, path string) error {
	if err := s.err("ClearPreviewAllowances"); err != nil {
		return err
	}
	return s.Store.ClearPreviewAllowances(ctx, path)
}

// failCompose is fakeCompose with injectable Config/Running errors.
type failCompose struct {
	*fakeCompose
	mu        sync.Mutex
	configErr error
}

func (c *failCompose) setConfigErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configErr = err
}

func (c *failCompose) Config(ctx context.Context, project, dir string, files []string) ([]byte, error) {
	c.mu.Lock()
	err := c.configErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.fakeCompose.Config(ctx, project, dir, files)
}

// failEngine is fakeEngine with an error injected per method name.
type failEngine struct {
	*fakeEngine
	mu   sync.Mutex
	fail map[string]error
}

func newFailEngine() *failEngine {
	return &failEngine{fakeEngine: newFakeEngine(), fail: map[string]error{}}
}

func (e *failEngine) set(method string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fail[method] = err
}

func (e *failEngine) err(method string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fail[method]
}

func (e *failEngine) EnsureImage(ctx context.Context, ref string) error {
	if err := e.err("EnsureImage"); err != nil {
		return err
	}
	return e.fakeEngine.EnsureImage(ctx, ref)
}
func (e *failEngine) EnsureNetwork(ctx context.Context, n string, l map[string]string) (bool, error) {
	if err := e.err("EnsureNetwork"); err != nil {
		return false, err
	}
	return e.fakeEngine.EnsureNetwork(ctx, n, l)
}
func (e *failEngine) NetworkExists(ctx context.Context, n string) (bool, error) {
	if err := e.err("NetworkExists"); err != nil {
		return false, err
	}
	return e.fakeEngine.NetworkExists(ctx, n)
}
func (e *failEngine) CreateProxy(ctx context.Context, s docker.ProxySpec) (string, error) {
	if err := e.err("CreateProxy"); err != nil {
		return "", err
	}
	return e.fakeEngine.CreateProxy(ctx, s)
}
func (e *failEngine) StartContainer(ctx context.Context, id string) error {
	if err := e.err("StartContainer"); err != nil {
		return err
	}
	return e.fakeEngine.StartContainer(ctx, id)
}
func (e *failEngine) Inspect(ctx context.Context, id string) (docker.State, error) {
	if err := e.err("Inspect"); err != nil {
		return docker.State{}, err
	}
	return e.fakeEngine.Inspect(ctx, id)
}
func (e *failEngine) RemoveContainer(ctx context.Context, id string) error {
	if err := e.err("RemoveContainer"); err != nil {
		return err
	}
	return e.fakeEngine.RemoveContainer(ctx, id)
}

// failEnv is a manager over failStore, failCompose and failEngine. It is
// not running until start, so failures can be armed for startup.
type failEnv struct {
	t       *testing.T
	ctx     context.Context
	m       *Manager
	st      *failStore
	compose *failCompose
	engine  *failEngine
	layout  paths.Layout
}

func newFailEnv(t *testing.T) *failEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	l := paths.New(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	e := &failEnv{t: t, ctx: ctx, st: &failStore{Store: st, fail: map[string]error{}},
		compose: &failCompose{fakeCompose: newFakeCompose()}, engine: newFailEngine(), layout: l}
	e.m = New(Options{Layout: l, HTTPPort: 18780, Debounce: 20 * time.Millisecond}, e.st, e.compose, e.engine,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e
}

func (e *failEnv) start() *failEnv {
	go func() { _ = e.m.Run(e.ctx) }()
	return e
}

// syncNow runs one sync of path on its worker and returns its error.
func (e *failEnv) syncNow(path string, force bool) error {
	_, err := e.m.enqueue(e.ctx, path, true, func(ctx context.Context) error { return e.m.sync(ctx, path, force) })
	return err
}

func (e *failEnv) add(dir string) *store.PreviewInstance {
	e.t.Helper()
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *failEnv) get(path string) *store.PreviewInstance {
	e.t.Helper()
	// Rows are keyed by the normalized path (symlinks resolved).
	p, err := e.st.Store.GetPreviewByPath(e.ctx, norm(path))
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestNewDefaults(t *testing.T) {
	m := New(Options{}, nil, nil, nil, nil)
	if m.opts.HTTPPort != 8780 || m.opts.Debounce != 2*time.Second || m.opts.TraefikImage != "traefik:v3.1" || m.log == nil || m.Name() != "previews" {
		t.Fatalf("%+v", m.opts)
	}
}

func TestManagerNotRunning(t *testing.T) {
	t.Parallel()
	m := New(Options{}, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Before Run, calls wait for the actor and give up with their context.
	if err := m.do(ctx, func(*actorState) {}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.proxyDo(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	// After Run returns, every call fails fast with ErrStopped.
	e := newFailEnv(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.m.Run(runCtx); close(done) }()
	stop()
	<-done
	dir := worktree(t, t.TempDir(), "w", webCompose)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir}); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if _, err := e.m.Down(e.ctx, dir); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
	if err := e.m.proxyDo(e.ctx, false); !errors.Is(err, ErrStopped) {
		t.Fatal(err)
	}
}

func TestCallerContextEndsWhileWorking(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	// The actor is busy when the caller's context ends: do returns.
	ctx, cancel := context.WithCancel(e.ctx)
	release := make(chan struct{})
	if err := e.m.do(ctx, func(*actorState) { cancel(); <-release }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	// A waited-for job outlives its caller's context. The context may end
	// while enqueue still hands the job over (then there is no result
	// channel) or while it waits for the result; retry until the latter.
	for i := 0; ; i++ {
		ctx, cancel := context.WithCancel(e.ctx)
		release := make(chan struct{})
		started := make(chan struct{})
		go func() {
			<-started
			_ = e.m.do(e.ctx, func(*actorState) {}) // the hand-over is done
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		res, err := e.m.enqueue(ctx, fmt.Sprint("/p", i), true, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		if res != nil {
			if err := <-res; err != nil {
				t.Fatal(err)
			}
			return
		}
		if i == 50 {
			t.Fatal("enqueue never waited for the result")
		}
	}
}

func TestRemoveWaitsForRetiringWorker(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	release := make(chan struct{})
	order := make(chan string, 2)
	// A job queued while an earlier retiring worker for the same path is
	// still busy runs after it, never concurrently.
	_, err := e.m.enqueueRetire(e.ctx, "/p", false, true, func(context.Context) error {
		<-release
		order <- "first"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.enqueue(e.ctx, "/p", false, func(context.Context) error {
		order <- "second"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	if a, b := <-order, <-order; a != "first" || b != "second" {
		t.Fatal(a, b)
	}
}

func TestResumeSkipsStoppedAndSurvivesErrors(t *testing.T) {
	t.Parallel()
	// A store that cannot list previews does not stop the manager.
	e := newFailEnv(t)
	e.st.set("ListPreviews", errBoom)
	e.start()
	if _, err := e.m.Lint(e.ctx, worktree(t, t.TempDir(), "w", webCompose)); err != nil {
		t.Fatal(err)
	}

	e = newFailEnv(t)
	// The daemon stores normalized paths (symlinks resolved).
	parent := norm(t.TempDir())
	stopped := worktree(t, parent, "a-stopped", webCompose)
	vanished := filepath.Join(parent, "b-vanished")
	for _, p := range []*store.PreviewInstance{
		{WorktreePath: stopped, ComposeProject: "icaro-a", Slug: "a", Hostname: "a.localhost", Status: store.PreviewStopped},
		{WorktreePath: vanished, ComposeProject: "icaro-b", Slug: "b", Hostname: "b.localhost", Status: store.PreviewRunning},
	} {
		if err := e.st.Store.CreatePreview(e.ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Cleaning the vanished worktree fails; its row is kept for a retry.
	e.st.set("ClearPreviewAllowances", errBoom)
	e.start()
	waitFor(t, func() bool { return e.compose.count(e.compose.downs, "icaro-b") == 1 })
	if e.compose.count(e.compose.ups, "icaro-a") != 0 || e.get(stopped).Status != store.PreviewStopped {
		t.Fatal("a stopped preview must not be resumed")
	}
	if _, err := e.st.Store.GetPreviewByPath(e.ctx, vanished); err != nil {
		t.Fatal("row deleted although cleanup failed")
	}
}

func TestAddUpdatesOwnerAndReportsStoreErrors(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	parent := t.TempDir()
	dir := worktree(t, parent, "w", webCompose)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, SessionID: "S1", Wait: true}); err != nil {
		t.Fatal(err)
	}
	// A new session taking the worktree over is recorded.
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, SessionID: "S2", ProjectID: "P", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if p := e.get(dir); p.SessionID != "S2" || p.ProjectID != "P" {
		t.Fatalf("%+v", p)
	}

	other := worktree(t, parent, "o", webCompose)
	e.st.set("ListPreviews", errBoom)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: other}); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.st.set("ListPreviews", nil)
	e.st.set("CreatePreview", errBoom)
	if _, err := e.m.WorktreeAdded(e.ctx, "S", "P", other); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.st.set("GetPreviewByPath", errBoom)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: other}); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if err := e.m.Logs(e.ctx, other, 10, io.Discard); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
}

func TestLoadProjectFallsBackWithoutDockerCLI(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	dir := worktree(t, t.TempDir(), "w", webCompose)
	// Without the docker CLI the raw compose file is linted instead.
	e.compose.setConfigErr(fmt.Errorf("docker: %w", exec.ErrNotFound))
	res, err := e.m.Lint(e.ctx, dir)
	if err != nil || res.Findings == nil || res.Blocked == nil || len(res.Findings) != 0 {
		t.Fatalf("a clean compose file lints to empty (not null) lists: %+v %v", res, err)
	}
	// Any other compose error is reported, never papered over.
	e.compose.setConfigErr(errBoom)
	if _, err := e.m.Lint(e.ctx, dir); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if err := e.m.Allow(e.ctx, dir, []string{"x"}, "t"); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if p := e.get(norm(dir)); p.Status != store.PreviewError || !strings.Contains(p.Error, "boom") {
		t.Fatalf("%+v", p)
	}
	e.compose.setConfigErr(nil)

	if _, err := e.m.Lint(e.ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no compose file") {
		t.Fatal(err)
	}
	e.st.set("PreviewAllowances", errBoom)
	if _, err := e.m.Lint(e.ctx, dir); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if err := e.syncNow(e.get(dir).WorktreePath, true); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
}

func TestAllowErrors(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	evil := "services:\n  web:\n    image: x\n    privileged: true\n    labels: {icaro.preview.port: \"80\"}\n"
	dir := worktree(t, t.TempDir(), "evil", evil)
	// Allowing a finding of a worktree that has no preview yet records the
	// allowance and starts nothing.
	if err := e.m.Allow(e.ctx, dir, []string{"web/privileged"}, "t"); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.m.List(e.ctx); len(list) != 0 {
		t.Fatal(list)
	}
	e.st.set("AllowPreviewFinding", errBoom)
	if err := e.m.Allow(e.ctx, dir, []string{"web/privileged"}, "t"); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if err := e.m.Allow(e.ctx, t.TempDir(), nil, "t"); err == nil {
		t.Fatal("allow without a compose file")
	}
}

func TestDownAndRemoveErrors(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	parent := t.TempDir()
	if _, err := e.m.Down(e.ctx, filepath.Join(parent, "never-added")); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	dir := worktree(t, parent, "w", webCompose)
	p := e.add(dir)

	// A route that cannot be removed stops Down and Remove alike.
	route := filepath.Join(e.layout.Previews(), "traefik", p.ComposeProject+".yaml")
	_ = os.Remove(route)
	if err := os.MkdirAll(filepath.Join(route, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Down(e.ctx, dir); err == nil {
		t.Fatal("down with an unremovable route")
	}
	if err := e.m.Remove(e.ctx, dir); err == nil {
		t.Fatal("remove with an unremovable route")
	}
	_ = os.RemoveAll(route)

	e.st.set("ClearPreviewAllowances", errBoom)
	if err := e.m.Remove(e.ctx, dir); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.st.set("ClearPreviewAllowances", nil)
	e.st.set("GetPreviewByPath", errBoom)
	if err := e.m.Remove(e.ctx, dir); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.st.set("GetPreviewByPath", nil)
	if err := e.m.Remove(e.ctx, dir); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFailures(t *testing.T) {
	t.Parallel()
	const unrouted = "services:\n  web:\n    image: x\n"
	const hostNet = "services:\n  web:\n    image: x\n    network_mode: none\n    labels: {icaro.preview.port: \"80\"}\n"
	cases := []struct {
		name    string
		compose string
		arm     func(e *failEnv, dir string)
		status  string
		want    string
	}{
		{"no route", unrouted, nil, store.PreviewError, "no routable service"},
		{"network_mode", hostNet, nil, store.PreviewError, "network_mode"},
		{"unreadable config file", webCompose, func(_ *failEnv, dir string) {
			_ = os.Symlink(".env", filepath.Join(dir, ".env")) // a symlink loop
		}, store.PreviewError, ".env"},
		{"config file is a directory", webCompose, func(_ *failEnv, dir string) {
			_ = os.Mkdir(filepath.Join(dir, ".env"), 0o700)
		}, store.PreviewError, ""},
		{"override dir is a file", webCompose, func(e *failEnv, dir string) {
			_ = os.WriteFile(filepath.Join(e.layout.Previews(), NameFor(norm(dir)).Project), nil, 0o600)
		}, store.PreviewError, ""},
		{"override is a directory", webCompose, func(e *failEnv, dir string) {
			_ = os.MkdirAll(filepath.Join(e.layout.Previews(), NameFor(norm(dir)).Project, "override.yaml"), 0o700)
		}, store.PreviewError, ""},
		{"route is a directory", webCompose, func(e *failEnv, dir string) {
			_ = os.MkdirAll(filepath.Join(e.layout.Previews(), "traefik", NameFor(norm(dir)).Project+".yaml", "x"), 0o700)
		}, store.PreviewError, ""},
		{"proxy", webCompose, func(e *failEnv, _ string) { e.engine.set("EnsureNetwork", errBoom) }, store.PreviewError, "preview proxy: boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newFailEnv(t).start()
			dir := worktree(t, t.TempDir(), "w", c.compose)
			if c.arm != nil {
				c.arm(e, dir)
			}
			_, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
			p := e.get(norm(dir))
			if err == nil || p.Status != c.status || !strings.Contains(p.Error, c.want) {
				t.Fatalf("%v %+v", err, p)
			}
			if e.compose.count(e.compose.ups, p.ComposeProject) != 0 && c.name != "route is a directory" {
				t.Fatal("a failed sync must not leave the stack up")
			}
		})
	}
}

func norm(p string) string {
	n, _ := NormalizePath(p)
	return n
}

func TestSyncStateChanges(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	dir := worktree(t, t.TempDir(), "w", webCompose)
	path := norm(dir)
	if err := e.syncNow(path, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sync of an unknown worktree: %v", err)
	}
	p := e.add(dir)

	// Unchanged and alive, but the proxy cannot be ensured: reported.
	e.engine.set("EnsureNetwork", errBoom)
	if err := e.syncNow(path, false); !errors.Is(err, errBoom) || e.get(path).Status != store.PreviewError {
		t.Fatal(err)
	}
	e.engine.set("EnsureNetwork", nil)
	if err := e.syncNow(path, true); err != nil {
		t.Fatal(err)
	}

	// The final state cannot be persisted: the sync fails.
	e.st.set("UpdatePreview", errBoom)
	if err := e.syncNow(path, true); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	// Persisting a failure fails too: the original error is still returned.
	e.compose.upErr = errors.New("up failed")
	if err := e.syncNow(path, true); err == nil || err.Error() != "up failed" {
		t.Fatal(err)
	}
	e.compose.upErr = nil
	e.st.set("UpdatePreview", nil)

	// The compose file is deleted: the preview stops with an explanation.
	if err := os.Remove(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := e.syncNow(path, false); err == nil {
		t.Fatal("expected an error")
	}
	if got := e.get(path); got.Status != store.PreviewStopped || !strings.Contains(got.Error, "no compose file") {
		t.Fatalf("%+v", got)
	}
	if e.compose.count(e.compose.downs, p.ComposeProject) == 0 {
		t.Fatal("stack must be stopped")
	}
}

func TestEnsureProxyStates(t *testing.T) {
	ctx := context.Background()
	opts := func(t *testing.T) ProxyOptions {
		return ProxyOptions{Name: "px", Image: "traefik:x", HTTPPort: 1, DynamicDir: filepath.Join(t.TempDir(), "dyn")}
	}
	t.Run("restarts a stopped proxy with the same config", func(t *testing.T) {
		e := newFailEngine()
		o := opts(t)
		if err := EnsureProxy(ctx, e, o); err != nil || e.creates != 1 {
			t.Fatal(err)
		}
		e.proxy.Running = false
		if err := EnsureProxy(ctx, e, o); err != nil || e.creates != 1 || !e.proxy.Running {
			t.Fatalf("%v creates=%d", err, e.creates)
		}
		if err := EnsureProxy(ctx, e, o); err != nil || e.creates != 1 {
			t.Fatal("a running proxy is left alone")
		}
		// A configuration change recreates it.
		o.HTTPPort = 2
		if err := EnsureProxy(ctx, e, o); err != nil || e.creates != 2 {
			t.Fatal(err)
		}
	})
	t.Run("a concurrent create wins", func(t *testing.T) {
		e := newFailEngine()
		e.set("CreateProxy", errors.New(`Conflict. The container name "/px" is already in use`))
		if err := EnsureProxy(ctx, e, opts(t)); err != nil {
			t.Fatal(err)
		}
	})
	for _, method := range []string{"EnsureNetwork", "Inspect", "EnsureImage", "CreateProxy", "StartContainer"} {
		t.Run(method, func(t *testing.T) {
			e := newFailEngine()
			e.set(method, errBoom)
			if err := EnsureProxy(ctx, e, opts(t)); !errors.Is(err, errBoom) {
				t.Fatal(err)
			}
		})
	}
	t.Run("RemoveContainer", func(t *testing.T) {
		e := newFailEngine()
		o := opts(t)
		_ = EnsureProxy(ctx, e, o)
		e.set("RemoveContainer", errBoom)
		o.Image = "traefik:y"
		if err := EnsureProxy(ctx, e, o); !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
	})
	t.Run("dynamic dir", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "f")
		_ = os.WriteFile(file, nil, 0o600)
		o := opts(t)
		o.DynamicDir = filepath.Join(file, "dyn")
		if err := EnsureProxy(ctx, newFailEngine(), o); err == nil {
			t.Fatal("dynamic dir under a file")
		}
	})
}

func TestWriteRouteErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, nil, 0o600)
	if err := writeRoute(filepath.Join(file, "d"), "r", nil); err == nil {
		t.Fatal("route dir under a file")
	}
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, ".r.tmp"), 0o700)
	if err := writeRoute(dir, "r", nil); err == nil {
		t.Fatal("temp file is a directory")
	}
}

func TestNetworkCheckErrors(t *testing.T) {
	ctx := context.Background()
	e := newFailEngine()
	check := NetworkCheck(e, "px")
	e.set("NetworkExists", errBoom)
	if r := check.Run(ctx); r.Severity != doctor.Error || !strings.Contains(r.Detail, "docker: boom") {
		t.Fatalf("%+v", r)
	}
	e.set("NetworkExists", nil)
	e.set("Inspect", errBoom)
	if r := check.Run(ctx); r.Severity != doctor.Error {
		t.Fatalf("no network, inspect fails: %+v", r)
	}
	e.networks[Network] = true
	if r := check.Run(ctx); r.Severity != doctor.Error {
		t.Fatalf("network, inspect fails: %+v", r)
	}
	e.set("Inspect", nil)
	if r := check.Run(ctx); r.Severity != doctor.OK || !strings.Contains(r.Detail, "first preview") {
		t.Fatalf("network without proxy yet: %+v", r)
	}
	// The stopped-proxy fix starts it.
	e.proxy = &docker.State{Exists: true}
	r := check.Run(ctx)
	if r.Fix == nil {
		t.Fatal(r)
	}
	if _, err := r.Fix(ctx); err != nil || !e.proxy.Running {
		t.Fatal(err)
	}
	// The missing-network fix reports engine errors.
	delete(e.networks, Network)
	r = check.Run(ctx)
	e.set("EnsureNetwork", errBoom)
	if _, err := r.Fix(ctx); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.set("EnsureNetwork", nil)
	e.set("RemoveContainer", errBoom)
	if _, err := r.Fix(ctx); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
}

type previewLister struct {
	list []*store.PreviewInstance
	err  error
}

func (l previewLister) ListPreviews(context.Context) ([]*store.PreviewInstance, error) {
	return l.list, l.err
}

func TestComposeCheck(t *testing.T) {
	ctx := context.Background()
	if r := ComposeCheck(previewLister{err: errBoom}).Run(ctx); r.Severity != doctor.Error {
		t.Fatalf("%+v", r)
	}
	ok := previewLister{list: []*store.PreviewInstance{{Status: store.PreviewRunning}}}
	if r := ComposeCheck(ok).Run(ctx); r.Severity != doctor.OK || r.Detail != "1 preview(s), none blocked" {
		t.Fatalf("%+v", r)
	}
	blocked := previewLister{list: []*store.PreviewInstance{
		{Status: store.PreviewRunning, WorktreePath: "/a"},
		{Status: store.PreviewBlocked, WorktreePath: "/b", Error: "web/privileged"},
	}}
	r := ComposeCheck(blocked).Run(ctx)
	if r.Severity != doctor.Error || !strings.Contains(r.Detail, "/b: web/privileged") || strings.Contains(r.Detail, "/a") || !strings.Contains(r.Detail, "icaro preview allow") {
		t.Fatalf("%+v", r)
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestDNSCheck(t *testing.T) {
	ctx := context.Background()
	for _, d := range []string{"", "localhost"} {
		c := DNSCheck(d, nil)
		if c.Name != "preview DNS .localhost" || c.Run(ctx).Severity != doctor.OK {
			t.Fatalf("%q", d)
		}
	}
	for _, c := range []struct {
		r    fakeResolver
		sev  doctor.Severity
		want string
	}{
		{fakeResolver{}, doctor.Warning, "does not resolve"},
		{fakeResolver{"icaro-probe.test": {}}, doctor.Warning, "does not resolve"},
		{fakeResolver{"icaro-probe.test": {"127.0.0.1", "10.0.0.1"}}, doctor.Warning, "not loopback"},
		{fakeResolver{"icaro-probe.test": {"not-an-ip"}}, doctor.Warning, "not loopback"},
		{fakeResolver{"icaro-probe.test": {"127.0.0.1", "::1"}}, doctor.OK, "resolves to loopback"},
	} {
		check := DNSCheck("test", c.r)
		if check.Name != "preview DNS .test" {
			t.Fatal(check.Name)
		}
		if r := check.Run(ctx); r.Severity != c.sev || !strings.Contains(r.Detail, c.want) {
			t.Fatalf("%v: %+v", c.r, r)
		}
	}
}
