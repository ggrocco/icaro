package authoring

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/crypto"
	"icaro/internal/mcp"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	client *mcp.Client
	sec    *secrets.Store
}

func newEnv(t *testing.T, scope string) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	sec := secrets.New(st, key)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.New(ctx)
	fake := docker.NewFake()
	fake.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Stdout: "hello from " + c.Spec.Image + "\n", Output: []byte(`{"greeting":"hi"}`)}
	}
	eng := engine.New(engine.Config{
		Concurrency: 2, StepTimeout: time.Minute, LogsDir: t.TempDir(), RunsDir: t.TempDir(),
		RetryBase: time.Millisecond, PollInterval: 10 * time.Millisecond, Heartbeat: 20 * time.Millisecond,
	}, engine.Deps{Store: st, Docker: fake, Events: bus, Secrets: sec, Log: quiet})
	go func() { _ = eng.Run(ctx) }()
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet, Events: bus})
	svc.SetRunner(eng)
	svc.SetSecrets(sec)

	srv := mcp.NewServer("icaro", "test", "")
	Register(srv, Deps{Workflows: svc, Secrets: sec})
	c := mcp.InProcess(srv, mcp.Caller{Principal: "agent-test", Scope: scope})
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}, nil); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, client: c, sec: sec}
}

// call invokes a tool and returns its text and isError.
func (e *env) call(name string, args map[string]any) (string, bool) {
	e.t.Helper()
	var res mcp.ToolResult
	if err := e.client.Call(e.ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) == 0 {
		e.t.Fatalf("%s: empty result", name)
	}
	return res.Content[0].Text, res.IsError
}

