package session

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
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/store"
)

// fakeEngine is a channel-free fake: tests drive it from one goroutine
// at a time except for the start goroutines, which only append.
type fakeEngine struct {
	ops     chan func()
	specs   map[string]docker.Spec
	running map[string]bool
	n       int
	failOn  string
	errs    map[string]error // per-operation failures, see fail
}

func newFakeEngine() *fakeEngine {
	f := &fakeEngine{ops: make(chan func()), specs: map[string]docker.Spec{}, running: map[string]bool{}, errs: map[string]error{}}
	go func() {
		for op := range f.ops {
			op()
		}
	}()
	return f
}

func (f *fakeEngine) do(fn func()) {
	done := make(chan struct{})
	f.ops <- func() { fn(); close(done) }
	<-done
}

// fail makes every later call of op ("ping", "create", "start", "stop",
// "remove", "inspect") return err; a nil err clears it. "create-once" fails only the
// next create.
func (f *fakeEngine) fail(op string, err error) { f.do(func() { f.errs[op] = err }) }

func (f *fakeEngine) Ping(context.Context) (err error) {
	f.do(func() { err = f.errs["ping"] })
	return err
}

func (f *fakeEngine) CreateContainerOn(_ context.Context, spec docker.Spec, _ docker.NetSpec) (id string, err error) {
	f.do(func() {
		if f.failOn != "" && strings.Contains(spec.Name, f.failOn) {
			err = fmt.Errorf("boom")
			return
		}
		if err = f.errs["create"]; err != nil {
			return
		}
		if err = f.errs["create-once"]; err != nil {
			delete(f.errs, "create-once")
			return
		}
		f.n++
		id = fmt.Sprintf("c%d", f.n)
		f.specs[id] = spec
	})
	return id, err
}

func (f *fakeEngine) StartContainer(_ context.Context, id string) (err error) {
	f.do(func() {
		if err = f.errs["start"]; err == nil {
			f.running[id] = true
		}
	})
	return err
}

func (f *fakeEngine) StopContainer(_ context.Context, id string, _ int) (err error) {
	f.do(func() {
		if err = f.errs["stop"]; err == nil {
			f.running[id] = false
		}
	})
	return err
}

func (f *fakeEngine) RemoveContainer(_ context.Context, id string) (err error) {
	f.do(func() {
		if err = f.errs["remove"]; err == nil {
			delete(f.specs, id)
			delete(f.running, id)
		}
	})
	return err
}

func (f *fakeEngine) Inspect(_ context.Context, id string) (st docker.State, err error) {
	f.do(func() {
		if err = f.errs["inspect"]; err != nil {
			return
		}
		_, ok := f.specs[id]
		st = docker.State{Exists: ok, Running: f.running[id]}
	})
	return st, err
}

func (f *fakeEngine) kill(id string) { f.do(func() { delete(f.specs, id); delete(f.running, id) }) }

func (f *fakeEngine) spec(id string) (s docker.Spec) {
	f.do(func() { s = f.specs[id] })
	return s
}

type fakeEgress struct{}

func (fakeEgress) Ensure(_ context.Context, profile string, _ []string) (container.Attachment, error) {
	return container.Attachment{Network: container.NetworkName(profile), Env: []string{"HTTPS_PROXY=http://icaro-egress:3128"}}, nil
}

type fakeImages struct{}

func (fakeImages) Resolve(_ context.Context, ref string) (string, error) {
	return ref + "@sha256:abc", nil
}

type fakeMemory struct{ calls chan Identity }

func (f fakeMemory) ServeSession(_ context.Context, id Identity, socket string) (func(), error) {
	f.calls <- id
	return func() {}, os.WriteFile(socket, nil, 0o600)
}

type fakePreview struct{ added, removed chan string }

func (f fakePreview) WorktreeAdded(_ context.Context, sid, _, path string) (string, error) {
	f.added <- path
	return "pv-" + sid, nil
}

func (f fakePreview) WorktreeRemoved(_ context.Context, _, path string) error {
	f.removed <- path
	return nil
}

type env struct {
	m      *Manager
	st     *store.Store
	eng    *fakeEngine
	repo   string
	cfg    *config.Config
	cancel context.CancelFunc
	mem    fakeMemory
	pv     fakePreview
}

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

// newEnv starts a manager on a fresh store and repository; mods adjust
// its options before it is built.
func newEnv(t *testing.T, maxActive int, mods ...func(*Options)) *env {
	t.Helper()
	// An explicit empty config file and Home keep the test off the user's
	// ~/.icaro without t.Setenv, so tests can run in parallel.
	cfgFile := filepath.Join(t.TempDir(), "icaro.yaml")
	if err := os.WriteFile(cfgFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Home = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "My App")
	_ = os.MkdirAll(repo, 0o700)
	gitInit(t, repo)
	e := &env{st: st, eng: newFakeEngine(), repo: repo, cfg: cfg, cancel: cancel,
		mem: fakeMemory{calls: make(chan Identity, 10)}, pv: fakePreview{added: make(chan string, 10), removed: make(chan string, 10)}}
	o := Options{
		Store: st, Config: cfg, Engine: e.eng, Egress: fakeEgress{}, Images: fakeImages{},
		Homes: &creds.Homes{Layout: cfg.Layout(), Mode: creds.ModeCopy}, Memory: e.mem, Preview: e.pv,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxActive: maxActive, UID: 1234, GID: 1234, Tick: time.Hour,
	}
	for _, mod := range mods {
		mod(&o)
	}
	e.m = New(o)
	go func() { _ = e.m.Run(ctx) }()
	return e
}

