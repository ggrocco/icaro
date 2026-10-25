package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"icaro/internal/crypto"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

type fakeRunner struct{ woke, cancelled int }

func (f *fakeRunner) Wake()         { f.woke++ }
func (f *fakeRunner) Cancel(string) { f.cancelled++ }

type fakeSecrets map[string]string

func (s fakeSecrets) Resolve(_ context.Context, n string) (string, error) { return s[n], nil }
func (s fakeSecrets) Names(context.Context) ([]string, error) {
	var out []string
	for k := range s {
		out = append(out, k)
	}
	return out, nil
}
func (s fakeSecrets) Hosts(context.Context, string) ([]string, error) { return nil, nil }

// boundSecrets adds host bindings (integration credentials).
type boundSecrets struct {
	fakeSecrets
	hosts map[string][]string
}

func (s boundSecrets) Hosts(_ context.Context, n string) ([]string, error) { return s.hosts[n], nil }

func newService(t *testing.T) (*Service, *fakeRunner) {
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
	fr := &fakeRunner{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet})
	svc.SetRunner(fr)
	svc.SetEvents(events.New(ctx))
	return svc, fr
}

const hello = `
version: 1
name: hello
triggers:
  - type: manual
steps:
  - id: say
    image: mirror.gcr.io/library/alpine:3.24
    command: echo ${{ trigger.who }} ${{ env.TOKEN }}
    when: ${{ !has(trigger.quiet) }}
env:
  TOKEN: ${{ secrets.api }}
`

// A host-bound secret is an integration credential: workflows cannot
// interpolate it, whatever its name.
func TestIntegrationCredentialIsRefused(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	svc.SetSecrets(boundSecrets{fakeSecrets: fakeSecrets{"api": "x"}, hosts: map[string][]string{"api": {"api.example.com"}}})
	_, err := svc.ApplyWorkflow(ctx, hello, "alice")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Path != "/env/TOKEN" || !strings.Contains(ve.Issues[0].Message, "integration credential") || !strings.Contains(ve.Issues[0].Hint, "uses:") {
		t.Fatalf("expected an integration credential issue, got %v", err)
	}
}

