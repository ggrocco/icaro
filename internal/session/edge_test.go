package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/store"
)

// These tests cover the decisions and failure paths of the manager. Store
// failures are injected with SQLite triggers (one statement raises), a
// renamed table or column (one kind of query fails) or a closed pool.

var errBoom = errors.New("boom")

type stubMemory struct{ err error }

func (s stubMemory) ServeSession(_ context.Context, _ Identity, socket string) (func(), error) {
	if s.err != nil {
		return nil, s.err
	}
	return func() {}, os.WriteFile(socket, nil, 0o600)
}

type stubPreview struct{ addErr, removeErr error }

func (s stubPreview) WorktreeAdded(context.Context, string, string, string) (string, error) {
	return "", s.addErr
}

func (s stubPreview) WorktreeRemoved(context.Context, string, string) error { return s.removeErr }

type errImages struct{}

func (errImages) Resolve(context.Context, string) (string, error) { return "", errBoom }

type errEgress struct{}

func (errEgress) Ensure(context.Context, string, []string) (container.Attachment, error) {
	return container.Attachment{}, errBoom
}

type fakeVolumes struct{ removeErr error }

func (fakeVolumes) CreateVolumeOpts(context.Context, string, string, map[string]string, map[string]string) error {
	return nil
}

func (f fakeVolumes) RemoveVolume(context.Context, string) error { return f.removeErr }

// newQuietEnv is newEnv with memory and preview facets that never block.
func newQuietEnv(t *testing.T, maxActive int, mods ...func(*Options)) *env {
	t.Helper()
	return newEnv(t, maxActive, append([]func(*Options){func(o *Options) {
		o.Memory, o.Preview = stubMemory{}, stubPreview{}
	}}, mods...)...)
}

func (e *env) sql(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.st.DB().Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// failWhen makes op on table raise while cond (over NEW/OLD) holds.
func (e *env) failWhen(t *testing.T, name, op, table, cond string) {
	t.Helper()
	when := ""
	if cond != "" {
		when = " WHEN " + cond
	}
	e.sql(t, "CREATE TRIGGER "+name+" BEFORE "+op+" ON "+table+when+" BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
}

// idle returns a manager on e's store and config whose Run loop is not
// started, so tests can drive fill and requestStart themselves.
func (e *env) idle() *Manager { return New(e.m.o) }

func (e *env) create(t *testing.T, name string) *Session {
	t.Helper()
	s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusRunning {
		t.Fatalf("%s: %s %s", name, s.Status, s.Error)
	}
	return s
}

func (e *env) waitStatus(t *testing.T, id, want string) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := e.m.Get(context.Background(), id)
		if err == nil && s.Status == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s: %+v %v, want %s", id, s, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isInvalid(err error) bool {
	var inv *ErrInvalid
	return errors.As(err, &inv) && inv.Error() != ""
}

func TestManagerDefaults(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	m := New(Options{Config: e.cfg})
	if m.o.Log == nil || m.o.Tick != 30*time.Second || m.Name() != "sessions" {
		t.Fatalf("%+v", m.o)
	}
	want := m.User()
	if os.Getuid() == 0 {
		if want != "1000:1000" {
			t.Fatalf("a root daemon runs containers as 1000:1000, got %s", want)
		}
	} else if want != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) {
		t.Fatalf("containers run as the daemon's user, got %s", want)
	}
	if u := New(Options{Config: e.cfg, GID: 5}).User(); u != "1000:1000" {
		t.Fatalf("uid 0 is never used for containers: %s", u)
	}
	if m.MaxActive() != DefaultMaxActive() {
		t.Fatal("derived cap")
	}
	e.cfg.Sessions.MaxActive = 3
	if m.MaxActive() != 3 {
		t.Fatal("configured cap")
	}
	m.SetPreviewHook(stubPreview{})
	m.SetMemoryAttacher(stubMemory{})
	if m.o.Preview == nil || m.o.Memory == nil {
		t.Fatal("setters")
	}
	// Nudges coalesce: a second one never blocks.
	m.nudge()
	m.nudge()
	if short("0123456789abcdef") != "0123456789ab" || short("c1") != "c1" || toSession(nil) != nil {
		t.Fatal("helpers")
	}
	// Replacing a registered MCP server stops the old one.
	stopped := make(chan struct{})
	m.mcp.put("s", func() { close(stopped) })
	m.mcp.put("s", func() {})
	<-stopped
}