func TestTwoSessionsOnTwoWorktrees(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	a, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "Fix Login"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.m.Create(ctx, CreateRequest{RepoPath: filepath.Join(e.repo, "."), Name: "refactor"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusRunning || b.Status != StatusRunning {
		t.Fatalf("%s %s %s", a.Status, b.Status, a.Error+b.Error)
	}
	if a.WorktreePath == b.WorktreePath || a.ContainerID == b.ContainerID || a.Branch != "icaro/fix-login" {
		t.Fatalf("%+v %+v", a, b)
	}
	if _, err := git(ctx, e.repo, "rev-parse", "--verify", "icaro/refactor"); err != nil {
		t.Fatal("branch not created")
	}
	spec := e.eng.spec(a.ContainerID)
	if spec.User != "1234:1234" || !contains(spec.Env, "ICARO_SESSION="+a.ID) || !contains(spec.Env, "HTTPS_PROXY=http://icaro-egress:3128") {
		t.Fatalf("%+v", spec)
	}
	var targets []string
	for _, mt := range spec.HostConfig.Mounts {
		targets = append(targets, mt.Target+"<-"+mt.Source)
	}
	joined := strings.Join(targets, " ")
	for _, want := range []string{"/workspace<-" + a.WorktreePath, "/home/icaro<-", "/icaro<-", filepath.Join(e.repo, ".git") + "/config<-"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing mount %q in %s", want, joined)
		}
	}
	if spec.Image != "ghcr.io/icaro-dev/agent-base:latest@sha256:abc" {
		t.Fatal("image not pinned by digest: " + spec.Image)
	}
	// Memory and preview facets were attached.
	id := <-e.mem.calls
	if id.SessionID == "" || id.Branch == "" || id.Agent != "claude" {
		t.Fatalf("%+v", id)
	}
	if p := <-e.pv.added; p != a.WorktreePath {
		t.Fatal(p)
	}
	got, _ := e.m.Get(ctx, a.ID)
	if got.PreviewInstanceID != "pv-"+a.ID {
		t.Fatal("preview id not recorded")
	}
	// The agent home carries the MCP entry and the preamble.
	cj, _, _ := e.m.o.Homes.Read("claude", a.ID, ".claude.json")
	if !strings.Contains(string(cj), "/icaro/mcp.sock") {
		t.Fatalf("%s", cj)
	}
	ex, err := e.m.Exec(ctx, a.ID)
	if err != nil || ex.ContainerID != a.ContainerID || ex.AgentCmd[0] != "claude" || ex.User != "1234:1234" {
		t.Fatalf("%+v %v", ex, err)
	}
	// Ensure reuses the running session.
	again, err := e.m.Ensure(ctx, CreateRequest{RepoPath: e.repo, Name: "refactor"})
	if err != nil || again.ID != b.ID {
		t.Fatalf("ensure: %+v %v", again, err)
	}
	inWT, err := e.m.Ensure(ctx, CreateRequest{RepoPath: a.WorktreePath})
	if err != nil || inWT.ID != a.ID {
		t.Fatalf("ensure from worktree: %+v %v", inWT, err)
	}
	list, _ := e.m.List(ctx, ListFilter{Project: "my-app"})
	if len(list) != 2 {
		t.Fatal(list)
	}
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "refactor"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Profile: "nope"}); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: t.TempDir()}); err == nil {
		t.Fatal("non-repo accepted")
	}

	// Remove deletes the worktree but keeps the branch with its commits.
	if err := e.m.Remove(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("worktree kept")
	}
	if _, err := git(ctx, e.repo, "rev-parse", "--verify", "icaro/fix-login"); err != nil {
		t.Fatal("branch removed")
	}
	if <-e.pv.removed != a.WorktreePath {
		t.Fatal("preview cleanup")
	}
	if _, err := e.m.Get(ctx, a.ID); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestConcurrencyCapQueues(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	a, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "a"})
	if err != nil || a.Status != StatusRunning {
		t.Fatal(a, err)
	}
	b, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "b"})
	if err != nil || b.Status != StatusQueued {
		t.Fatalf("over the cap the session must be queued: %+v %v", b, err)
	}
	if _, err := e.m.Stop(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := e.m.Get(ctx, b.ID)
		if got.Status == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued session not started: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Restarting a now stays queued behind b.
	re, err := e.m.Start(ctx, a.ID)
	if err != nil || re.Status != StatusQueued {
		t.Fatalf("%+v %v", re, err)
	}
}

func TestReconcileAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	alive, _ := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "alive"})
	dead, _ := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "dead"})
	e.eng.kill(dead.ContainerID)
	// A session caught mid-create by a crash.
	mid := &store.SessionRow{Kind: KindAgent, ProjectID: alive.ProjectID, Profile: "claude", Status: StatusCreating, WorktreePath: alive.WorktreePath}
	if err := e.st.CreateSession(ctx, mid); err != nil {
		t.Fatal(err)
	}
	e.cancel() // "daemon stops"

	m2 := New(Options{
		Store: e.st, Config: e.cfg, Engine: e.eng, Egress: fakeEgress{}, Images: fakeImages{},
		Homes: e.m.o.Homes, Memory: e.mem, Log: e.m.o.Log, MaxActive: 4, UID: 1234, GID: 1234,
	})
	if err := m2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{alive.ID: StatusRunning, dead.ID: StatusOrphaned, mid.ID: StatusOrphaned} {
		got, _ := m2.Get(ctx, id)
		if got.Status != want {
			t.Fatalf("%s: %s (%s), want %s", id, got.Status, got.Error, want)
		}
	}
	// Doctor's cleanup leaves the worktree and marks the session stopped.
	if err := CleanupOrphan(ctx, e.st, e.eng, dead.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m2.Get(ctx, dead.ID)
	if got.Status != StatusStopped || !strings.Contains(got.Error, "cleaned up") {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(dead.WorktreePath); err != nil {
		t.Fatal("cleanup must keep the user's worktree")
	}
}

func TestDisallowedMountFailsStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	p := e.cfg.Profiles["claude"]
	p.Mounts = []string{t.TempDir() + ":/data"}
	e.cfg.Profiles["claude"] = p
	s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusError || !strings.Contains(s.Error, "not under an allowed directory") {
		t.Fatalf("a directory outside the allowlist must not be mounted: %+v", s)
	}
}

func TestRunSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	ws, err := e.m.StartRun(ctx, "RUN1", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.m.StartRun(ctx, "RUN1", "")
	if err != nil || again != ws {
		t.Fatalf("StartRun must be idempotent: %+v %+v", ws, again)
	}
	if fi, err := os.Stat(ws.Dir); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.repo, "README"), []byte("v1"), 0o600)
	if _, err := git(ctx, e.repo, "add", "README"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, e.repo, "commit", "-q", "-m", "readme"); err != nil {
		t.Fatal(err)
	}
	p1, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "main")
	if err != nil || p1 != p2 {
		t.Fatalf("same ref+revision must resolve once per run: %s %s %v", p1, p2, err)
	}
	if b, _ := os.ReadFile(filepath.Join(p1, "README")); string(b) != "v1" {
		t.Fatal("worktree content")
	}
	head, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "")
	if err != nil || head == p1 {
		t.Fatalf("HEAD is a separate (ref, revision): %s %v", head, err)
	}
	if _, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "nope"); err == nil {
		t.Fatal("unknown revision")
	}
	if err := e.m.SetContainer(ctx, ws.SessionID, "c9"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.FinishRun(ctx, ws.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatal("run worktree kept")
	}
	got, _ := e.m.Get(ctx, ws.SessionID)
	if got.Status != StatusStopped || got.Kind != KindRun || got.RunID != "RUN1" {
		t.Fatalf("%+v", got)
	}
	if _, err := e.m.StartRun(ctx, "RUN2", "nope"); err == nil {
		t.Fatal("unknown profile")
	}
}

func TestCapacity(t *testing.T) {
	t.Parallel()
	if capFor(0) != 2 || capFor(2<<30) != 1 || capFor(16<<30) != 4 {
		t.Fatal("capFor")
	}
	if DefaultMaxActive() < 1 {
		t.Fatal("DefaultMaxActive")
	}
	if Slug("Hello, World!") != "hello-world" || repoKey("git@x:a/b.git") == repoKey("git@x:a/c.git") {
		t.Fatal("slug/repoKey")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Sessions start only while Docker answers, so creating one while it is
// down fails at once instead of waiting, and leaves nothing behind.
func TestCreateFailsFastWhileDockerIsDown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	e.eng.fail("ping", errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"))

	start := time.Now()
	for _, create := range []func(context.Context, CreateRequest) (*Session, error){e.m.Create, e.m.Ensure} {
		_, err := create(ctx, CreateRequest{RepoPath: e.repo, Name: "down"})
		var un *ErrUnavailable
		if !errors.As(err, &un) || !strings.Contains(un.Msg, "docker is not reachable") || !strings.Contains(un.Msg, "docker.sock") {
			t.Fatalf("want ErrUnavailable naming the Docker error, got %v", err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v; must not wait for Docker", d)
	}
	if rows, err := e.st.ListSessions(ctx, store.SessionFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("no session row may be left: %v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Layout().Worktrees(), "my-app", "down")); !os.IsNotExist(err) {
		t.Fatalf("no worktree may be left: %v", err)
	}
	// A bad request is still reported as such while Docker is down.
	var inv *ErrInvalid
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Profile: "nope"}); !errors.As(err, &inv) {
		t.Fatalf("unknown profile: %v", err)
	}

	// Once Docker answers, the same request succeeds.
	e.eng.fail("ping", nil)
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "down"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("after Docker is back: %+v %v", s, err)
	}
}
