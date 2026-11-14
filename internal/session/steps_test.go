package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/creds"
	"icaro/internal/inject"
	"icaro/internal/store"
)

func TestAgentStepSessions(t *testing.T) {
	t.Parallel()
	e := newInjectEnv(t, true)
	ctx := context.Background()
	if d, err := e.m.AgentImage(ctx, "claude"); err != nil || !strings.HasSuffix(d, "@sha256:abc") {
		t.Fatal(d, err)
	}
	if _, err := e.m.AgentImage(ctx, "ghost"); !isInvalid(err) {
		t.Fatal(err)
	}
	ws, err := e.m.StartRun(ctx, "run-1", "")
	if err != nil {
		t.Fatal(err)
	}
	req := AgentStepRequest{RunSessionID: ws.SessionID, RunID: "run-1", StepID: "review", Attempt: 1, Profile: "claude", Workspace: ws.Dir, Memory: true}
	a, err := e.m.StartAgentStep(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Agent != "claude" || a.Home.Dir == "" || a.Egress.Network == "" || a.MCPSocket == "" {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Stat(a.MCPSocket); err != nil {
		t.Fatal("memory socket", err)
	}
	if id := <-e.mem.calls; id.SessionID != a.SessionID || id.Agent != "claude" {
		t.Fatalf("memory identity %+v", id)
	}
	// The provider key never reaches the step: only a capability token
	// and the relay's base URL.
	env := strings.Join(a.Env, " ")
	token := envValue(a.Env, "ANTHROPIC_API_KEY")
	if strings.Contains(env, realAnthropicKey) || !strings.HasPrefix(token, inject.TokenPrefix+a.SessionID+".") || !strings.Contains(env, "ICARO_SESSION="+a.SessionID) {
		t.Fatalf("env %v", a.Env)
	}
	if id, err := e.m.Authorize(ctx, "claude", token); err != nil || id != a.SessionID {
		t.Fatalf("the step's token works while it runs: %v", err)
	}
	// The MCP entry is in the step's own layer, never the seed.
	if b, ok, _ := e.m.o.Homes.Read("claude", a.SessionID, ".claude.json"); !ok || !strings.Contains(string(b), "mcp.sock") {
		t.Fatalf("mcp config %s", b)
	}
	if _, err := os.Stat(filepath.Join(e.m.o.Homes.SeedDir("claude"), ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the seed was written")
	}

	// A second step starts from the seed: a file the first wrote is absent.
	if err := e.m.o.Homes.Write("claude", a.SessionID, "notes.txt", []byte("mine")); err != nil {
		t.Fatal(err)
	}
	req.StepID, req.Memory = "second", false
	b, err := e.m.StartAgentStep(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.m.o.Homes.Read("claude", b.SessionID, "notes.txt"); ok || b.MCPSocket != "" || b.Home.Dir == a.Home.Dir {
		t.Fatalf("steps share a home: %+v", b)
	}

	// After a restart, the step is resumed with its socket served again.
	r, err := e.m.ResumeAgentStep(ctx, a.SessionID)
	if err != nil || r.MCPSocket == "" || r.Home != a.Home {
		t.Fatalf("%+v %v", r, err)
	}
	<-e.mem.calls
	if r, err := e.m.ResumeAgentStep(ctx, b.SessionID); err != nil || r.MCPSocket != "" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := e.m.ResumeAgentStep(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := e.m.ResumeAgentStep(ctx, ws.SessionID); !isInvalid(err) {
		t.Fatal(err)
	}

	// Finishing revokes the token and discards the layer and the row.
	if err := e.m.FinishAgentStep(ctx, a.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authorize(ctx, "claude", token); err == nil {
		t.Fatal("a finished step's token still works")
	}
	if _, err := os.Stat(a.Home.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("home layer kept")
	}
	if _, err := e.st.GetSession(ctx, a.SessionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("row kept")
	}
	if err := e.m.FinishAgentStep(ctx, a.SessionID); err != nil {
		t.Fatal("finishing twice", err)
	}
	if err := e.m.FinishAgentStep(ctx, ws.SessionID); !isInvalid(err) {
		t.Fatal(err)
	}
	// The run's end finishes a step session the engine left behind.
	if err := e.m.FinishRun(ctx, ws.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetSession(ctx, b.SessionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("step session outlived its run")
	}
}

func TestAgentStepSessionErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	ws, err := e.m.StartRun(ctx, "run-2", "")
	if err != nil {
		t.Fatal(err)
	}
	base := AgentStepRequest{RunSessionID: ws.SessionID, RunID: "run-2", StepID: "s", Attempt: 1, Workspace: ws.Dir}
	for _, tc := range []struct {
		mod  func(*AgentStepRequest)
		want string
	}{
		{func(r *AgentStepRequest) { r.Profile = "ghost" }, `unknown profile "ghost"`},
		{func(r *AgentStepRequest) { r.Profile = "workflow" }, "cannot run workflow steps"},
		{func(r *AgentStepRequest) { r.Profile, r.RunSessionID = "claude", "nope" }, "run session"},
	} {
		req := base
		tc.mod(&req)
		if _, err := e.m.StartAgentStep(ctx, req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	// A profile that injects but whose proxy has no relay: the half-made
	// step session is finished, not left behind.
	e2 := newInjectEnv(t, false)
	ws2, _ := e2.m.StartRun(ctx, "run-3", "")
	if _, err := e2.m.StartAgentStep(ctx, AgentStepRequest{RunSessionID: ws2.SessionID, RunID: "run-3", Profile: "claude", Workspace: ws2.Dir}); err == nil || !strings.Contains(err.Error(), "no injection relay") {
		t.Fatal(err)
	}
	rows, _ := e2.st.ListSessions(ctx, store.SessionFilter{Kind: KindStep})
	if len(rows) != 0 {
		t.Fatalf("left %d step sessions", len(rows))
	}
}

// A running agent step holds its profile's seed: `icaro login` is refused.
func TestCheckLoginRefusesDuringAgentStep(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1, func(o *Options) { o.Homes = &creds.Homes{Layout: o.Config.Layout(), Mode: creds.ModeOverlay} })
	ctx := context.Background()
	r := &store.SessionRow{Kind: KindStep, Profile: "claude", RunID: "run-x", WorktreePath: t.TempDir(), Status: StatusRunning}
	if err := e.st.CreateSession(ctx, r); err != nil {
		t.Fatal(err)
	}
	var busy *ErrBusy
	if err := e.m.CheckLogin(ctx, "claude", false); !errors.As(err, &busy) || !strings.Contains(busy.Msg, r.ID+" (running)") {
		t.Fatal(err)
	}
}