func TestWorkflowLifecycle(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	svc.SetSecrets(fakeSecrets{})
	_, err := svc.ApplyWorkflow(ctx, hello, "alice")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Path != "/env/TOKEN" || !strings.Contains(ve.Issues[0].Hint, "icaro secret set api") {
		t.Fatalf("expected missing secret issue, got %v", err)
	}
	svc.SetSecrets(fakeSecrets{"api": "x"})
	info, err := svc.ApplyWorkflow(ctx, hello, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 1 || len(info.Steps) != 1 || info.YAML == "" || info.Concurrency != "allow" {
		t.Fatalf("info: %+v", info)
	}
	if strings.Join(info.TriggerFields.Required, ",") != "who" || strings.Join(info.TriggerFields.Optional, ",") != "quiet" {
		t.Fatalf("trigger fields %+v", info.TriggerFields)
	}
	info2, _ := svc.ApplyWorkflow(ctx, hello, "bob")
	if info2.Version != 2 {
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
	if _, err := svc.GetWorkflowVersion(ctx, "hello", 9); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	list, _ := svc.ListWorkflows(ctx)
	if len(list) != 1 || list[0].YAML != "" {
		t.Fatalf("list should omit yaml: %+v", list)
	}
	specs, err := svc.ActiveSpecs(ctx)
	if err != nil || len(specs) != 1 || specs[0].Name != "hello" {
		t.Fatalf("active specs %v %v", specs, err)
	}
	if err := svc.SetWorkflowStatus(ctx, "hello", "bogus"); err == nil {
		t.Fatal("bad status accepted")
	}
	if err := svc.SetWorkflowStatus(ctx, "hello", store.WorkflowDisabled); err != nil {
		t.Fatal(err)
	}
	if specs, _ := svc.ActiveSpecs(ctx); len(specs) != 0 {
		t.Fatal("disabled workflow is not active")
	}
	if _, err := svc.StartRun(ctx, "hello", map[string]any{"who": "x"}, "t"); err == nil {
		t.Fatal("disabled workflow must not run")
	}
	if err := svc.DeleteWorkflow(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetWorkflow(ctx, "hello"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := svc.DeleteWorkflow(ctx, "hello"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestValidateReturnsStructuredIssues(t *testing.T) {
	svc, _ := newService(t)
	_, issues, err := svc.ValidateWorkflow(context.Background(), "version: 1\nname: bad-one\nsteps:\n  - id: aa\n    image: x\n    needz: []\n")
	if err != nil || len(issues) != 1 || issues[0].Path != "/steps/0/needz" || issues[0].Hint != `did you mean "needs"?` {
		t.Fatalf("%v %+v", err, issues)
	}
	_, issues, _ = svc.ValidateWorkflow(context.Background(), "a: [")
	if len(issues) != 1 || issues[0].Path != "/" {
		t.Fatalf("decode error issue: %+v", issues)
	}
}

func TestManualRunsAndLogs(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	if _, err := svc.ApplyWorkflow(ctx, hello, "alice"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartRun(ctx, "hello", nil, "alice")
	var mt *MissingTriggerFieldsError
	if !errors.As(err, &mt) || mt.Fields[0] != "who" {
		t.Fatalf("missing trigger fields: %v", err)
	}
	sub, _ := svc.SubscribeRun("x")
	defer sub.Cancel()
	run, err := svc.StartRun(ctx, "hello", map[string]any{"who": "icaro"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunQueued || run.TriggerKind != "manual" || run.Actor != "alice" || len(run.Steps) != 1 || fr.woke != 1 {
		t.Fatalf("run: %+v woke=%d", run, fr.woke)
	}
	if run.Steps[0].ID != "say" || run.Steps[0].Kind != "image" || string(run.Trigger) != `{"who":"icaro"}` {
		t.Fatalf("steps: %+v", run.Steps)
	}
	runs, _ := svc.ListRuns(ctx, "hello", "", 10)
	if len(runs) != 1 {
		t.Fatal("list runs")
	}
	if _, err := svc.ListRuns(ctx, "nope", "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// Logs: write a fake log for attempt 1 and read pages.
	row, _ := svc.store.GetStepRun(ctx, run.ID, "say")
	logPath := filepath.Join(t.TempDir(), "say.1.log")
	_ = os.WriteFile(logPath, []byte("one\ntwo\nthree\n"), 0o600)
	row.Attempt, row.LogPath, row.Status = 1, logPath, store.StepRunning
	_ = svc.store.UpdateStepRun(ctx, row)
	page, err := svc.StepLogs(ctx, run.ID, "say", 0, 0, 0)
	if err != nil || string(page.Data) != "one\ntwo\nthree\n" || page.Size != 14 || page.Attempt != 1 {
		t.Fatalf("logs: %+v %v", page, err)
	}
	page, _ = svc.StepLogs(ctx, run.ID, "say", 1, 4, 0)
	if string(page.Data) != "two\nthree\n" || page.Offset != 4 {
		t.Fatalf("offset page %q", page.Data)
	}
	page, _ = svc.StepLogs(ctx, run.ID, "say", 1, 0, 8)
	if string(page.Data) != "three\n" {
		t.Fatalf("tail page %q", page.Data)
	}
	if page, _ := svc.StepLogs(ctx, run.ID, "say", 2, 0, 0); len(page.Data) != 0 {
		t.Fatal("missing attempt log should be empty")
	}
	if _, err := svc.StepLogs(ctx, run.ID, "nope", 0, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	got, err := svc.CancelRun(ctx, run.ID)
	if err != nil || got.Status != store.RunCancelled {
		t.Fatalf("cancel queued: %+v %v", got, err)
	}
	if _, err := svc.CancelRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := svc.GetRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func policyWorkflow(policy string) string {
	return "version: 1\nname: pol-" + policy + "\nconcurrency: " + policy + "\nsteps:\n  - { id: s1, image: x }\n"
}

func TestConcurrencyPoliciesAndIdempotency(t *testing.T) {
	svc, fr := newService(t)
	ctx := context.Background()
	for _, p := range []string{"forbid", "queue", "replace", "allow"} {
		if _, err := svc.ApplyWorkflow(ctx, policyWorkflow(p), "t"); err != nil {
			t.Fatal(err)
		}
	}
	enq := func(name, key string) workflow.EnqueueResult {
		t.Helper()
		res, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: name, TriggerKind: workflow.RunTriggerIntegration, IdempotencyKey: key, DeliveryID: key})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Idempotency: the same key never creates a second run.
	a := enq("pol-allow", "github:d1")
	b := enq("pol-allow", "github:d1")
	if a.RunID == "" || !b.Deduplicated || b.RunID != a.RunID {
		t.Fatalf("dedup: %+v %+v", a, b)
	}
	if c := enq("pol-allow", ""); c.RunID == "" || c.RunID == a.RunID {
		t.Fatal("allow runs side by side")
	}

	// forbid: skipped while a run is active.
	f1 := enq("pol-forbid", "")
	f2 := enq("pol-forbid", "")
	if f1.RunID == "" || f2.RunID != "" || !strings.Contains(f2.Skipped, "concurrency forbid") {
		t.Fatalf("forbid: %+v %+v", f1, f2)
	}
	_, err := svc.StartRun(ctx, "pol-forbid", nil, "t")
	var conflict *Conflict
	if !errors.As(err, &conflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("manual forbid: %v", err)
	}

	// replace: the active run is cancelled first.
	r1 := enq("pol-replace", "")
	r2 := enq("pol-replace", "")
	old, _ := svc.GetRun(ctx, r1.RunID)
	if old.Status != store.RunCancelled || r2.RunID == "" {
		t.Fatalf("replace: old=%s new=%+v", old.Status, r2)
	}

	// queue: both runs are created; the store's claim serializes them.
	q1, q2 := enq("pol-queue", ""), enq("pol-queue", "")
	if q1.RunID == "" || q2.RunID == "" {
		t.Fatal("queue creates runs")
	}
	run, _ := svc.GetRun(ctx, q2.RunID)
	if run.Concurrency != "queue" || run.TriggerKind != "integration" {
		t.Fatalf("queued run %+v", run)
	}
	if fr.woke == 0 {
		t.Fatal("engine not woken")
	}
	if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// forbid and replace decide and insert atomically: concurrent enqueues
// (a webhook burst, a cron tick racing a manual run) never leave two
// active runs of a forbid or replace workflow.
func TestConcurrentEnqueuesHonorPolicy(t *testing.T) {
	for _, policy := range []string{"forbid", "replace"} {
		t.Run(policy, func(t *testing.T) {
			svc, _ := newService(t)
			ctx := context.Background()
			mustApply(t, svc, policyWorkflow(policy))
			const n = 24
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := svc.EnqueueRun(ctx, workflow.EnqueueRequest{Workflow: "pol-" + policy}); err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			w, _ := svc.store.GetWorkflow(ctx, "pol-"+policy)
			active, err := svc.store.ActiveRuns(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 1 {
				t.Fatalf("%d active runs, want 1", len(active))
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	plain, hash, _ := crypto.NewToken()
	if _, err := svc.store.CreateToken(ctx, "t", hash, store.ScopeWrite); err != nil {
		t.Fatal(err)
	}
	if tok, err := svc.Authenticate(ctx, plain); err != nil || tok.Scope != store.ScopeWrite {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "nope"} {
		if _, err := svc.Authenticate(ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if !ScopeAllows(store.ScopeAdmin, store.ScopeRead) || ScopeAllows(store.ScopeRead, store.ScopeWrite) || ScopeAllows(store.ScopeAdmin, "") {
		t.Fatal("scopes")
	}
	if !Terminal(store.RunFailed) || Terminal(store.RunRunning) {
		t.Fatal("terminal")
	}
}
