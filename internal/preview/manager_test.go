package preview

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/paths"
	"icaro/internal/serial"
	"icaro/internal/store"
)

// fakeCompose records calls; Config returns the raw file (ParseCompose
// reads YAML and JSON alike).
type fakeCompose struct {
	ops     serial.Queue
	ups     map[string]int
	downs   map[string]int
	running map[string]int
	files   map[string][]string
	upErr   error
}

func newFakeCompose() *fakeCompose {
	return &fakeCompose{ops: serial.New(), ups: map[string]int{}, downs: map[string]int{}, running: map[string]int{}, files: map[string][]string{}}
}

// filesFor returns the compose files the last up of project got.
func (f *fakeCompose) filesFor(project string) (files []string) {
	f.ops.Do(func() { files = append([]string(nil), f.files[project]...) })
	return files
}

func (f *fakeCompose) Config(_ context.Context, _, _ string, files []string) ([]byte, error) {
	return os.ReadFile(files[0])
}

func (f *fakeCompose) Up(_ context.Context, project, _ string, files []string) (err error) {
	f.ops.Do(func() {
		if err = f.upErr; err != nil {
			return
		}
		f.ups[project]++
		f.running[project] = 1
		f.files[project] = files
	})
	return err
}

func (f *fakeCompose) Down(_ context.Context, project string) error {
	f.ops.Do(func() {
		f.downs[project]++
		f.running[project] = 0
	})
	return nil
}

func (f *fakeCompose) Running(_ context.Context, project string) (n int, _ error) {
	f.ops.Do(func() { n = f.running[project] })
	return n, nil
}

func (f *fakeCompose) Logs(_ context.Context, project string, _ int, w io.Writer) error {
	_, err := io.WriteString(w, "logs of "+project+"\n")
	return err
}

func (f *fakeCompose) count(m map[string]int, k string) (n int) {
	f.ops.Do(func() { n = m[k] })
	return n
}

type fakeEngine struct {
	ops      serial.Queue
	networks map[string]bool
	proxy    *docker.State
	creates  int
	restarts int
}

func newFakeEngine() *fakeEngine { return &fakeEngine{ops: serial.New(), networks: map[string]bool{}} }

// snapshot reads the counters on the queue: the manager's proxy loop
// writes them from its own goroutine.
func (e *fakeEngine) snapshot() (network bool, creates, restarts int) {
	e.ops.Do(func() { network, creates, restarts = e.networks[Network], e.creates, e.restarts })
	return network, creates, restarts
}

func (e *fakeEngine) EnsureImage(context.Context, string) error { return nil }
func (e *fakeEngine) EnsureNetwork(_ context.Context, n string, _ map[string]string) (created bool, _ error) {
	e.ops.Do(func() {
		created = !e.networks[n]
		e.networks[n] = true
	})
	return created, nil
}
func (e *fakeEngine) NetworkExists(_ context.Context, n string) (exists bool, _ error) {
	e.ops.Do(func() { exists = e.networks[n] })
	return exists, nil
}
func (e *fakeEngine) CreateProxy(_ context.Context, s docker.ProxySpec) (string, error) {
	e.ops.Do(func() {
		e.creates++
		e.proxy = &docker.State{Exists: true, Labels: s.Labels}
	})
	return s.Name, nil
}
func (e *fakeEngine) StartContainer(context.Context, string) error {
	e.ops.Do(func() { e.proxy.Running = true })
	return nil
}
func (e *fakeEngine) Inspect(context.Context, string) (st docker.State, _ error) {
	e.ops.Do(func() {
		if e.proxy != nil {
			st = *e.proxy
		}
	})
	return st, nil
}
func (e *fakeEngine) RemoveContainer(context.Context, string) error {
	e.ops.Do(func() { e.proxy = nil })
	return nil
}
func (e *fakeEngine) Restart(context.Context, string) error {
	e.ops.Do(func() { e.restarts++ })
	return nil
}

type env struct {
	t       *testing.T
	ctx     context.Context
	m       *Manager
	st      *store.Store
	compose *fakeCompose
	engine  *fakeEngine
	layout  paths.Layout
}

func newEnv(t *testing.T, mod func(*Options)) *env {
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
	opts := Options{Layout: l, HTTPPort: 18780, Debounce: 20 * time.Millisecond}
	if mod != nil {
		mod(&opts)
	}
	e := &env{t: t, ctx: ctx, st: st, compose: newFakeCompose(), engine: newFakeEngine(), layout: l}
	e.m = New(opts, st, e.compose, e.engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = e.m.Run(ctx) }()
	return e
}

const webCompose = "services:\n  web:\n    image: busybox\n    labels: {icaro.preview.port: \"8000\"}\n    volumes: [\".:/www\"]\n"

