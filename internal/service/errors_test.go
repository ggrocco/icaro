package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

// The tests in this file reach the error paths of the use-cases by
// breaking the SQLite store underneath them: a trigger that raises on one
// statement, a renamed table, a corrupted spec or a closed pool.

func execSQL(t *testing.T, svc *Service, q string) {
	t.Helper()
	if _, err := svc.store.DB().Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// failOn installs a trigger that aborts every op on table.
func failOn(t *testing.T, svc *Service, op, table string) {
	t.Helper()
	execSQL(t, svc, "CREATE TRIGGER fail_"+strings.ToLower(op)+"_"+table+" BEFORE "+op+" ON "+table+
		" BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
}

// hide renames table so every query on it fails, and restores it after the test.
func hide(t *testing.T, svc *Service, table string) {
	t.Helper()
	execSQL(t, svc, "ALTER TABLE "+table+" RENAME TO hidden_"+table)
	t.Cleanup(func() { _, _ = svc.store.DB().Exec("ALTER TABLE hidden_" + table + " RENAME TO " + table) })
}

func corruptSpec(t *testing.T, svc *Service, name, specJSON string) {
	t.Helper()
	if _, err := svc.store.DB().Exec("UPDATE workflows SET spec_json = ? WHERE name = ?", specJSON, name); err != nil {
		t.Fatal(err)
	}
}

func mustApply(t *testing.T, svc *Service, yaml string) {
	t.Helper()
	if _, err := svc.ApplyWorkflow(context.Background(), yaml, "t"); err != nil {
		t.Fatal(err)
	}
}

func TestErrorMessages(t *testing.T) {
	ve := &ValidationError{Issues: workflow.Issues{{Path: "/name", Message: "required"}}}
	if !strings.HasPrefix(ve.Error(), "validation failed: ") || !strings.Contains(ve.Error(), "required") {
		t.Fatal(ve.Error())
	}
	if (&BadRequest{Msg: "bad"}).Error() != "bad" || (&Conflict{Msg: "busy"}).Error() != "busy" {
		t.Fatal("messages")
	}
	if errors.Is(&Conflict{}, ErrNotFound) {
		t.Fatal("a conflict is only ErrConflict")
	}
	if got := (&MissingTriggerFieldsError{Fields: []string{"a", "b"}}).Error(); got != "missing trigger field(s): a, b" {
		t.Fatal(got)
	}
}

func TestNewDefaultsAndSetters(t *testing.T) {
	svc := New(Options{})
	if svc.log == nil || svc.maxTimeout != 30*time.Minute || svc.Store() != nil {
		t.Fatalf("defaults: %+v", svc)
	}
	if _, err := svc.SubscribeRun("r"); err == nil {
		t.Fatal("subscribing without an event bus must fail")
	}
	var calls int
	svc.OnWorkflowChange(func() { calls++ })
	svc.OnWorkflowChange(func() { calls++ })
	svc.changed()
	if calls != 2 {
		t.Fatalf("every change listener runs: %d", calls)
	}
	svc.SetCatalog(nil)
	if orJSON("") != "{}" || orJSON("null") != "{}" || orJSON(`{"a":1}`) != `{"a":1}` {
		t.Fatal("orJSON")
	}
}

func TestWorkflowChangeListeners(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	var calls int
	svc.OnWorkflowChange(func() { calls++ })
	mustApply(t, svc, hello)
	if err := svc.SetWorkflowStatus(ctx, "hello", store.WorkflowDisabled); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteWorkflow(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("apply, disable and delete each notify: %d", calls)
	}
}

func TestUILogin(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	code, err := svc.NewUILoginCode(ctx, "alice", store.ScopeAdmin)
	if err != nil || !strings.HasPrefix(code, "icl_") {
		t.Fatalf("%q %v", code, err)
	}
	cookie, p, err := svc.UILogin(ctx, code)
	if err != nil || p.Name != "alice" || p.Scope != store.ScopeAdmin || cookie == "" {
		t.Fatalf("%q %+v %v", cookie, p, err)
	}
	if _, _, err := svc.UILogin(ctx, code); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a login code is single use: %v", err)
	}
	got, err := svc.UISession(ctx, cookie)
	if err != nil || got.Name != "alice" {
		t.Fatalf("%+v %v", got, err)
	}

	// An API token also signs in, with its own name and scope.
	plain, hash, _ := crypto.NewToken()
	if _, err := svc.store.CreateToken(ctx, "ci", hash, store.ScopeRead); err != nil {
		t.Fatal(err)
	}
	tokCookie, tp, err := svc.UILogin(ctx, plain)
	if err != nil || tp.Name != "ci" || tp.Scope != store.ScopeRead || tokCookie == cookie {
		t.Fatalf("%+v %v", tp, err)
	}

	for _, bad := range []string{"", "nope"} {
		if _, _, err := svc.UILogin(ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%q: %v", bad, err)
		}
		if _, err := svc.UISession(ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%q: %v", bad, err)
		}
	}

	if err := svc.UILogout(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.UILogout(ctx, cookie); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UISession(ctx, cookie); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a logged-out cookie must not resolve: %v", err)
	}
}

func TestUILoginStoreFailures(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	code, err := svc.NewUILoginCode(ctx, "alice", store.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	// The code is valid, but the browser session cannot be stored.
	execSQL(t, svc, "CREATE TRIGGER fail_session BEFORE INSERT ON ui_sessions WHEN NEW.kind = 'session' BEGIN SELECT RAISE(ABORT, 'injected failure'); END")
	if _, _, err := svc.UILogin(ctx, code); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a store failure is not an authentication failure: %v", err)
	}
	// A store that cannot be read is an error, not "unknown code".
	hide(t, svc, "ui_sessions")
	if _, _, err := svc.UILogin(ctx, "icl_whatever"); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
}

func TestWorkflowStoreFailures(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	mustApply(t, svc, hello)

	t.Run("versions", func(t *testing.T) {
		hide(t, svc, "workflow_versions")
		if _, err := svc.ListWorkflowVersions(ctx, "hello"); err == nil {
			t.Fatal("list versions")
		}
		if _, err := svc.GetWorkflowVersion(ctx, "hello", 1); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("get version: %v", err)
		}
	})
	t.Run("unknown workflow", func(t *testing.T) {
		if _, err := svc.ListWorkflowVersions(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := svc.GetWorkflowVersion(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if err := svc.SetWorkflowStatus(ctx, "nope", store.WorkflowActive); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := svc.StartRun(ctx, "nope", nil, "t"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
	t.Run("writes", func(t *testing.T) {
		failOn(t, svc, "UPDATE", "workflows")
		failOn(t, svc, "DELETE", "workflows")
		defer execSQL(t, svc, "DROP TRIGGER fail_update_workflows")
		defer execSQL(t, svc, "DROP TRIGGER fail_delete_workflows")
		if err := svc.SetWorkflowStatus(ctx, "hello", store.WorkflowDisabled); err == nil {
			t.Fatal("status")
		}
		if err := svc.DeleteWorkflow(ctx, "hello"); err == nil {
			t.Fatal("delete")
		}
	})
	t.Run("undecodable spec", func(t *testing.T) {
		mustApply(t, svc, policyWorkflow("allow"))
		corruptSpec(t, svc, "pol-allow", "{")
		specs, err := svc.ActiveSpecs(ctx)
		if err != nil || len(specs) != 1 || specs[0].Name != "hello" {
			t.Fatalf("an undecodable spec is skipped, not fatal: %v %v", specs, err)
		}
		info, err := svc.GetWorkflow(ctx, "pol-allow")
		if err != nil || info.Name != "pol-allow" || len(info.Steps) != 0 || info.YAML == "" {
			t.Fatalf("an undecodable spec still shows its row and YAML: %+v %v", info, err)
		}
		if _, err := svc.StartRun(ctx, "pol-allow", nil, "t"); err == nil {
			t.Fatal("start with an undecodable spec")
		}
		if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow"}); err == nil {
			t.Fatal("enqueue with an undecodable spec")
		}
	})
	t.Run("closed store", func(t *testing.T) {
		_ = svc.store.Close()
		if _, err := svc.ApplyWorkflow(ctx, hello, "t"); err == nil {
			t.Fatal("apply")
		}
		if _, err := svc.ListWorkflows(ctx); err == nil {
			t.Fatal("list")
		}
		if _, err := svc.ActiveSpecs(ctx); err == nil {
			t.Fatal("active specs")
		}
		if _, err := svc.GetWorkflow(ctx, "hello"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("get: %v", err)
		}
	})
}

func TestEnqueueEdgeCases(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	mustApply(t, svc, policyWorkflow("allow"))

	res, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow"})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := svc.GetRun(ctx, res.RunID)
	if run.TriggerKind != workflow.RunTriggerManual || string(run.Trigger) != "{}" {
		t.Fatalf("defaults: kind %q trigger %s", run.TriggerKind, run.Trigger)
	}
	_, err = svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow", Trigger: map[string]any{"c": make(chan int)}})
	var br *BadRequest
	if !errors.As(err, &br) || !strings.Contains(br.Msg, "trigger payload") {
		t.Fatalf("an unencodable trigger is a bad request: %v", err)
	}

	// A spec whose plan is invalid (unknown need) is refused before a run exists.
	mustApply(t, svc, policyWorkflow("queue"))
	w, _ := svc.store.GetWorkflow(ctx, "pol-queue")
	var spec map[string]any
	_ = json.Unmarshal([]byte(w.SpecJSON), &spec)
	spec["steps"].([]any)[0].(map[string]any)["needs"] = []string{"ghost"}
	b, _ := json.Marshal(spec)
	corruptSpec(t, svc, "pol-queue", string(b))
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-queue"}); err == nil {
		t.Fatal("a plan with an unknown need must not enqueue")
	}

	// The run cannot be stored.
	failOn(t, svc, "INSERT", "runs")
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow", IdempotencyKey: "k1"}); err == nil {
		t.Fatal("create run failure")
	}
	if _, err := svc.StartRun(ctx, "pol-allow", nil, "t"); err == nil {
		t.Fatal("start run failure")
	}
	execSQL(t, svc, "DROP TRIGGER fail_insert_runs")
	// A unique violation without a matching run is still an error.
	execSQL(t, svc, "CREATE TRIGGER dup BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: runs.idempotency_key'); END")
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow", IdempotencyKey: "k2"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("%v", err)
	}
}

func TestEnqueuePolicyStoreFailures(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	for _, p := range []string{"forbid", "replace"} {
		mustApply(t, svc, policyWorkflow(p))
	}
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-replace"}); err != nil {
		t.Fatal(err)
	}
	// replace: the active run cannot be cancelled.
	failOn(t, svc, "UPDATE", "runs")
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-replace"}); err == nil {
		t.Fatal("a failed cancel must stop the replacement")
	}
	// Policies and idempotency need to read runs.
	hide(t, svc, "runs")
	for _, req := range []workflow.EnqueueRequest{
		{Workflow: "pol-forbid"}, {Workflow: "pol-replace"}, {Workflow: "pol-forbid", IdempotencyKey: "k"},
	} {
		if _, err := svc.EnqueueRun(ctx, req); err == nil {
			t.Fatalf("%+v", req)
		}
	}
}