func TestCreateVariants(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 8)
	ctx := context.Background()

	// An existing branch is checked out instead of created.
	if _, err := git(ctx, e.repo, "branch", "icaro/existing"); err != nil {
		t.Fatal(err)
	}
	if s := e.create(t, "existing"); s.Branch != "icaro/existing" {
		t.Fatal(s.Branch)
	}
	based, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "based", Base: "main"})
	if err != nil || based.Status != StatusRunning {
		t.Fatalf("%+v %v", based, err)
	}
	// An unknown base fails the worktree, and the session records why.
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "bad-base", Base: "nope"}); err == nil {
		t.Fatal("unknown base accepted")
	}
	failed, _ := e.m.List(ctx, ListFilter{Status: StatusError})
	if len(failed) != 1 || failed[0].Branch != "icaro/bad-base" || failed[0].Error == "" {
		t.Fatalf("%+v", failed)
	}
	// Without a usable name the branch comes from the session id.
	anon, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "!!!"})
	if err != nil || !strings.HasPrefix(anon.Branch, "icaro/") || len(anon.Branch) != len("icaro/")+6 {
		t.Fatalf("%+v %v", anon, err)
	}
	if long := Slug(strings.Repeat("ab-", 30)); len(long) > 40 || strings.HasSuffix(long, "-") {
		t.Fatalf("slugs are capped at 40 characters without a trailing dash: %q", long)
	}

	// Two repositories with the same directory name get distinct projects.
	other := filepath.Join(t.TempDir(), "My App")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	gitInit(t, other)
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: other, Name: "x"}); err != nil {
		t.Fatal(err)
	}
	// A directory name without slug characters still names a project.
	odd := filepath.Join(t.TempDir(), "___")
	if err := os.MkdirAll(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	gitInit(t, odd)
	if _, err := e.m.Create(ctx, CreateRequest{RepoPath: odd, Name: "x"}); err != nil {
		t.Fatal(err)
	}
	projects, err := e.m.Projects(ctx)
	if err != nil || len(projects) != 3 {
		t.Fatalf("%+v %v", projects, err)
	}
	names := map[string]bool{}
	for _, p := range projects {
		names[p.Name] = true
	}
	if !names["my-app"] || !names["project"] || len(names) != 3 {
		t.Fatalf("project names: %v", names)
	}

	// Ensure creates when nothing matches and rejects a non-repository.
	ensured, err := e.m.Ensure(ctx, CreateRequest{RepoPath: e.repo, Name: "fresh"})
	if err != nil || ensured.Branch != "icaro/fresh" {
		t.Fatalf("%+v %v", ensured, err)
	}
	if _, err := e.m.Ensure(ctx, CreateRequest{RepoPath: t.TempDir()}); !isInvalid(err) {
		t.Fatal(err)
	}
}

func TestFacetFailuresDoNotFailTheSession(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4, func(o *Options) {
		o.Memory = stubMemory{err: errBoom}
		o.Preview = stubPreview{addErr: errBoom, removeErr: errBoom}
	})
	s := e.create(t, "facets")
	if s.PreviewInstanceID != "" {
		t.Fatal(s.PreviewInstanceID)
	}
	if err := e.m.Remove(context.Background(), s.ID, false); err != nil {
		t.Fatalf("a preview cleanup failure is only logged: %v", err)
	}
}

