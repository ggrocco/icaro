package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow/events"
)

type env struct {
	bus    *events.Bus
	t      *testing.T
	srv    *httptest.Server
	admin  *apiclient.Client
	reader *apiclient.Client
	svc    *service.Service
}

func newEnv(t *testing.T) *env {
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
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := events.New(ctx)
	svc := service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet, Events: bus})
	srv := httptest.NewServer(New(svc, quiet).Handler())
	t.Cleanup(srv.Close)

	mk := func(name, scope string) *apiclient.Client {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(srv.URL, plain)
	}
	return &env{bus: bus, t: t, srv: srv, admin: mk("admin", store.ScopeAdmin), reader: mk("reader", store.ScopeRead), svc: svc}
}

const wfYAML = `
version: 1
name: hello
steps:
  - id: say
    image: mirror.gcr.io/library/alpine:3.20
    command: echo ${{ trigger.who }}
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
	if err != nil || !strings.Contains(string(schema), `"$id": "https://icaro.dev/schemas/workflow/v1.json"`) {
		t.Fatalf("schema: %v", err)
	}
}

func TestWorkflowAndRunEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	v, err := e.admin.Validate(ctx, strings.Replace(wfYAML, "command:", "comand:", 1))
	if err != nil || v.Valid || len(v.Issues) == 0 {
		t.Fatalf("validate: %v %+v", err, v)
	}
	_, err = e.admin.ApplyWorkflow(ctx, strings.Replace(wfYAML, "command:", "comand:", 1))
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

	_, err = e.admin.Run(ctx, "hello", nil)
	if !errorsAs(err, &apiErr) || apiErr.Status != 400 || len(apiErr.Fields) != 1 || apiErr.Fields[0] != "who" {
		t.Fatalf("missing trigger fields: %v", err)
	}
	run, err := e.admin.Run(ctx, "hello", map[string]any{"who": "api"})
	var compact bytes.Buffer
	_ = json.Compact(&compact, run.Trigger)
	if err != nil || run.Status != "queued" || compact.String() != `{"who":"api"}` || len(run.Steps) != 1 || run.Steps[0].ID != "say" {
		t.Fatalf("run: %v %+v", err, run)
	}
	runs, _ := e.admin.ListRuns(ctx, "hello", "", 10)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("list runs: %+v", runs)
	}
	logs, err := e.admin.StepLogs(ctx, run.ID, "say", 0, 100)
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
	if _, err := e.admin.Run(ctx, "hello", map[string]any{"who": "x"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("run disabled: %v", err)
	}
	if err := e.admin.DeleteWorkflow(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.DeleteWorkflow(ctx, "hello"); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestRunEventsStream(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.admin.ApplyWorkflow(ctx, wfYAML); err != nil {
		t.Fatal(err)
	}
	run, err := e.admin.Run(ctx, "hello", map[string]any{"who": "sse"})
	if err != nil {
		t.Fatal(err)
	}
	st := e.svc.Store()
	logPath := filepath.Join(t.TempDir(), "say.1.log")
	if err := os.WriteFile(logPath, []byte("backlog line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	row, _ := st.GetStepRun(ctx, run.ID, "say")
	row.Status, row.Attempt, row.LogPath = store.StepRunning, 1, logPath
	if err := st.UpdateStepRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	var got []string
	done := make(chan error, 1)
	connected := make(chan struct{})
	go func() {
		done <- e.reader.Events(ctx, run.ID, true, func(ev apiclient.Event) bool {
			var payload struct {
				Line   string `json:"line"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal(ev.Data, &payload)
			got = append(got, ev.Name+":"+payload.Line+payload.Status)
			if ev.Name == "snapshot" && len(got) == 1 {
				close(connected)
			}
			return true
		})
	}()
	<-connected
	time.Sleep(100 * time.Millisecond) // let the backlog flush
	bus := e.bus
	// A line already in the backlog (offset <= 13) is not repeated.
	bus.Publish(events.Event{Type: events.TypeLog, RunID: run.ID, Step: "say", Attempt: 1, Line: "backlog line", Offset: 13})
	bus.Publish(events.Event{Type: events.TypeLog, RunID: run.ID, Step: "say", Attempt: 1, Line: "live line", Offset: 23})
	bus.Publish(events.Event{Type: events.TypeStep, RunID: run.ID, Step: "say", Status: "succeeded"})
	if err := st.FinishRun(ctx, run.ID, store.RunSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	bus.Publish(events.Event{Type: events.TypeRun, RunID: run.ID, Status: store.RunSucceeded})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stream did not end: %v", got)
	}
	want := "snapshot:queued log:backlog line log:live line step:succeeded end:succeeded"
	if strings.Join(got, " ") != want {
		t.Fatalf("events:\n got  %v\n want %s", got, want)
	}

	// A finished run's stream ends right after the snapshot.
	var names []string
	if err := e.reader.Events(ctx, run.ID, false, func(ev apiclient.Event) bool { names = append(names, ev.Name); return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "snapshot,end" {
		t.Fatal(names)
	}
	if err := e.reader.Events(ctx, "missing", false, func(apiclient.Event) bool { return true }); !apiclient.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestJSONBodyForYAML(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/workflows/validate", strings.NewReader(`{"yaml":"version: 1\nname: xyz\nsteps: []\n"}`))
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
