package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ggrocco/icaro/internal/apiclient"
	"github.com/ggrocco/icaro/internal/crypto"
	"github.com/ggrocco/icaro/internal/service"
	"github.com/ggrocco/icaro/internal/store"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	admin  *apiclient.Client
	reader *apiclient.Client
	svc    *service.Service
}

func newEnv(t *testing.T) *env {
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
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet})
	srv := httptest.NewServer(New(svc, quiet).Handler())
	t.Cleanup(srv.Close)

	mk := func(name, scope string) *apiclient.Client {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(srv.URL, plain)
	}
	return &env{t: t, srv: srv, admin: mk("admin", store.ScopeAdmin), reader: mk("reader", store.ScopeRead), svc: svc}
}

const wfYAML = `
name: hello
inputs:
  who: { default: world }
steps:
  - name: say
    run: { image: alpine, script: "echo {{ inputs.who }}" }
`

func TestAuthAndScopes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	resp, err := http.Get(e.srv.URL + "/api/v1/workflows")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %d", err, resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp, _ = http.Get(e.srv.URL + "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	bad := apiclient.New(e.srv.URL, "icaro_nope")
	if _, err := bad.ListWorkflows(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := e.reader.ApplyWorkflow(ctx, wfYAML); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("reader apply: %v", err)
	}
	if _, err := e.reader.ListWorkflows(ctx); err != nil {
		t.Fatalf("reader list: %v", err)
	}
	schema, err := e.reader.Schema(ctx)
	if err != nil || !strings.Contains(string(schema), `"$id": "https://icaro.dev/schema/workflow"`) {
		t.Fatalf("schema: %v", err)
	}
}

func TestWorkflowAndRunEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	v, err := e.admin.Validate(ctx, strings.Replace(wfYAML, "image:", "imgae:", 1))
	if err != nil || v.Valid || len(v.Issues) == 0 {
		t.Fatalf("validate: %v %+v", err, v)
	}
	_, err = e.admin.ApplyWorkflow(ctx, strings.Replace(wfYAML, "image:", "imgae:", 1))
	var apiErr *apiclient.Error
	if !strings.Contains(err.Error(), "validation failed") || !errorsAs(err, &apiErr) || apiErr.Status != 422 || len(apiErr.Issues) == 0 {
		t.Fatalf("apply invalid: %v", err)
	}

	info, err := e.admin.ApplyWorkflow(ctx, wfYAML)
	if err != nil || info.Version != 1 || info.Name != "hello" {
		t.Fatalf("apply: %v %+v", err, info)
	}
	got, err := e.admin.GetWorkflow(ctx, "hello")
	if err != nil || got.YAML != wfYAML || got.Steps[0] != "say" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := e.admin.GetWorkflow(ctx, "missing"); !apiclient.IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
	vs, _ := e.admin.ListVersions(ctx, "hello")
	if len(vs) != 1 || vs[0].Actor != "token:admin" {
		t.Fatalf("versions: %+v", vs)
	}
	v1, _ := e.admin.GetVersion(ctx, "hello", 1)
	if v1.YAML != wfYAML {
		t.Fatal("version yaml")
	}

	run, err := e.admin.Run(ctx, "hello", map[string]any{"who": "api"})
	var compact bytes.Buffer
	_ = json.Compact(&compact, run.Input)
	if err != nil || run.Status != "queued" || compact.String() != `{"who":"api"}` || len(run.Steps) != 1 {
		t.Fatalf("run: %v %+v", err, run)
	}
	runs, _ := e.admin.ListRuns(ctx, "hello", "", 10)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("list runs: %+v", runs)
	}
	logs, err := e.admin.StepLogs(ctx, run.ID, 0, 100)
	if err != nil || len(logs) != 0 {
		t.Fatalf("logs: %v %q", err, logs)
	}
	c, err := e.admin.CancelRun(ctx, run.ID)
	if err != nil || c.Status != "cancelled" {
		t.Fatalf("cancel: %v %+v", err, c)
	}
	final, err := e.admin.WaitRun(ctx, run.ID, 0)
	if err != nil || final.Status != "cancelled" {
		t.Fatalf("wait: %v %+v", err, final)
	}

	off, err := e.admin.SetWorkflowEnabled(ctx, "hello", false)
	if err != nil || off.Status != "disabled" {
		t.Fatalf("disable: %v %+v", err, off)
	}
	if _, err := e.admin.Run(ctx, "hello", nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("run disabled: %v", err)
	}
	if err := e.admin.DeleteWorkflow(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.DeleteWorkflow(ctx, "hello"); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestConnectionsEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.reader.UpsertConnection(ctx, "x", "generic", nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("reader create: %v", err)
	}
	info, err := e.admin.UpsertConnection(ctx, "slack", "bearer", map[string]string{"token": "xoxb-123456"})
	if err != nil || info.Type != "bearer" || info.FieldNames[0] != "token" {
		t.Fatalf("create: %v %+v", err, info)
	}
	list, _ := e.reader.ListConnections(ctx)
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	resp, _ := http.Get(e.srv.URL + "/api/v1/connections")
	_ = resp.Body.Close()
	if _, err := e.admin.UpsertConnection(ctx, "bad", "bearer", map[string]string{}); err == nil || !strings.Contains(err.Error(), "requires field") {
		t.Fatalf("bad request mapping: %v", err)
	}
	if err := e.admin.DeleteConnection(ctx, "slack"); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.DeleteConnection(ctx, "slack"); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestJSONBodyForYAML(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/workflows/validate", strings.NewReader(`{"yaml":"name: x\nsteps: []\n"}`))
	req.Header.Set("Authorization", "Bearer "+e.admin.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"valid": false`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func errorsAs(err error, target **apiclient.Error) bool {
	e, ok := err.(*apiclient.Error)
	if ok {
		*target = e
	}
	return ok
}