func worktree(t *testing.T, parent, name, compose string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if compose != "" {
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAddSyncAndNoRestartWithoutConfigChange(t *testing.T) {
	e := newEnv(t, nil)
	dir := worktree(t, t.TempDir(), "feature-x", webCompose)
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, SessionID: "S1", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != store.PreviewRunning || p.Hostname != "feature-x.localhost" || p.URL != "http://feature-x.localhost:18780" || p.Port != 8000 || p.Service != "web" {
		t.Fatalf("%+v", p)
	}
	if network, creates, _ := e.engine.snapshot(); e.compose.count(e.compose.ups, p.ComposeProject) != 1 || !network || creates != 1 {
		t.Fatal("expected one up, the network and the proxy")
	}
	files := e.compose.filesFor(p.ComposeProject)
	if len(files) != 2 || files[0] != filepath.Join(p.WorktreePath, "compose.yaml") {
		t.Fatalf("user compose must be passed unchanged: %v", files)
	}
	override, _ := os.ReadFile(files[1])
	if !strings.Contains(string(override), Network) || strings.HasPrefix(files[1], p.WorktreePath) {
		t.Fatal("override must live outside the worktree and join the preview network")
	}
	route, err := os.ReadFile(filepath.Join(e.layout.Previews(), "traefik", p.ComposeProject+".yaml"))
	if err != nil || !strings.Contains(string(route), "feature-x.localhost") {
		t.Fatalf("route file: %s %v", route, err)
	}
	if !strings.Contains(e.m.CurlHint(p), "Host: feature-x.localhost") {
		t.Fatal(e.m.CurlHint(p))
	}

	// A code edit does not restart; a lockfile change does.
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("v2"), 0o600)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if n := e.compose.count(e.compose.ups, p.ComposeProject); n != 1 {
		t.Fatalf("code edit restarted the preview (%d ups)", n)
	}
	_ = os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}"), 0o600)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if n := e.compose.count(e.compose.ups, p.ComposeProject); n != 2 {
		t.Fatalf("lockfile change must restart (%d ups)", n)
	}
	// A stack that died is brought back even when unchanged.
	_ = e.compose.Down(e.ctx, p.ComposeProject)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if n := e.compose.count(e.compose.ups, p.ComposeProject); n != 3 {
		t.Fatalf("dead stack not restarted (%d ups)", n)
	}
	var sb strings.Builder
	if err := e.m.Logs(e.ctx, p.ID, 10, &sb); err != nil || !strings.Contains(sb.String(), p.ComposeProject) {
		t.Fatal(sb.String(), err)
	}
	got, err := e.m.Get(e.ctx, dir)
	if err != nil || got.SessionID != "S1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSameSlugDoesNotCollide(t *testing.T) {
	e := newEnv(t, nil)
	a := worktree(t, t.TempDir(), "app", webCompose)
	b := worktree(t, t.TempDir(), "app", webCompose)
	pa, err := e.m.Add(e.ctx, AddRequest{Path: a, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := e.m.Add(e.ctx, AddRequest{Path: b, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if pa.Hostname == pb.Hostname || pa.ComposeProject == pb.ComposeProject || pa.Slug != pb.Slug {
		t.Fatalf("collision: %+v %+v", pa, pb)
	}
	if pa.Status != store.PreviewRunning || pb.Status != store.PreviewRunning {
		t.Fatal("both must run")
	}
}

func TestDangerousComposeIsBlockedAndExplained(t *testing.T) {
	e := newEnv(t, nil)
	dir := worktree(t, t.TempDir(), "evil", "services:\n  web:\n    image: x\n    privileged: true\n    labels: {icaro.preview.port: \"80\"}\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n")
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != store.PreviewBlocked || !strings.Contains(p.Error, "privileged") || !strings.Contains(p.Error, "engine socket") {
		t.Fatalf("%+v", p)
	}
	if e.compose.count(e.compose.ups, p.ComposeProject) != 0 {
		t.Fatal("a blocked compose file must never start")
	}
	res, err := e.m.Lint(e.ctx, dir)
	if err != nil || len(res.Blocked) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if err := e.m.Allow(e.ctx, dir, []string{"web/nonexistent"}, "test"); err == nil {
		t.Fatal("unknown finding allowed")
	}
	if err := e.m.Allow(e.ctx, dir, []string{"web/privileged"}, "test"); err != nil {
		t.Fatal(err)
	}
	p, _ = e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if p.Status != store.PreviewBlocked || strings.Contains(p.Error, "web/privileged:") {
		t.Fatalf("partial allow: %+v", p)
	}
	if err := e.m.Allow(e.ctx, dir, []string{"web/" + mountID(resolvedSock(t), "/var/run/docker.sock", false)}, "test"); err != nil {
		t.Fatal(err)
	}
	p, _ = e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true, Force: true})
	if p.Status != store.PreviewRunning {
		t.Fatalf("all findings allowed: %+v", p)
	}
	// The allowed mount is swapped for a worse one in the same position:
	// the old allowance does not carry over (ADR 0018).
	worse := "services:\n  web:\n    image: x\n    privileged: true\n    labels: {icaro.preview.port: \"80\"}\n    volumes: [\"/:/var/run/docker.sock\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(worse), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ = e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true, Force: true})
	if p.Status != store.PreviewBlocked || !strings.Contains(p.Error, "host root") {
		t.Fatalf("a changed mount must be blocked again: %+v", p)
	}
}

func TestRemoveIsIdempotentAndWorksAfterDeletion(t *testing.T) {
	e := newEnv(t, nil)
	parent := t.TempDir()
	dir := worktree(t, parent, "gone", webCompose)
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.m.Remove(e.ctx, dir); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
	}
	if _, err := e.st.GetPreview(e.ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("row must be gone")
	}
	if e.compose.count(e.compose.downs, p.ComposeProject) < 2 {
		t.Fatal("each remove must run compose down")
	}
	if _, err := os.Stat(filepath.Join(e.layout.Previews(), "traefik", p.ComposeProject+".yaml")); !os.IsNotExist(err) {
		t.Fatal("route file must be removed")
	}
	// Re-adding after removal works (a fresh worker).
	dir = worktree(t, parent, "gone", webCompose)
	if _, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Down(e.ctx, dir); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Get(e.ctx, dir)
	if got.Status != store.PreviewStopped {
		t.Fatal(got.Status)
	}
	// The watcher leaves a stopped preview alone; an explicit up restarts it.
	if got, _ = e.m.Add(e.ctx, AddRequest{Path: dir, Automatic: true, Wait: true}); got.Status != store.PreviewStopped {
		t.Fatal("watcher restarted a stopped preview")
	}
	if got, _ = e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true}); got.Status != store.PreviewRunning {
		t.Fatal(got.Status)
	}
}

