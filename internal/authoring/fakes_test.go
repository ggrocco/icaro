package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"icaro/internal/integration"
	"icaro/internal/mcp"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/workflow"
)

// fakeWorkflows is a scripted service layer for the error paths the real
// service rarely produces.
type fakeWorkflows struct {
	validateErr error
	issues      workflow.Issues
	spec        *workflow.Spec
	existing    map[string]*service.WorkflowInfo
	getErr      error
	applyErr    error
	applyActor  string
	listErr     error
	startErr    error
	trigger     map[string]any
	run         *service.RunInfo
	runErr      error
	logs        map[string]*service.LogPage
	logTails    []int64
}

func (f *fakeWorkflows) ValidateWorkflow(context.Context, string) (*workflow.Spec, workflow.Issues, error) {
	return f.spec, f.issues, f.validateErr
}

func (f *fakeWorkflows) ApplyWorkflow(_ context.Context, _, actor string) (*service.WorkflowInfo, error) {
	f.applyActor = actor
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &service.WorkflowInfo{Name: f.spec.Name, Version: 1}, nil
}

func (f *fakeWorkflows) GetWorkflow(_ context.Context, name string) (*service.WorkflowInfo, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if w, ok := f.existing[name]; ok {
		return w, nil
	}
	return nil, service.ErrNotFound
}

func (f *fakeWorkflows) ListWorkflows(context.Context) ([]*service.WorkflowInfo, error) {
	return nil, f.listErr
}

func (f *fakeWorkflows) StartRun(_ context.Context, _ string, trigger map[string]any, _ string) (*service.RunInfo, error) {
	f.trigger = trigger
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &service.RunInfo{ID: "r1", Status: "queued"}, nil
}

func (f *fakeWorkflows) GetRun(context.Context, string) (*service.RunInfo, error) {
	return f.run, f.runErr
}

func (f *fakeWorkflows) StepLogs(_ context.Context, _, stepID string, _ int, _, tail int64) (*service.LogPage, error) {
	f.logTails = append(f.logTails, tail)
	if p, ok := f.logs[stepID]; ok {
		return p, nil
	}
	return nil, errors.New("log unreadable")
}

type fakeHub struct{}

func (fakeHub) Integrations() []integration.IntegrationInfo {
	return []integration.IntegrationInfo{
		{IntegrationSummary: integration.IntegrationSummary{Name: "github", Events: []string{"push"}}},
		{IntegrationSummary: integration.IntegrationSummary{Name: "slack"}},
	}
}

func (h fakeHub) Integration(name string) (*integration.IntegrationInfo, error) {
	for _, i := range h.Integrations() {
		if i.Name == name {
			i.Pack = "builtin"
			return &i, nil
		}
	}
	return nil, errors.New("not found")
}

func (fakeHub) ActionInputSchemas() map[string]map[string]any {
	return map[string]map[string]any{"slack.post": {"type": "object"}}
}

type failingSecrets struct{}

func (failingSecrets) List(context.Context) ([]secrets.Info, error) {
	return nil, errors.New("store offline")
}

// fakeEnv wires the tools over fakes; principal "" exercises the default.
func fakeEnv(t *testing.T, w *fakeWorkflows, d Deps) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.Workflows = w
	srv := mcp.NewServer("icaro", "test", "")
	Register(srv, d)
	c := mcp.InProcess(srv, mcp.Caller{Scope: "write"})
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}, nil); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, client: c}
}

func wantErr(t *testing.T, e *env, tool string, args map[string]any, want string) {
	t.Helper()
	text, isErr := e.call(tool, args)
	if !isErr || !strings.Contains(text, want) {
		t.Fatalf("%s(%v): want error containing %q, got %v %s", tool, args, want, isErr, text)
	}
}