func TestRunQueriesStoreFailures(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	mustApply(t, svc, policyWorkflow("allow"))
	res, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow"})
	if err != nil {
		t.Fatal(err)
	}
	// Cancelling a running run asks the engine to kill it.
	execSQL(t, svc, "UPDATE runs SET status = 'running' WHERE id = '"+res.RunID+"'")
	got, err := svc.CancelRun(ctx, res.RunID)
	if err != nil || fr.cancelled != 1 || got.ID != res.RunID {
		t.Fatalf("%+v %v cancelled=%d", got, err, fr.cancelled)
	}

	t.Run("step_runs", func(t *testing.T) {
		hide(t, svc, "step_runs")
		if _, err := svc.GetRun(ctx, res.RunID); err == nil {
			t.Fatal("get run")
		}
		if _, err := svc.StepLogs(ctx, res.RunID, "s1", 0, 0, 0); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("step logs: %v", err)
		}
	})
	t.Run("runs", func(t *testing.T) {
		hide(t, svc, "runs")
		if _, err := svc.GetRun(ctx, res.RunID); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("get run: %v", err)
		}
		if _, err := svc.ListRuns(ctx, "", "", 10); err == nil {
			t.Fatal("list runs")
		}
		if _, err := svc.CancelRun(ctx, res.RunID); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("cancel: %v", err)
		}
	})
}