func TestSessionHook(t *testing.T) {
	e := newEnv(t, nil)
	parent := t.TempDir()
	plain := worktree(t, parent, "plain", "")
	id, err := e.m.WorktreeAdded(e.ctx, "S1", "P1", plain)
	if err != nil || id != "" {
		t.Fatalf("worktree without compose: %q %v", id, err)
	}
	dir := worktree(t, parent, "with", webCompose)
	id, err = e.m.WorktreeAdded(e.ctx, "S2", "P1", dir)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	waitFor(t, func() bool {
		p, err := e.st.GetPreview(e.ctx, id)
		return err == nil && p.Status == store.PreviewRunning && p.SessionID == "S2" && p.ProjectID == "P1"
	})
	if err := e.m.WorktreeRemoved(e.ctx, "S2", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetPreview(e.ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("hook removal must clean up")
	}
}

func TestMacOSPollingAndProxyKick(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MacOS = true })
	dir := worktree(t, t.TempDir(), "mac", webCompose)
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	override, _ := os.ReadFile(e.compose.filesFor(p.ComposeProject)[1])
	if !strings.Contains(string(override), "CHOKIDAR_USEPOLLING") {
		t.Fatal("macOS override must enable polling watch")
	}
	if _, _, restarts := e.engine.snapshot(); restarts == 0 {
		t.Fatal("macOS must kick the proxy after a route change")
	}
}

func TestUpFailureAndResumeCleansVanished(t *testing.T) {
	e := newEnv(t, nil)
	e.compose.upErr = errors.New("boom")
	dir := worktree(t, t.TempDir(), "fails", webCompose)
	p, err := e.m.Add(e.ctx, AddRequest{Path: dir, Wait: true})
	if err == nil || p.Status != store.PreviewError || !strings.Contains(p.Error, "boom") {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := e.m.Add(e.ctx, AddRequest{Path: t.TempDir()}); err == nil {
		t.Fatal("dir without compose file must be refused")
	}

	// A new manager over the same store cleans up a vanished worktree.
	_ = os.RemoveAll(dir)
	m2 := New(e.m.opts, e.st, e.compose, e.engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = m2.Run(e.ctx) }()
	waitFor(t, func() bool {
		_, err := e.st.GetPreview(e.ctx, p.ID)
		return errors.Is(err, store.ErrNotFound)
	})
}

func TestWatcherDebouncesAndCleansUp(t *testing.T) {
	root := t.TempDir()
	e := newEnv(t, func(o *Options) { o.Roots = []string{root} })
	dir := worktree(t, filepath.Join(root, "proj"), "sess", webCompose)
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "x"), 0o750)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "x", "compose.yaml"), []byte(webCompose), 0o600)
	waitFor(t, func() bool {
		p, err := e.m.Get(e.ctx, dir)
		return err == nil && p.Status == store.PreviewRunning
	})
	list, _ := e.m.List(e.ctx)
	if len(list) != 1 {
		t.Fatalf("node_modules must be skipped: %d previews", len(list))
	}
	_ = os.RemoveAll(dir)
	waitFor(t, func() bool {
		list, _ := e.m.List(e.ctx)
		return len(list) == 0
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