func TestIntegrationTools(t *testing.T) {
	e := fakeEnv(t, &fakeWorkflows{}, Deps{Integrations: fakeHub{}, Secrets: failingSecrets{}})

	var list struct {
		Integrations []integration.IntegrationSummary `json:"integrations"`
	}
	e.callJSON(ToolListIntegration, map[string]any{}, &list)
	if len(list.Integrations) != 2 || list.Integrations[0].Name != "github" {
		t.Fatalf("list: %+v", list)
	}
	var one map[string]any
	e.callJSON(ToolGetIntegration, map[string]any{"name": "github"}, &one)
	if one["pack"] != "builtin" || one["name"] != "github" {
		t.Fatalf("get: %+v", one)
	}
	wantErr(t, e, ToolGetIntegration, map[string]any{"name": "jira"}, "installed: github, slack")
	wantErr(t, e, ToolGetIntegration, map[string]any{"nme": "x"}, "unknown field")
	wantErr(t, e, ToolListSecrets, map[string]any{}, "store offline")

	var res struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := e.client.Call(e.ctx, "resources/read", map[string]any{"uri": "icaro://schemas/actions"}, &res); err != nil || len(res.Contents) == 0 || !strings.Contains(res.Contents[0].Text, "slack.post") {
		t.Fatalf("actions resource: %v %+v", err, res)
	}
}

func TestActionsResourceWithoutHub(t *testing.T) {
	e := fakeEnv(t, &fakeWorkflows{}, Deps{})
	var res struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := e.client.Call(e.ctx, "resources/read", map[string]any{"uri": "icaro://schemas/actions"}, &res); err != nil || len(res.Contents) == 0 || strings.TrimSpace(res.Contents[0].Text) != "{}" {
		t.Fatalf("actions resource: %v %+v", err, res)
	}
	var secs struct {
		Secrets []any `json:"secrets"`
	}
	e.callJSON(ToolListSecrets, map[string]any{}, &secs)
	if secs.Secrets == nil || len(secs.Secrets) != 0 {
		t.Fatalf("no secret store lists nothing: %+v", secs)
	}
}

func TestWorkflowToolErrors(t *testing.T) {
	boom := errors.New("db down")
	spec := &workflow.Spec{Name: "w"}

	t.Run("validate", func(t *testing.T) {
		w := &fakeWorkflows{spec: spec, issues: workflow.Issues{{Path: "/steps/1"}, {Path: "/name"}}}
		e := fakeEnv(t, w, Deps{})
		var v struct {
			Valid  bool             `json:"valid"`
			Name   string           `json:"name"`
			Issues []workflow.Issue `json:"issues"`
		}
		e.callJSON(ToolValidate, map[string]any{"yaml": "x"}, &v)
		if v.Valid || v.Name != "w" || len(v.Issues) != 2 || v.Issues[0].Path != "/name" {
			t.Fatalf("issues must be sorted by path: %+v", v)
		}
		w.validateErr = boom
		wantErr(t, e, ToolValidate, map[string]any{"yaml": "x"}, "db down")
		wantErr(t, e, ToolValidate, map[string]any{"yml": "x"}, "unknown field")
		wantErr(t, e, ToolCreate, map[string]any{"yaml": "x"}, "db down")
		wantErr(t, e, ToolCreate, map[string]any{"yml": "x"}, "unknown field")
	})

	t.Run("apply", func(t *testing.T) {
		w := &fakeWorkflows{spec: spec}
		e := fakeEnv(t, w, Deps{})
		wantErr(t, e, ToolUpdate, map[string]any{"yaml": "x"}, `"w" does not exist; use create_workflow`)
		var created map[string]any
		e.callJSON(ToolCreate, map[string]any{"yaml": "x"}, &created)
		if w.applyActor != "mcp:agent" {
			t.Fatalf("a caller without a principal is recorded as agent: %q", w.applyActor)
		}
		w.applyErr = boom
		wantErr(t, e, ToolCreate, map[string]any{"yaml": "x"}, "db down")
	})

	t.Run("get and list", func(t *testing.T) {
		w := &fakeWorkflows{existing: map[string]*service.WorkflowInfo{"w": {Name: "w", Version: 3, YAML: "name: w\n"}}}
		e := fakeEnv(t, w, Deps{})
		var got map[string]any
		e.callJSON(ToolGet, map[string]any{"name": "w"}, &got)
		if got["yaml"] != "name: w\n" || got["version"] != float64(3) {
			t.Fatalf("get: %+v", got)
		}
		wantErr(t, e, ToolGet, map[string]any{"name": "nope"}, `workflow "nope" not found`)
		wantErr(t, e, ToolGet, map[string]any{"id": "w"}, "unknown field")
		w.getErr = boom
		wantErr(t, e, ToolGet, map[string]any{"name": "w"}, "db down")
		w.listErr = boom
		wantErr(t, e, ToolList, map[string]any{}, "db down")
	})

	t.Run("run", func(t *testing.T) {
		w := &fakeWorkflows{}
		e := fakeEnv(t, w, Deps{})
		var run map[string]any
		e.callJSON(ToolRun, map[string]any{"name": "w"}, &run)
		if run["run_id"] != "r1" || w.trigger == nil {
			t.Fatalf("a missing trigger is sent as {}: %+v %v", run, w.trigger)
		}
		wantErr(t, e, ToolRun, map[string]any{"nme": "w"}, "unknown field")
		w.startErr = service.ErrNotFound
		wantErr(t, e, ToolRun, map[string]any{"name": "w"}, `workflow "w" not found`)
		w.startErr = boom
		wantErr(t, e, ToolRun, map[string]any{"name": "w"}, "db down")
	})
}