func TestStartAgentFailures(t *testing.T) {
	t.Parallel()
	profile := func(e *env, f func(*config.ProfileConfig)) {
		p := e.cfg.Profiles["claude"]
		f(&p)
		e.cfg.Profiles["claude"] = p
	}
	for _, tc := range []struct {
		name  string
		mods  []func(*Options)
		setup func(t *testing.T, e *env)
		want  string
	}{
		{name: "unknown agent", setup: func(_ *testing.T, e *env) { profile(e, func(p *config.ProfileConfig) { p.Agent = "nope" }) }, want: "unknown agent"},
		{name: "image", mods: []func(*Options){func(o *Options) { o.Images = errImages{} }}, want: "image "},
		{name: "egress", mods: []func(*Options){func(o *Options) { o.Egress = errEgress{} }}, want: "boom"},
		{name: "mount syntax", setup: func(_ *testing.T, e *env) { profile(e, func(p *config.ProfileConfig) { p.Mounts = []string{"bad"} }) }, want: "host:container"},
		{name: "create", setup: func(_ *testing.T, e *env) { e.eng.fail("create", errBoom) }, want: "create container"},
		{name: "start", setup: func(_ *testing.T, e *env) { e.eng.fail("start", errBoom) }, want: "start container"},
		{name: "icaro dir", setup: func(t *testing.T, e *env) {
			blockWith(t, filepath.Join(e.m.layout.RunDir(), "sessions"))
		}, want: "not a directory"},
		{name: "home", setup: func(t *testing.T, e *env) { blockWith(t, e.m.layout.Home("claude")) }, want: "not a directory"},
		{name: "agent config", setup: func(t *testing.T, e *env) {
			seed, err := e.m.o.Homes.EnsureSeed("claude")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(seed, ".claude.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "configure claude"},
		{name: "persist container", setup: func(t *testing.T, e *env) {
			e.failWhen(t, "persist", "UPDATE", "sessions", "NEW.status = 'creating' AND NEW.container_id <> ''")
		}, want: "injected failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newQuietEnv(t, 4, tc.mods...)
			if tc.setup != nil {
				tc.setup(t, e)
			}
			s, err := e.m.Create(context.Background(), CreateRequest{RepoPath: e.repo, Name: "s"})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusError || !strings.Contains(s.Error, tc.want) {
				t.Fatalf("want an error session mentioning %q, got %s: %s", tc.want, s.Status, s.Error)
			}
		})
	}
}

func blockWith(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedMount(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	data := t.TempDir()
	e.m.policy.Allow = []string{data}
	p := e.cfg.Profiles["claude"]
	p.Mounts = []string{data + ":/data:ro"}
	e.cfg.Profiles["claude"] = p
	s := e.create(t, "mounted")
	var found bool
	for _, mt := range e.eng.spec(s.ContainerID).HostConfig.Mounts {
		found = found || (mt.Target == "/data" && mt.ReadOnly)
	}
	if !found {
		t.Fatal("an allowed mount must reach the container read-only")
	}
}

func TestOverlayFallback(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("overlay homes are Linux only")
	}
	e := newQuietEnv(t, 4, func(o *Options) {
		o.Homes = &creds.Homes{Layout: o.Config.Layout(), Mode: creds.ModeAuto, Volumes: fakeVolumes{}}
	})
	if !e.m.o.Homes.OverlayEnabled() {
		t.Fatal("auto mode with volumes starts on overlay")
	}
	e.eng.fail("create-once", errors.New("error mounting overlay"))
	s := e.create(t, "ov")
	if e.m.o.Homes.OverlayEnabled() {
		t.Fatal("a refused overlay switches to per-session copies")
	}
	if src := homeSource(e.eng.spec(s.ContainerID)); !strings.Contains(src, filepath.Join("sessions", s.ID, "home")) {
		t.Fatalf("home mount %q", src)
	}
}

func homeSource(spec docker.Spec) string {
	for _, mt := range spec.HostConfig.Mounts {
		if mt.Target == "/home/icaro" {
			return mt.Source
		}
	}
	return ""
}

func TestLifecycleEdgeCases(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 8)
	ctx := context.Background()

	for name, err := range map[string]error{
		"get":    func() error { _, err := e.m.Get(ctx, "nope"); return err }(),
		"stop":   func() error { _, err := e.m.Stop(ctx, "nope"); return err }(),
		"start":  func() error { _, err := e.m.Start(ctx, "nope"); return err }(),
		"exec":   func() error { _, err := e.m.Exec(ctx, "nope"); return err }(),
		"remove": e.m.Remove(ctx, "nope", false),
		"finish": e.m.FinishRun(ctx, "nope", false),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s of an unknown session: %v", name, err)
		}
	}
	if err := e.m.SetContainer(ctx, "nope", "c"); err == nil {
		t.Error("set container of an unknown session")
	}
	if err := e.m.Cleanup(ctx, "nope"); err == nil {
		t.Error("cleanup of an unknown session")
	}
	if _, err := e.m.List(ctx, ListFilter{Project: "nope"}); !isInvalid(err) {
		t.Errorf("unknown project filter: %v", err)
	}

	a := e.create(t, "a")
	// Starting a running session is a no-op; exec needs a running one.
	if s, err := e.m.Start(ctx, a.ID); err != nil || s.Status != StatusRunning || s.ContainerID != a.ContainerID {
		t.Fatalf("%+v %v", s, err)
	}
	// Engine failures surface from Stop and Remove.
	e.eng.fail("stop", errBoom)
	if _, err := e.m.Stop(ctx, a.ID); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	if err := e.m.Remove(ctx, a.ID, false); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.eng.fail("stop", nil)
	e.eng.fail("remove", errBoom)
	if _, err := e.m.Stop(ctx, a.ID); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.eng.fail("remove", nil)
	if _, err := e.m.Stop(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Exec(ctx, a.ID); !isInvalid(err) {
		t.Fatalf("exec into a stopped session: %v", err)
	}
	if list, _ := e.m.List(ctx, ListFilter{Status: StatusStopped}); len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("%+v", list)
	}

	// A stopped session whose worktree is gone cannot start again.
	b := e.create(t, "b")
	if _, err := e.m.Stop(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(b.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Start(ctx, b.ID); !isInvalid(err) {
		t.Fatalf("start without a worktree: %v", err)
	}

	// An orphaned session keeps its dead container id; Start drops it.
	c := e.create(t, "c")
	e.eng.kill(c.ContainerID)
	e.m.probe(ctx)
	orphan := e.waitStatus(t, c.ID, StatusOrphaned)
	if orphan.ContainerID != c.ContainerID {
		t.Fatal("the orphan keeps its container id for cleanup")
	}
	restarted, err := e.m.Start(ctx, c.ID)
	if err != nil || restarted.Status != StatusRunning || restarted.ContainerID == c.ContainerID {
		t.Fatalf("%+v %v", restarted, err)
	}
	// Cleanup surfaces engine errors.
	e.eng.fail("remove", errBoom)
	if err := e.m.Cleanup(ctx, c.ID); !errors.Is(err, errBoom) {
		t.Fatal(err)
	}
	e.eng.fail("remove", nil)
	if err := e.m.Cleanup(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	// Remove keeps the worktree on request.
	d := e.create(t, "d")
	if err := e.m.Remove(ctx, d.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.WorktreePath); err != nil {
		t.Fatal("keepWorktree removed the worktree")
	}

	// Run sessions are not started by hand, and Remove finishes them.
	ws, err := e.m.StartRun(ctx, "RUN1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Start(ctx, ws.SessionID); !isInvalid(err) {
		t.Fatalf("start a run session: %v", err)
	}
	if err := e.m.FinishRun(ctx, a.ID, false); err == nil {
		t.Fatal("finishing an agent session as a run")
	}
	if err := e.m.Remove(ctx, ws.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Get(ctx, ws.SessionID); got.Status != StatusStopped {
		t.Fatalf("%+v", got)
	}
}

func TestRemoveWithoutProject(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	s := e.create(t, "lost")
	e.sql(t, "DELETE FROM projects")
	if err := e.m.Remove(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("without its project the worktree directory is still removed")
	}
}

func TestRemoveFailures(t *testing.T) {
	t.Parallel()
	t.Run("worktree", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		s := e.create(t, "w")
		e.sql(t, "UPDATE projects SET root_path = ?", t.TempDir())
		if err := e.m.Remove(context.Background(), s.ID, false); err == nil {
			t.Fatal("a worktree that cannot be pruned must fail the removal")
		}
		if _, err := e.m.Get(context.Background(), s.ID); err != nil {
			t.Fatal("a failed removal keeps the session")
		}
	})
	t.Run("home", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4, func(o *Options) {
			o.Homes = &creds.Homes{Layout: o.Config.Layout(), Mode: creds.ModeCopy, Volumes: fakeVolumes{removeErr: errBoom}}
		})
		s := e.create(t, "h")
		if err := e.m.Remove(context.Background(), s.ID, false); !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
	})
}

func TestStoreWriteFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("create row", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		e.failWhen(t, "ins", "INSERT", "sessions", "")
		if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "x"}); err == nil {
			t.Fatal("create")
		}
		if _, err := e.m.StartRun(ctx, "R", ""); err == nil {
			t.Fatal("start run")
		}
	})
	t.Run("queue", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		s := e.create(t, "q")
		if _, err := e.m.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		e.failWhen(t, "queue", "UPDATE", "sessions", "NEW.status = 'queued'")
		if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "y"}); err == nil {
			t.Fatal("create")
		}
		if _, err := e.m.Start(ctx, s.ID); err == nil {
			t.Fatal("start")
		}
	})
	t.Run("stop", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		s := e.create(t, "s")
		e.failWhen(t, "stop", "UPDATE", "sessions", "NEW.status = 'stopped'")
		if _, err := e.m.Stop(ctx, s.ID); err == nil {
			t.Fatal("stop")
		}
	})
	t.Run("orphan", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		s := e.create(t, "o")
		e.eng.kill(s.ContainerID)
		e.failWhen(t, "orphan", "UPDATE", "sessions", "NEW.status = 'orphaned'")
		if err := e.m.Reconcile(ctx); err == nil {
			t.Fatal("reconcile must report a session it could not mark")
		}
	})
	t.Run("claim", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		row := &store.SessionRow{Kind: KindAgent, Profile: "claude", Status: StatusQueued, WorktreePath: t.TempDir()}
		if err := e.st.CreateSession(ctx, row); err != nil {
			t.Fatal(err)
		}
		e.failWhen(t, "claim", "UPDATE", "sessions", "NEW.status = 'creating'")
		e.idle().fill(ctx)
		if got, _ := e.m.Get(ctx, row.ID); got.Status != StatusQueued {
			t.Fatalf("a session that cannot be claimed stays queued: %+v", got)
		}
	})
	t.Run("project", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		e.sql(t, "ALTER TABLE projects RENAME TO hidden_projects")
		if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "x"}); err == nil {
			t.Fatal("create without a projects table")
		}
	})
}

func TestFillFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("missing project", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		row := &store.SessionRow{Kind: KindAgent, ProjectID: "ghost", Profile: "claude", Status: StatusQueued, WorktreePath: t.TempDir()}
		if err := e.st.CreateSession(ctx, row); err != nil {
			t.Fatal(err)
		}
		e.idle().fill(ctx)
		if got := e.waitStatus(t, row.ID, StatusError); !strings.Contains(got.Error, "project") {
			t.Fatal(got.Error)
		}
	})
	t.Run("unreadable queue", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		if _, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "p"}); err != nil {
			t.Fatal(err)
		}
		// Session rows can no longer be read; the project still can.
		e.sql(t, "ALTER TABLE sessions DROP COLUMN last_heartbeat")
		e.idle().fill(ctx)
		if _, err := e.m.Ensure(ctx, CreateRequest{RepoPath: e.repo, Name: "other"}); err == nil {
			t.Fatal("ensure must report an unreadable session list")
		}
	})
	t.Run("run loop", func(t *testing.T) {
		t.Parallel()
		e := newQuietEnv(t, 4)
		e.sql(t, "ALTER TABLE sessions RENAME TO hidden_sessions")
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if err := e.idle().Run(ctx); err != nil {
			t.Fatalf("store errors are logged; Run ends with its context: %v", err)
		}
	})
}