func (e *env) callJSON(name string, args map[string]any, out any) {
	e.t.Helper()
	text, isErr := e.call(name, args)
	if isErr {
		e.t.Fatalf("%s failed: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		e.t.Fatalf("%s: decode %q: %v", name, text, err)
	}
}

// The Phase 4 acceptance flow: schema → validate (fix a typo from the
// issue path) → create → run → get_run, with no destructive tools.
func TestAgentAuthoringLoop(t *testing.T) {
	e := newEnv(t, "write")

	var tools struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				DestructiveHint *bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := e.client.Call(e.ctx, "tools/list", nil, &tools); err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if strings.Contains(tl.Name, "delete") || strings.Contains(tl.Name, "remove") || tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint {
			t.Fatalf("tool %s is destructive or unannotated", tl.Name)
		}
	}

	schemaText, _ := e.call(ToolGetSchema, map[string]any{})
	if !strings.Contains(schemaText, `"$schema"`) || !strings.Contains(schemaText, "steps") {
		t.Fatalf("schema: %.200s", schemaText)
	}

	// A deliberate typo: `comand`.
	draft := "version: 1\nname: agent-hello\nsteps:\n  - id: greet\n    image: alpine:3.24\n    comand: echo hi\n    outputs:\n      greeting: json:greeting\n"
	var v struct {
		Valid  bool `json:"valid"`
		Issues []struct {
			Path, Message, Hint string
		} `json:"issues"`
	}
	e.callJSON(ToolValidate, map[string]any{"yaml": draft}, &v)
	if v.Valid || len(v.Issues) == 0 {
		t.Fatalf("typo not reported: %+v", v)
	}
	is := v.Issues[0]
	if is.Path != "/steps/0/comand" || !strings.Contains(is.Hint, "command") {
		t.Fatalf("issue should point at the typo with a hint: %+v", is)
	}
	// The agent fixes exactly what the path points at.
	fixed := strings.Replace(draft, "comand:", "command:", 1)
	e.callJSON(ToolValidate, map[string]any{"yaml": fixed}, &v)
	if !v.Valid {
		t.Fatalf("still invalid: %+v", v.Issues)
	}

	// create refuses a document with issues and an existing name.
	if text, isErr := e.call(ToolCreate, map[string]any{"yaml": draft}); !isErr || !strings.Contains(text, "issues") {
		t.Fatalf("create accepted an invalid workflow: %s", text)
	}
	var created struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	e.callJSON(ToolCreate, map[string]any{"yaml": fixed}, &created)
	if created.Name != "agent-hello" || created.Version != 1 {
		t.Fatalf("created %+v", created)
	}
	if _, isErr := e.call(ToolCreate, map[string]any{"yaml": fixed}); !isErr {
		t.Fatal("create must refuse an existing workflow")
	}
	e.callJSON(ToolUpdate, map[string]any{"yaml": fixed + "description: v2\n"}, &created)
	if created.Version != 2 {
		t.Fatalf("update version = %d", created.Version)
	}

	var list struct {
		Workflows []map[string]any `json:"workflows"`
	}
	e.callJSON(ToolList, map[string]any{}, &list)
	if len(list.Workflows) != 1 {
		t.Fatalf("list %+v", list)
	}

	var run struct {
		RunID string `json:"run_id"`
	}
	e.callJSON(ToolRun, map[string]any{"name": "agent-hello"}, &run)
	if run.RunID == "" {
		t.Fatal("no run id")
	}

	var got struct {
		Status string `json:"status"`
		Steps  []struct {
			ID      string         `json:"id"`
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
			LogTail string         `json:"log_tail"`
		} `json:"steps"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.callJSON(ToolGetRun, map[string]any{"id": run.RunID, "log_tail_bytes": 1024}, &got)
		if service.Terminal(got.Status) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != store.RunSucceeded || len(got.Steps) != 1 {
		t.Fatalf("run: %+v", got)
	}
	if got.Steps[0].Outputs["greeting"] != "hi" || !strings.Contains(got.Steps[0].LogTail, "hello from alpine") {
		t.Fatalf("step: %+v", got.Steps[0])
	}
}

func TestSecretsNeverReturnedAndReadOnlyScope(t *testing.T) {
	e := newEnv(t, "read")
	if err := e.sec.Set(e.ctx, "github.token", "ghp_supersecretvalue", []string{"api.github.com"}); err != nil {
		t.Fatal(err)
	}
	text, isErr := e.call(ToolListSecrets, map[string]any{})
	if isErr || strings.Contains(text, "ghp_supersecretvalue") || !strings.Contains(text, "github.token") || !strings.Contains(text, "api.github.com") {
		t.Fatalf("list_secrets: %s", text)
	}
	yaml := "version: 1\nname: ro-test\nsteps:\n  - id: a\n    image: alpine:3.24\n    command: echo hi\n"
	if text, isErr := e.call(ToolCreate, map[string]any{"yaml": yaml}); !isErr || !strings.Contains(text, "read-only") {
		t.Fatalf("read scope created a workflow: %s", text)
	}
	if text, isErr := e.call(ToolRun, map[string]any{"name": "x"}); !isErr || !strings.Contains(text, "read-only") {
		t.Fatalf("read scope ran: %s", text)
	}
	if text, isErr := e.call(ToolGetIntegration, map[string]any{"name": "github"}); !isErr || !strings.Contains(text, "no integrations") {
		t.Fatalf("get_integration without hub: %s", text)
	}
	if text, isErr := e.call(ToolGetRun, map[string]any{"id": "nope"}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("get_run unknown: %s", text)
	}
	if _, isErr := e.call(ToolGet, map[string]any{"name": "nope"}); !isErr {
		t.Fatal("get unknown workflow")
	}
	var ints struct {
		Integrations []any `json:"integrations"`
	}
	e.callJSON(ToolListIntegration, map[string]any{}, &ints)
	if ints.Integrations == nil {
		t.Fatal("list_integrations must return an empty list, not null")
	}
	var res struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := e.client.Call(e.ctx, "resources/read", map[string]any{"uri": "icaro://schemas/workflow"}, &res); err != nil || len(res.Contents) == 0 || !strings.Contains(res.Contents[0].Text, "steps") {
		t.Fatalf("schema resource: %v %+v", err, res)
	}
}
