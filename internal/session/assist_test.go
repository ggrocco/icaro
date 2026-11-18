package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/store"
)

type fakeAssist struct{ calls chan Identity }

func (f fakeAssist) ServeAssist(_ context.Context, id Identity, socket string) (func(), error) {
	f.calls <- id
	return func() {}, os.WriteFile(socket, nil, 0o600)
}

// fakeExecer records execs and answers them with a script.
type fakeExecer struct {
	execs chan docker.ExecOptions
	run   func(o docker.ExecOptions, stdout, stderr io.Writer) (int, error)
}

func (f *fakeExecer) Exec(_ context.Context, _ string, o docker.ExecOptions, stdout, stderr io.Writer) (int, error) {
	f.execs <- o
	if f.run == nil {
		return 0, nil
	}
	return f.run(o, stdout, stderr)
}

func newAssistEnv(t *testing.T, maxAssist int) (*env, fakeAssist, *fakeExecer) {
	t.Helper()
	fa := fakeAssist{calls: make(chan Identity, 10)}
	fx := &fakeExecer{execs: make(chan docker.ExecOptions, 10)}
	e := newEnv(t, 8, func(o *Options) {
		o.Config.Assistant.MaxActive = maxAssist
		o.Assist, o.AssistPreamble, o.Execer = fa, "You are the Ícaro workflow assistant.", fx
	})
	return e, fa, fx
}

func TestCreateAssist(t *testing.T) {
	t.Parallel()
	e, fa, _ := newAssistEnv(t, 2)
	ctx := context.Background()
	s, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "chat1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindAssist || s.Status != StatusRunning || s.Profile != e.cfg.AssistantProfile() || s.ProjectID != "" || s.Branch != "" {
		t.Fatalf("%+v", s)
	}
	if want := filepath.Join(e.cfg.Layout().Assist(), "chat1", "workspace"); s.WorktreePath != want {
		t.Fatalf("workspace %s, want %s", s.WorktreePath, want)
	}
	if fi, err := os.Stat(s.WorktreePath); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	spec := e.eng.spec(s.ContainerID)
	if !contains(spec.Env, "ICARO_PROJECT=") || spec.Labels[container.LabelKind] != KindAssist {
		t.Fatalf("spec %+v", spec)
	}
	for _, mt := range spec.HostConfig.Mounts {
		if strings.HasSuffix(mt.Target, ".git") || strings.Contains(mt.Source, "/.git") {
			t.Fatalf("an assistant gets no git mounts: %+v", mt)
		}
	}
	if id := <-fa.calls; id.SessionID != s.ID || id.ProjectID != "" || id.Agent != "claude" {
		t.Fatalf("attacher identity %+v", id)
	}
	md, ok, err := e.m.o.Homes.Read(s.Profile, s.ID, ".claude/CLAUDE.md")
	if err != nil || !ok || !strings.Contains(string(md), "Ícaro workflow assistant") || strings.Contains(string(md), "memory_query") {
		t.Fatalf("preamble %q %v %v", md, ok, err)
	}

	// With a project, the identity carries it and the project name is in
	// the environment.
	proj, err := e.st.UpsertProject(ctx, &store.Project{ID: "p1", Name: "my-app", RootPath: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "chat2", ProjectID: proj.ID})
	if err != nil || p.Status != StatusRunning {
		t.Fatal(p, err)
	}
	if id := <-fa.calls; id.ProjectID != "p1" {
		t.Fatalf("%+v", id)
	}
	if !contains(e.eng.spec(p.ContainerID).Env, "ICARO_PROJECT=my-app") {
		t.Fatal(e.eng.spec(p.ContainerID).Env)
	}

	// Assist sessions do not count against sessions.max_active, and agent
	// sessions do not count against assistant.max_active.
	if n, _ := e.st.CountSessions(ctx, KindAgent, StatusRunning); n != 0 {
		t.Fatalf("agent sessions %d", n)
	}
}