func TestRequestStartAndWaitHonourContext(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	m := e.idle()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.requestStart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("nobody runs the loop: %v", err)
	}
	// Create and Start give up when the loop never answers.
	tctx, tcancel := context.WithTimeout(context.Background(), time.Second)
	defer tcancel()
	if _, err := m.Create(tctx, CreateRequest{RepoPath: e.repo, Name: "w"}); err == nil {
		t.Fatal("create must give up with its context")
	}
	s := e.create(t, "x")
	if _, err := e.m.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer scancel()
	if _, err := m.Start(sctx, s.ID); err == nil {
		t.Fatal("start must give up with its context")
	}

	row := &store.SessionRow{Kind: KindAgent, Profile: "claude", Status: StatusCreating}
	if err := e.st.CreateSession(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer wcancel()
	// Waiting stops with the context and returns the row as is (or the
	// store's context error when the deadline lands mid-query).
	got, err := m.waitSettled(wctx, row.ID, time.Minute)
	switch {
	case err == nil:
		if got.Status != StatusCreating {
			t.Fatalf("%+v", got)
		}
	case !errors.Is(err, context.DeadlineExceeded):
		t.Fatal(err)
	}
}

func TestReconcileLiveness(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 8)
	ctx := context.Background()
	wt := t.TempDir()
	live := func(started bool) string {
		id, err := e.eng.CreateContainerOn(ctx, docker.Spec{Name: "x"}, docker.NetSpec{})
		if err != nil {
			t.Fatal(err)
		}
		if started {
			_ = e.eng.StartContainer(ctx, id)
		}
		return id
	}
	cases := []struct {
		row        store.SessionRow
		status, in string
	}{
		{store.SessionRow{Kind: KindAgent, Status: StatusCreating, WorktreePath: wt, ContainerID: live(true)}, StatusOrphaned, "being created"},
		{store.SessionRow{Kind: KindAgent, Status: StatusRunning, WorktreePath: filepath.Join(wt, "gone")}, StatusOrphaned, "is gone"},
		{store.SessionRow{Kind: KindAgent, Status: StatusRunning, WorktreePath: wt}, StatusOrphaned, "no container recorded"},
		{store.SessionRow{Kind: KindAgent, Status: StatusRunning, WorktreePath: wt, ContainerID: live(false)}, StatusOrphaned, "exited (code 0)"},
		{store.SessionRow{Kind: KindRun, Status: StatusRunning, WorktreePath: wt}, StatusRunning, ""},
		{store.SessionRow{Kind: KindAgent, Status: StatusRunning, WorktreePath: wt, ContainerID: live(true), Profile: "claude"}, StatusRunning, ""},
	}
	for i := range cases {
		if cases[i].row.Profile == "" {
			cases[i].row.Profile = "claude"
		}
		if err := e.st.CreateSession(ctx, &cases[i].row); err != nil {
			t.Fatal(err)
		}
	}
	// Reconciling twice re-serves memory for the live session, replacing
	// the first server.
	for range 2 {
		if err := e.m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range cases {
		got, _ := e.m.Get(ctx, tc.row.ID)
		if got.Status != tc.status || !strings.Contains(got.Error, tc.in) {
			t.Errorf("%+v: got %s %q, want %s %q", tc.row, got.Status, got.Error, tc.status, tc.in)
		}
	}

	e.eng.fail("inspect", errBoom)
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	last, _ := e.m.Get(ctx, cases[len(cases)-1].row.ID)
	if last.Status != StatusOrphaned || !strings.Contains(last.Error, "inspect container") {
		t.Fatalf("%+v", last)
	}
}

