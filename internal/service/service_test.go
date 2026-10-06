package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/crypto"
	"icaro/internal/store"
)

type fakeRunner struct{ woke, cancelled int }

func (f *fakeRunner) Wake()         { f.woke++ }
func (f *fakeRunner) Cancel(string) { f.cancelled++ }

func newService(t *testing.T) (*Service, *fakeRunner) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	fr := &fakeRunner{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Options{Store: st, Key: key, Runner: fr, LogsDir: t.TempDir(), Log: quiet}), fr
}

const hello = `
name: hello
inputs:
  who: { type: string, default: world }
  must: { type: string, required: true }
on:
  webhook: {}
steps:
  - name: say
    run: { image: alpine, script: "echo {{ inputs.who }}" }
    connection: api
`

func TestWorkflowLifecycle(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.ApplyWorkflow(ctx, hello, "alice")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Path != "/steps/0/connection" {
		t.Fatalf("expected unknown connection issue, got %v", err)
	}
	if _, err := svc.UpsertConnection(ctx, "api", "bearer", map[string]string{"token": "abc123xyz"}); err != nil {
		t.Fatal(err)
	}
	info, err := svc.ApplyWorkflow(ctx, hello, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 1 || info.WebhookToken == "" || len(info.Steps) != 1 || info.YAML == "" {
		t.Fatalf("info: %+v", info)
	}
	info2, _ := svc.ApplyWorkflow(ctx, hello, "bob")
	if info2.Version != 2 || info2.WebhookToken != info.WebhookToken {
		t.Fatalf("second apply: %+v", info2)
	}
	vs, _ := svc.ListWorkflowVersions(ctx, "hello")
	if len(vs) != 2 || vs[0].Actor != "bob" {
		t.Fatalf("versions: %+v", vs)
	}
	v1, _ := svc.GetWorkflowVersion(ctx, "hello", 1)
	if v1.YAML != hello {
		t.Fatal("version yaml")
	}
	list, _ := svc.ListWorkflows(ctx)
	if len(list) != 1 || list[0].YAML != "" {
		t.Fatalf("list: %+v", list)
	}

	// Validation without saving reports typos with hints.
	_, issues, _ := svc.ValidateWorkflow(ctx, strings.Replace(hello, "image:", "imgae:", 1))
	hinted := false
	for _, is := range issues {
		if is.Path == "/steps/0/run/imgae" && strings.Contains(is.Hint, `"image"`) {
			hinted = true
		}
	}
	if !hinted {
		t.Fatalf("issues: %v", issues)
	}

	if err := svc.SetWorkflowStatus(ctx, "hello", store.WorkflowDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueRun(ctx, "hello", map[string]any{"must": "x"}, EnqueueOptions{}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled enqueue: %v", err)
	}
	_ = svc.SetWorkflowStatus(ctx, "hello", store.WorkflowActive)
	if err := svc.DeleteWorkflow(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRunsAndInputs(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	_, _ = svc.UpsertConnection(ctx, "api", "bearer", map[string]string{"token": "abc123xyz"})
	if _, err := svc.ApplyWorkflow(ctx, hello, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueRun(ctx, "hello", nil, EnqueueOptions{}); err == nil || !strings.Contains(err.Error(), `missing required input "must"`) {
		t.Fatalf("required input: %v", err)
	}
	run, err := svc.EnqueueRun(ctx, "hello", map[string]any{"must": "x"}, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunQueued || string(run.Input) != `{"must":"x","who":"world"}` || len(run.Steps) != 1 || run.Steps[0].Name != "say" || fr.woke != 1 {
		t.Fatalf("run: %+v woke=%d", run, fr.woke)
	}
	if string(run.Steps[0].Outputs) != "{}" {
		t.Fatalf("outputs default: %s", run.Steps[0].Outputs)
	}
	list, _ := svc.ListRuns(ctx, "hello", "", 10)
	if len(list) != 1 || list[0].ID != run.ID {
		t.Fatalf("list: %+v", list)
	}
	cancelled, err := svc.CancelRun(ctx, run.ID)
	if err != nil || cancelled.Status != store.RunCancelled {
		t.Fatalf("cancel: %v %+v", err, cancelled)
	}
	if _, err := svc.GetRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if b, err := svc.StepLogs(ctx, run.ID, 0, 0); err != nil || b != nil {
		t.Fatalf("logs of never-run step: %v %q", err, b)
	}

	// Child runs inherit lineage and are depth-capped.
	parent, _ := svc.Store().GetRun(ctx, run.ID)
	child, err := svc.EnqueueRun(ctx, "hello", map[string]any{"must": "y"}, EnqueueOptions{Parent: parent})
	if err != nil || child.ParentRunID != run.ID || child.RootRunID != run.ID {
		t.Fatalf("child: %v %+v", err, child)
	}
	parent.Depth = 5
	if _, err := svc.EnqueueRun(ctx, "hello", map[string]any{"must": "y"}, EnqueueOptions{Parent: parent}); err == nil {
		t.Fatal("depth cap")
	}
}

func TestConnectionsAndAuth(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.UpsertConnection(ctx, "bad name", "generic", nil); err == nil {
		t.Fatal("name validation")
	}
	if _, err := svc.UpsertConnection(ctx, "x", "bearer", map[string]string{}); err == nil {
		t.Fatal("required field")
	}
	if _, err := svc.UpsertConnection(ctx, "x", "nope", nil); err == nil {
		t.Fatal("unknown type")
	}
	info, err := svc.UpsertConnection(ctx, "slack", "generic", map[string]string{"bot_token": "xoxb-1", "team": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "generic" || strings.Join(info.FieldNames, ",") != "bot_token,team" {
		t.Fatalf("info: %+v", info)
	}
	fields, err := svc.ResolveConnection(ctx, "slack")
	if err != nil || fields["bot_token"] != "xoxb-1" {
		t.Fatalf("resolve: %v %v", err, fields)
	}
	raw, _ := svc.Store().GetConnection(ctx, "slack")
	if strings.Contains(string(raw.FieldsCT), "xoxb-1") {
		t.Fatal("fields stored in clear")
	}
	if err := svc.DeleteConnection(ctx, "slack"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveConnection(ctx, "slack"); err == nil {
		t.Fatal("resolved deleted connection")
	}

	plain, hash, _ := crypto.NewToken()
	if _, err := svc.Store().CreateToken(ctx, "t", hash, store.ScopeWrite); err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Authenticate(ctx, plain)
	if err != nil || tok.Scope != store.ScopeWrite {
		t.Fatalf("auth: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "icaro_wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if !ScopeAllows(store.ScopeAdmin, store.ScopeRead) || ScopeAllows(store.ScopeRead, store.ScopeWrite) || ScopeAllows("", store.ScopeRead) {
		t.Fatal("scopes")
	}
}