func TestCreateAssistRefusals(t *testing.T) {
	t.Parallel()
	e, _, _ := newAssistEnv(t, 2)
	ctx := context.Background()
	var inv *ErrInvalid
	for _, req := range []AssistRequest{{}, {ChatID: "../x"}, {ChatID: "c", Profile: "ghost"}, {ChatID: "c", ProjectID: "nope"}} {
		if _, err := e.m.CreateAssist(ctx, req); !errors.As(err, &inv) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	e.cfg.Profiles["sh"] = config.ProfileConfig{Egress: []string{}}
	if _, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c", Profile: "sh"}); err == nil || !strings.Contains(err.Error(), "cannot run the assistant") {
		t.Errorf("shell profile: %v", err)
	}
	saved := e.cfg.Profiles
	e.cfg.Profiles = map[string]config.ProfileConfig{"sh": {Egress: []string{}}}
	if _, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c"}); err == nil || !strings.Contains(err.Error(), "set assistant.profile") {
		t.Errorf("no chat profile: %v", err)
	}
	e.cfg.Profiles = saved
	e.eng.fail("ping", errors.New("down"))
	var un *ErrUnavailable
	if _, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c"}); !errors.As(err, &un) {
		t.Errorf("docker down: %v", err)
	}
	e.eng.fail("ping", nil)
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, nil, 0o600)
	e.cfg.Home = blocked // the scratch directory cannot be made
	e.m.layout = e.cfg.Layout()
	if _, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c"}); err == nil {
		t.Error("unwritable home")
	}
}

func TestAssistCapQueues(t *testing.T) {
	t.Parallel()
	e, _, _ := newAssistEnv(t, 1)
	ctx := context.Background()
	a, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "a"})
	if err != nil || a.Status != StatusRunning {
		t.Fatal(a, err)
	}
	b, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "b"})
	if err != nil || b.Status != StatusQueued {
		t.Fatalf("over the cap: %+v %v", b, err)
	}
	if e.m.AssistMaxActive() != 1 {
		t.Fatal(e.m.AssistMaxActive())
	}
	// An agent session still starts: the caps are separate.
	if s := e.create(t, "agent"); s.Status != StatusRunning {
		t.Fatalf("agent %+v", s)
	}
	if _, err := e.m.Stop(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, b.ID, StatusRunning)

	// Start brings a stopped assist session back (queued at the cap).
	if s, err := e.m.Start(ctx, a.ID); err != nil || s.Status != StatusQueued {
		t.Fatalf("restart at the cap: %+v %v", s, err)
	}
}

func TestRunAndCancelTurn(t *testing.T) {
	t.Parallel()
	e, _, fx := newAssistEnv(t, 2)
	ctx := context.Background()
	s, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	fx.run = func(o docker.ExecOptions, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, `{"type":"result"}`+"\n")
		_, _ = io.WriteString(stderr, strings.Repeat("x", stderrTail)+"warn: last line\n")
		return 3, nil
	}
	var out strings.Builder
	res, err := e.m.RunTurn(ctx, s.ID, TurnRequest{Cmd: []string{"claude", "-p"}, Env: []string{"A=b"}, Stdin: []byte("hi")}, &out)
	if err != nil || res.ExitCode != 3 || out.String() != `{"type":"result"}`+"\n" || !strings.HasSuffix(res.Stderr, "warn: last line") || len(res.Stderr) > stderrTail {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	o := <-fx.execs
	if strings.Join(o.Cmd[:2], " ") != "sh -c" || !strings.Contains(o.Cmd[2], turnPIDFile) || strings.Join(o.Cmd[3:], " ") != "icaro-turn claude -p" ||
		o.User != "1234:1234" || o.Workdir != "/workspace" || string(o.Stdin) != "hi" || !contains(o.Env, "A=b") {
		t.Fatalf("exec %+v", o)
	}

	if err := e.m.CancelTurn(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if o := <-fx.execs; !strings.Contains(strings.Join(o.Cmd, " "), "kill -TERM") {
		t.Fatalf("cancel %+v", o)
	}

	// Refusals.
	var inv *ErrInvalid
	if _, err := e.m.RunTurn(ctx, s.ID, TurnRequest{}, io.Discard); !errors.As(err, &inv) {
		t.Errorf("empty command: %v", err)
	}
	if _, err := e.m.RunTurn(ctx, "nope", TurnRequest{Cmd: []string{"x"}}, io.Discard); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	agent := e.create(t, "agent")
	if _, err := e.m.RunTurn(ctx, agent.ID, TurnRequest{Cmd: []string{"x"}}, io.Discard); !errors.As(err, &inv) {
		t.Errorf("agent session: %v", err)
	}
	if _, err := e.m.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.m.CancelTurn(ctx, s.ID); !errors.As(err, &inv) {
		t.Errorf("stopped: %v", err)
	}
	e.m.o.Execer = nil
	if _, err := e.m.RunTurn(ctx, s.ID, TurnRequest{Cmd: []string{"x"}}, io.Discard); !errors.Is(err, ErrNoExec) {
		t.Errorf("no execer: %v", err)
	}
}

func TestRunTurnStoreError(t *testing.T) {
	t.Parallel()
	e, _, _ := newAssistEnv(t, 2)
	_ = e.st.Close()
	if _, err := e.m.RunTurn(context.Background(), "x", TurnRequest{Cmd: []string{"x"}}, io.Discard); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("closed store: %v", err)
	}
}