func TestProbeTicks(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4, func(o *Options) { o.Tick = 20 * time.Millisecond })
	s := e.create(t, "tick")
	alive := e.create(t, "alive")
	e.eng.kill(s.ContainerID)
	if got := e.waitStatus(t, s.ID, StatusOrphaned); !strings.Contains(got.Error, "is gone") {
		t.Fatal(got.Error)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := e.m.Get(context.Background(), alive.ID)
		if got.Status != StatusRunning {
			t.Fatalf("%+v", got)
		}
		if got.LastHeartbeat != nil && alive.LastHeartbeat != nil && got.LastHeartbeat.After(*alive.LastHeartbeat) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a live session is heartbeated by the probe")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClosedStore(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	s := e.create(t, "c")
	ws, err := e.m.StartRun(ctx, "R", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = e.st.Close()
	e.m.probe(ctx) // logs nothing, changes nothing
	for name, err := range map[string]error{
		"get":       func() error { _, err := e.m.Get(ctx, s.ID); return err }(),
		"list":      func() error { _, err := e.m.List(ctx, ListFilter{}); return err }(),
		"list proj": func() error { _, err := e.m.List(ctx, ListFilter{Project: "p"}); return err }(),
		"stop":      func() error { _, err := e.m.Stop(ctx, s.ID); return err }(),
		"start":     func() error { _, err := e.m.Start(ctx, s.ID); return err }(),
		"exec":      func() error { _, err := e.m.Exec(ctx, s.ID); return err }(),
		"remove":    e.m.Remove(ctx, s.ID, false),
		"reconcile": e.m.Reconcile(ctx),
		"finish":    e.m.FinishRun(ctx, ws.SessionID, false),
		"run":       func() error { _, err := e.m.StartRun(ctx, "R", ""); return err }(),
		"repo":      func() error { _, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, ""); return err }(),
	} {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want a store error, got %v", name, err)
		}
	}
}