func TestStepLogsEdgeCases(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	mustApply(t, svc, policyWorkflow("allow"))
	res, _ := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-allow"})

	// A step that never ran has no log yet.
	page, err := svc.StepLogs(ctx, res.RunID, "s1", 0, 0, 0)
	if err != nil || page.Attempt != 0 || page.Data != nil {
		t.Fatalf("%+v %v", page, err)
	}

	setLog := func(path string) {
		t.Helper()
		row, err := svc.store.GetStepRun(ctx, res.RunID, "s1")
		if err != nil {
			t.Fatal(err)
		}
		row.Attempt, row.LogPath = 1, path
		if err := svc.store.UpdateStepRun(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "s1.1.log")
	if err := os.WriteFile(logPath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	setLog(logPath)
	for _, off := range []int64{-1, 100} {
		page, err := svc.StepLogs(ctx, res.RunID, "s1", 1, off, 0)
		if err != nil || page.Offset != 3 || len(page.Data) != 0 || page.Size != 3 {
			t.Fatalf("offset %d outside the file reads nothing at the end: %+v %v", off, page, err)
		}
	}
	// A tail without a line boundary is returned whole.
	if page, _ := svc.StepLogs(ctx, res.RunID, "s1", 1, 0, 2); string(page.Data) != "bc" || page.Offset != 1 {
		t.Fatalf("%+v", page)
	}

	// A path that cannot be opened is an error; a directory cannot be read.
	setLog("bad\x00path.1.log")
	if _, err := svc.StepLogs(ctx, res.RunID, "s1", 0, 0, 0); err == nil {
		t.Fatal("invalid log path")
	}
	if info, err := os.Stat(dir); err == nil && info.Size() > 0 {
		setLog(dir)
		if _, err := svc.StepLogs(ctx, res.RunID, "s1", 0, 0, 0); err == nil {
			t.Fatal("a directory is not a log")
		}
	}
}

func TestValidateOptionsIgnoresSecretErrors(t *testing.T) {
	svc := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	svc.SetSecrets(brokenSecrets{})
	if opts := svc.validateOptions(context.Background()); opts.SecretExists != nil {
		t.Fatal("without secret names the existence check is skipped")
	}
}

type brokenSecrets struct{}

func (brokenSecrets) Resolve(context.Context, string) (string, error) {
	return "", errors.New("locked")
}
func (brokenSecrets) Names(context.Context) ([]string, error) { return nil, errors.New("locked") }
func (brokenSecrets) Hosts(context.Context, string) ([]string, error) {
	return nil, errors.New("locked")
}