func TestRemoveAssist(t *testing.T) {
	t.Parallel()
	e, _, _ := newAssistEnv(t, 2)
	ctx := context.Background()
	s, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Remove(ctx, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(s.WorktreePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch directory kept: %v", err)
	}
	if _, err := e.m.Get(ctx, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	select {
	case p := <-e.pv.removed:
		t.Fatalf("preview hook called for %s", p)
	default:
	}
	// A row whose workspace is outside ~/.icaro/assist never deletes it.
	outside := t.TempDir()
	row := &store.SessionRow{Kind: KindAssist, Profile: "claude", Status: StatusStopped, WorktreePath: filepath.Join(outside, "workspace")}
	_ = os.MkdirAll(row.WorktreePath, 0o700)
	if err := e.st.CreateSession(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Remove(ctx, row.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(row.WorktreePath); err != nil {
		t.Fatalf("outside workspace removed: %v", err)
	}
}

func TestReconcileAndProbeAssist(t *testing.T) {
	t.Parallel()
	e, fa, _ := newAssistEnv(t, 4)
	ctx := context.Background()
	live, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "dead"})
	if err != nil {
		t.Fatal(err)
	}
	<-fa.calls
	<-fa.calls
	// A restart re-serves the live session's socket.
	if err := e.idle().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for range 2 {
		served[(<-fa.calls).SessionID] = true
	}
	if !served[live.ID] || !served[dead.ID] {
		t.Fatalf("re-served %v", served)
	}
	// The probe orphans an assist session whose container exited, and
	// stops nothing it cannot find.
	_ = e.eng.StopContainer(ctx, dead.ContainerID, 0)
	e.m.probe(ctx)
	if got := e.waitStatus(t, dead.ID, StatusOrphaned); !strings.Contains(got.Error, "exited") {
		t.Fatal(got.Error)
	}
	if got, _ := e.m.Get(ctx, live.ID); got.Status != StatusRunning {
		t.Fatalf("%+v", got)
	}

	// A start the daemon never finished is orphaned on reconcile.
	mid := &store.SessionRow{Kind: KindAssist, Profile: "claude", Status: StatusCreating, WorktreePath: t.TempDir()}
	cid, _ := e.eng.CreateContainerOn(ctx, docker.Spec{Name: "mid"}, docker.NetSpec{})
	_ = e.eng.StartContainer(ctx, cid)
	mid.ContainerID = cid
	if err := e.st.CreateSession(ctx, mid); err != nil {
		t.Fatal(err)
	}
	if err := e.idle().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Get(ctx, mid.ID); got.Status != StatusOrphaned {
		t.Fatalf("%+v", got)
	}
	if st := e.state(cid); st.Running {
		t.Fatal("orphaned assistant container still running")
	}
}

func TestAssistWithoutAttacher(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 8, func(o *Options) { o.Execer = &fakeExecer{execs: make(chan docker.ExecOptions, 1)} })
	ctx := context.Background()
	s, err := e.m.CreateAssist(ctx, AssistRequest{ChatID: "c"})
	if err != nil || s.Status != StatusRunning {
		t.Fatal(s, err)
	}
	// No attacher: no MCP entry is configured, and the memory attacher
	// is never asked to serve an assist session.
	raw, _, _ := e.m.o.Homes.Read(s.Profile, s.ID, ".claude.json")
	if strings.Contains(string(raw), "mcpServers") {
		t.Fatalf("mcp configured without an attacher: %s", raw)
	}
	select {
	case id := <-e.mem.calls:
		t.Fatalf("memory served an assist session: %+v", id)
	default:
	}
	e.m.SetAssist(fakeAssist{calls: make(chan Identity, 1)}, "p")
	if e.m.o.AssistPreamble != "p" {
		t.Fatal("SetAssist")
	}
}

func TestAssistAttacherError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 8, func(o *Options) { o.Assist = failAssist{} })
	s, err := e.m.CreateAssist(context.Background(), AssistRequest{ChatID: "c"})
	if err != nil || s.Status != StatusRunning {
		t.Fatalf("an attacher failure must not fail the session: %+v %v", s, err)
	}
}

type failAssist struct{}

func (failAssist) ServeAssist(context.Context, Identity, string) (func(), error) {
	return nil, errors.New("no socket")
}

func TestTail(t *testing.T) {
	var tl tail
	_, _ = tl.Write([]byte("  a  "))
	if tl.String() != "a" {
		t.Fatal(tl.String())
	}
}