func TestRunSessionRestart(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	ws, err := e.m.StartRun(ctx, "R1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.FinishRun(ctx, ws.SessionID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatal("keep leaves the workspace")
	}
	again, err := e.m.StartRun(ctx, "R1", "")
	if err != nil || again != ws {
		t.Fatalf("%+v %v", again, err)
	}
	if got, _ := e.m.Get(ctx, ws.SessionID); got.Status != StatusRunning {
		t.Fatalf("a restarted run session runs again: %+v", got)
	}

	if err := e.m.FinishRun(ctx, ws.SessionID, true); err != nil {
		t.Fatal(err)
	}
	e.failWhen(t, "rerun", "UPDATE", "sessions", "NEW.status = 'running'")
	if _, err := e.m.StartRun(ctx, "R1", ""); err == nil {
		t.Fatal("a run session that cannot be marked running")
	}
	e.sql(t, "DROP TRIGGER rerun")
	blocker := filepath.Join(t.TempDir(), "file")
	blockWith(t, blocker)
	e.sql(t, "UPDATE sessions SET worktree_path = ? WHERE run_id = 'R1'", filepath.Join(blocker, "ws"))
	if _, err := e.m.StartRun(ctx, "R1", ""); err == nil {
		t.Fatal("a workspace that cannot be created")
	}

	e2 := newQuietEnv(t, 4)
	blockWith(t, e2.m.layout.Runs())
	if _, err := e2.m.StartRun(ctx, "R2", ""); err == nil {
		t.Fatal("runs directory blocked")
	}
}