func TestGetRunView(t *testing.T) {
	exit := 2
	w := &fakeWorkflows{
		run: &service.RunInfo{ID: "r1", Workflow: "w", Version: 1, Status: "failed", Error: "step b failed", Steps: []service.StepInfo{
			{ID: "a", Status: "succeeded", Outputs: json.RawMessage(`{"x":1}`), LogSize: 10},
			{ID: "b", Status: "failed", ExitCode: &exit, Error: "exit 2", LogSize: 99999},
			{ID: "c", Status: "skipped"},
			{ID: "d", Status: "failed", LogSize: 5},
			{ID: "e", Status: "waiting", Approval: &service.ApprovalInfo{Message: "ship?"}},
		}},
		logs: map[string]*service.LogPage{
			"a": {Data: []byte("all of a")},
			"b": {Data: []byte("end of b"), Offset: 99000},
		},
	}
	e := fakeEnv(t, w, Deps{})

	var got struct {
		Error string           `json:"error"`
		Steps []map[string]any `json:"steps"`
	}
	e.callJSON(ToolGetRun, map[string]any{"id": "r1", "log_tail_bytes": 1 << 20}, &got)
	if got.Error != "step b failed" || len(got.Steps) != 5 {
		t.Fatalf("run: %+v", got)
	}
	a, b, c, d := got.Steps[0], got.Steps[1], got.Steps[2], got.Steps[3]
	if a["log_tail"] != "all of a" || a["log_truncated"] != false {
		t.Fatalf("a: %+v", a)
	}
	if b["log_tail"] != "end of b" || b["log_truncated"] != true || b["exit_code"] != float64(2) || b["error"] != "exit 2" {
		t.Fatalf("b: %+v", b)
	}
	if _, ok := c["log_tail"]; ok {
		t.Fatalf("a step without a log has no tail: %+v", c)
	}
	if _, ok := d["log_tail"]; ok {
		t.Fatalf("an unreadable log is left out, not an error: %+v", d)
	}
	if ap, _ := got.Steps[4]["approval"].(map[string]any); ap["message"] != "ship?" || ap["decide_with"] != "icaro runs approve r1 e" {
		t.Fatalf("e: %+v", got.Steps[4])
	}
	for _, tail := range w.logTails {
		if tail != maxTail {
			t.Fatalf("tail must be capped at %d: %v", maxTail, w.logTails)
		}
	}

	// Only the named step's log, and the default tail.
	w.logTails = nil
	got.Steps = nil
	e.callJSON(ToolGetRun, map[string]any{"id": "r1", "step": "a"}, &got)
	if len(w.logTails) != 1 || w.logTails[0] != defaultTail {
		t.Fatalf("step filter / default tail: %v", w.logTails)
	}
	if _, ok := got.Steps[1]["log_tail"]; ok {
		t.Fatal("other steps' logs must be omitted")
	}
	// A zero tail fetches no logs at all.
	w.logTails = nil
	e.callJSON(ToolGetRun, map[string]any{"id": "r1", "log_tail_bytes": 0}, &got)
	if len(w.logTails) != 0 {
		t.Fatalf("zero tail: %v", w.logTails)
	}

	wantErr(t, e, ToolGetRun, map[string]any{"run": "r1"}, "unknown field")
	w.runErr = errors.New("db down")
	wantErr(t, e, ToolGetRun, map[string]any{"id": "r1"}, "db down")
}