func commitFile(t *testing.T, repo, name, content string) {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", name}} {
		if _, err := git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepoWorktreeFailures(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 4)
	ctx := context.Background()
	commitFile(t, e.repo, "README", "v1")
	ws, err := e.m.StartRun(ctx, "R1", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.m.RepoWorktree(ctx, ws.SessionID, "", ""); !isInvalid(err) {
		t.Fatalf("empty ref: %v", err)
	}
	if _, err := e.m.RepoWorktree(ctx, "nope", e.repo, ""); err == nil {
		t.Fatal("unknown session")
	}
	if _, err := e.m.RepoWorktree(ctx, ws.SessionID, filepath.Join(t.TempDir(), "missing.git"), ""); err == nil {
		t.Fatal("a repository that cannot be cloned")
	}
	// The repository cache is shared: a second run fetches it once.
	if _, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "main"); err != nil {
		t.Fatal(err)
	}
	commitFile(t, e.repo, "README", "v2")
	ws2, _ := e.m.StartRun(ctx, "R2", "")
	p, err := e.m.RepoWorktree(ctx, ws2.SessionID, e.repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(p, "README")); string(b) != "v2" {
		t.Fatalf("a new run sees the fetched revision: %q", b)
	}

	// The cache lock honours the context.
	e.m.gitLock <- struct{}{}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.m.RepoWorktree(cctx, ws.SessionID, e.repo, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	<-e.m.gitLock

	// A forgotten but still registered worktree cannot be added again.
	e.sql(t, "DELETE FROM session_repos WHERE session_id = ?", ws.SessionID)
	if _, err := e.m.RepoWorktree(ctx, ws.SessionID, e.repo, "main"); err == nil {
		t.Fatal("re-adding a registered worktree path")
	}

	t.Run("record", func(t *testing.T) {
		e.failWhen(t, "rec", "INSERT", "session_repos", "")
		defer e.sql(t, "DROP TRIGGER rec")
		if _, err := e.m.RepoWorktree(ctx, ws2.SessionID, e.repo, "HEAD~1"); err == nil {
			t.Fatal("record the worktree")
		}
	})
	t.Run("touch", func(t *testing.T) {
		e.failWhen(t, "touch", "INSERT", "repositories", "")
		defer e.sql(t, "DROP TRIGGER touch")
		if _, err := e.m.RepoWorktree(ctx, ws2.SessionID, e.repo, "HEAD~1"); err == nil {
			t.Fatal("touch the cache entry")
		}
	})
	t.Run("target", func(t *testing.T) {
		ws3, _ := e.m.StartRun(ctx, "R3", "")
		blockWith(t, filepath.Join(filepath.Dir(ws3.Dir), "repos"))
		if _, err := e.m.RepoWorktree(ctx, ws3.SessionID, e.repo, ""); err == nil {
			t.Fatal("worktree parent blocked")
		}
	})
	t.Run("lookup", func(t *testing.T) {
		e.sql(t, "ALTER TABLE session_repos RENAME TO hidden_session_repos")
		defer e.sql(t, "ALTER TABLE hidden_session_repos RENAME TO session_repos")
		if _, err := e.m.RepoWorktree(ctx, ws2.SessionID, e.repo, "main"); err == nil {
			t.Fatal("lookup")
		}
		if err := e.m.FinishRun(ctx, ws2.SessionID, false); err == nil {
			t.Fatal("finish without the worktree list")
		}
	})

	// A worktree whose cache vanished is logged, not fatal, on finish.
	if err := os.RemoveAll(filepath.Join(e.m.layout.Repos(), repoKey(e.repo)+".git")); err != nil {
		t.Fatal(err)
	}
	if err := e.m.FinishRun(ctx, ws2.SessionID, false); err != nil {
		t.Fatal(err)
	}

	// A cache path that is not a directory cannot be fetched.
	e2 := newQuietEnv(t, 4)
	blockWith(t, e2.m.layout.Repos())
	ws4, _ := e2.m.StartRun(ctx, "R4", "")
	if _, err := e2.m.RepoWorktree(ctx, ws4.SessionID, e2.repo, ""); err == nil {
		t.Fatal("cache blocked")
	}
}

func TestGitHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bare := t.TempDir()
	if _, err := git(ctx, bare, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRepo(ctx, bare); err == nil || !strings.Contains(err.Error(), "bare") {
		t.Fatalf("a bare repository has no checkout to branch from: %v", err)
	}

	repo := t.TempDir()
	gitInit(t, repo)
	if _, err := git(ctx, repo, "checkout", "-q", "--detach"); err != nil {
		t.Fatal(err)
	}
	info, err := InspectRepo(ctx, repo)
	if err != nil || len(info.Head) != 40 {
		t.Fatalf("a detached HEAD is reported as its commit: %+v %v", info, err)
	}

	blocker := filepath.Join(t.TempDir(), "file")
	blockWith(t, blocker)
	if err := addWorktree(ctx, repo, filepath.Join(blocker, "wt"), "b", ""); err == nil {
		t.Fatal("worktree under a file")
	}
	// A directory that is not a registered worktree is removed anyway.
	stray := filepath.Join(t.TempDir(), "stray")
	blockWith(t, filepath.Join(stray, "f"))
	if err := removeWorktree(ctx, repo, stray); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("stray directory kept")
	}
	if k := repoKey("!!!"); !strings.HasPrefix(k, "repo-") {
		t.Fatal(k)
	}
}
