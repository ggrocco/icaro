package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/memory"
	"icaro/internal/session"
)

// recorded is what the fake server saw for the last request.
type recorded struct {
	method, uri, auth, contentType, body string
}

// fakeServer replies to every request with reply (status 200) and
// records the request.
func fakeServer(t *testing.T, reply string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*rec = recorded{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "tok"), rec
}

func TestEndpoints(t *testing.T) {
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	cases := []struct {
		name   string
		call   func(context.Context, *Client) error
		method string
		uri    string
		body   string // expected JSON body, "" for none
	}{
		{"Health", func(ctx context.Context, c *Client) error { return c.Health(ctx) }, "GET", "/healthz", ""},
		{"Schema", func(ctx context.Context, c *Client) error { _, err := c.Schema(ctx); return err }, "GET", "/api/v1/schema/workflow", ""},
		{"Validate", func(ctx context.Context, c *Client) error { _, err := c.Validate(ctx, "name: x"); return err }, "POST", "/api/v1/workflows/validate", ""},
		{"ApplyWorkflow", func(ctx context.Context, c *Client) error { _, err := c.ApplyWorkflow(ctx, "name: x"); return err }, "POST", "/api/v1/workflows", ""},
		{"ListWorkflows", func(ctx context.Context, c *Client) error { _, err := c.ListWorkflows(ctx); return err }, "GET", "/api/v1/workflows", ""},
		{"GetWorkflow", func(ctx context.Context, c *Client) error { _, err := c.GetWorkflow(ctx, "a b"); return err }, "GET", "/api/v1/workflows/a%20b", ""},
		{"DeleteWorkflow", func(ctx context.Context, c *Client) error { return c.DeleteWorkflow(ctx, "w") }, "DELETE", "/api/v1/workflows/w", ""},
		{"Enable", func(ctx context.Context, c *Client) error { _, err := c.SetWorkflowEnabled(ctx, "w", true); return err }, "POST", "/api/v1/workflows/w/enable", ""},
		{"Disable", func(ctx context.Context, c *Client) error {
			_, err := c.SetWorkflowEnabled(ctx, "w", false)
			return err
		}, "POST", "/api/v1/workflows/w/disable", ""},
		{"ListVersions", func(ctx context.Context, c *Client) error { _, err := c.ListVersions(ctx, "w"); return err }, "GET", "/api/v1/workflows/w/versions", ""},
		{"GetVersion", func(ctx context.Context, c *Client) error { _, err := c.GetVersion(ctx, "w", 3); return err }, "GET", "/api/v1/workflows/w/versions/3", ""},
		{"Run", func(ctx context.Context, c *Client) error {
			_, err := c.Run(ctx, "w", map[string]any{"k": "v"})
			return err
		}, "POST", "/api/v1/workflows/w/runs", `{"trigger":{"k":"v"}}`},
		{"GetRun", func(ctx context.Context, c *Client) error { _, err := c.GetRun(ctx, "r1"); return err }, "GET", "/api/v1/runs/r1", ""},
		{"ListRuns all", func(ctx context.Context, c *Client) error { _, err := c.ListRuns(ctx, "", "", 0); return err }, "GET", "/api/v1/runs?", ""},
		{"ListRuns filtered", func(ctx context.Context, c *Client) error { _, err := c.ListRuns(ctx, "w", "failed", 5); return err }, "GET", "/api/v1/runs?limit=5&status=failed&workflow=w", ""},
		{"CancelRun", func(ctx context.Context, c *Client) error { _, err := c.CancelRun(ctx, "r1"); return err }, "POST", "/api/v1/runs/r1/cancel", ""},
		{"StepLogs", func(ctx context.Context, c *Client) error { _, err := c.StepLogs(ctx, "r1", "s", 2, 100); return err }, "GET", "/api/v1/runs/r1/steps/s/logs?attempt=2&tail=100", ""},
		{"StepLogs latest", func(ctx context.Context, c *Client) error { _, err := c.StepLogs(ctx, "r1", "s", 0, 0); return err }, "GET", "/api/v1/runs/r1/steps/s/logs?", ""},

		{"Integrations", func(ctx context.Context, c *Client) error { _, err := c.Integrations(ctx); return err }, "GET", "/api/v1/integrations", ""},
		{"Integration", func(ctx context.Context, c *Client) error { _, err := c.Integration(ctx, "github"); return err }, "GET", "/api/v1/integrations/github", ""},
		{"IntegrationStatus", func(ctx context.Context, c *Client) error { _, err := c.IntegrationStatus(ctx); return err }, "GET", "/api/v1/integrations/status", ""},
		{"ReloadIntegrations", func(ctx context.Context, c *Client) error { _, err := c.ReloadIntegrations(ctx); return err }, "POST", "/api/v1/integrations/reload", ""},
		{"EditManifest", func(ctx context.Context, c *Client) error { _, err := c.EditManifest(ctx, "gh", "y"); return err }, "PUT", "/api/v1/integrations/gh/manifest", `{"yaml":"y"}`},
		{"PlanCredential", func(ctx context.Context, c *Client) error { _, err := c.PlanCredential(ctx, "gh", "token"); return err }, "GET", "/api/v1/credentials/plan?integration=gh&name=token", ""},
		{"AddCredential default hosts", func(ctx context.Context, c *Client) error {
			_, err := c.AddCredential(ctx, "gh", "token", "v", nil)
			return err
		}, "POST", "/api/v1/credentials", `{"integration":"gh","name":"token","value":"v"}`},
		{"AddCredential hosts", func(ctx context.Context, c *Client) error {
			_, err := c.AddCredential(ctx, "gh", "token", "v", []string{"api.github.com"})
			return err
		}, "POST", "/api/v1/credentials", `{"hosts":["api.github.com"],"integration":"gh","name":"token","value":"v"}`},
		{"Credentials", func(ctx context.Context, c *Client) error { _, err := c.Credentials(ctx); return err }, "GET", "/api/v1/credentials", ""},
		{"DeleteCredential", func(ctx context.Context, c *Client) error { return c.DeleteCredential(ctx, "t") }, "DELETE", "/api/v1/credentials/t", ""},
		{"Deliveries all", func(ctx context.Context, c *Client) error { _, err := c.Deliveries(ctx, "", "", 0); return err }, "GET", "/api/v1/deliveries?", ""},
		{"Deliveries filtered", func(ctx context.Context, c *Client) error { _, err := c.Deliveries(ctx, "gh", "failed", 2); return err }, "GET", "/api/v1/deliveries?integration=gh&limit=2&status=failed", ""},
		{"Delivery", func(ctx context.Context, c *Client) error { _, err := c.Delivery(ctx, "d1"); return err }, "GET", "/api/v1/deliveries/d1", ""},
		{"Redeliver", func(ctx context.Context, c *Client) error { _, err := c.Redeliver(ctx, "d1"); return err }, "POST", "/api/v1/deliveries/d1/redeliver", ""},
		{"WebhookTokens", func(ctx context.Context, c *Client) error { _, err := c.WebhookTokens(ctx); return err }, "GET", "/api/v1/webhook-tokens", ""},
		{"RotateWebhookToken", func(ctx context.Context, c *Client) error { _, err := c.RotateWebhookToken(ctx, "w"); return err }, "POST", "/api/v1/webhook-tokens/w/rotate", ""},
		{"Schedules", func(ctx context.Context, c *Client) error { _, err := c.Schedules(ctx); return err }, "GET", "/api/v1/schedules", ""},

		{"WriteMemory", func(ctx context.Context, c *Client) error {
			_, err := c.WriteMemory(ctx, memory.RESTWrite{Project: "p"})
			return err
		}, "POST", "/api/v1/memory", ""},
		{"QueryMemory minimal", func(ctx context.Context, c *Client) error {
			_, err := c.QueryMemory(ctx, MemoryQuery{Project: "p"})
			return err
		}, "GET", "/api/v1/memory?project=p", ""},
		{"QueryMemory full", func(ctx context.Context, c *Client) error {
			_, err := c.QueryMemory(ctx, MemoryQuery{Project: "p", Text: "q", Kinds: []string{"a", "b"}, Branch: "main", Agent: "claude", SourcePrefix: "src/", Status: "active", Since: &since, Limit: 7})
			return err
		}, "GET", "/api/v1/memory?agent=claude&branch=main&kind=a%2Cb&limit=7&project=p&q=q&since=2026-01-02T02%3A04%3A05Z&source=src%2F&status=active", ""},
		{"MemoryProvenance", func(ctx context.Context, c *Client) error { _, err := c.MemoryProvenance(ctx, "p q", "m1"); return err }, "GET", "/api/v1/memory/m1?project=p+q", ""},
		{"PendingHandoffs", func(ctx context.Context, c *Client) error { _, err := c.PendingHandoffs(ctx, "p", ""); return err }, "GET", "/api/v1/memory/handoffs?project=p", ""},
		{"PendingHandoffs branch", func(ctx context.Context, c *Client) error { _, err := c.PendingHandoffs(ctx, "p", "b"); return err }, "GET", "/api/v1/memory/handoffs?branch=b&project=p", ""},
		{"AcceptHandoff", func(ctx context.Context, c *Client) error {
			_, err := c.AcceptHandoff(ctx, memory.RESTAccept{Project: "p"}, "m1")
			return err
		}, "POST", "/api/v1/memory/m1/accept", `{"project":"p"}`},
		{"IngestMemory", func(ctx context.Context, c *Client) error {
			_, err := c.IngestMemory(ctx, memory.RESTIngest{Path: "/x"})
			return err
		}, "POST", "/api/v1/memory/ingest", `{"path":"/x"}`},
		{"ImportMemory", func(ctx context.Context, c *Client) error {
			_, err := c.ImportMemory(ctx, memory.RESTImport{Project: "p", Format: "f", Data: "d"})
			return err
		}, "POST", "/api/v1/memory/import", `{"project":"p","format":"f","data":"d"}`},

		{"ListPreviews", func(ctx context.Context, c *Client) error { _, err := c.ListPreviews(ctx); return err }, "GET", "/api/v1/previews", ""},
		{"GetPreview", func(ctx context.Context, c *Client) error { _, err := c.GetPreview(ctx, "p1"); return err }, "GET", "/api/v1/previews/p1", ""},
		{"PreviewUp", func(ctx context.Context, c *Client) error { _, err := c.PreviewUp(ctx, "/w", true); return err }, "POST", "/api/v1/previews/up", `{"force":true,"path":"/w"}`},
		{"PreviewDown", func(ctx context.Context, c *Client) error { _, err := c.PreviewDown(ctx, "/w"); return err }, "POST", "/api/v1/previews/down", `{"path":"/w"}`},
		{"PreviewRemove", func(ctx context.Context, c *Client) error { return c.PreviewRemove(ctx, "/w") }, "POST", "/api/v1/previews/remove", `{"path":"/w"}`},
		{"PreviewLint", func(ctx context.Context, c *Client) error { _, err := c.PreviewLint(ctx, "/w"); return err }, "POST", "/api/v1/previews/lint", `{"path":"/w"}`},
		{"PreviewAllow", func(ctx context.Context, c *Client) error {
			_, err := c.PreviewAllow(ctx, "/w", []string{"f1"})
			return err
		}, "POST", "/api/v1/previews/allow", `{"confirm":true,"findings":["f1"],"path":"/w"}`},
		{"PreviewLogs", func(ctx context.Context, c *Client) error { _, err := c.PreviewLogs(ctx, "p1", 50); return err }, "GET", "/api/v1/previews/p1/logs?tail=50", ""},

		{"CreateSession", func(ctx context.Context, c *Client) error {
			_, err := c.CreateSession(ctx, session.CreateRequest{RepoPath: "/r"})
			return err
		}, "POST", "/api/v1/sessions", `{"repo_path":"/r"}`},
		{"EnsureSession", func(ctx context.Context, c *Client) error {
			_, err := c.EnsureSession(ctx, session.CreateRequest{RepoPath: "/r"})
			return err
		}, "POST", "/api/v1/sessions/ensure", `{"repo_path":"/r"}`},
		{"ListSessions all", func(ctx context.Context, c *Client) error { _, err := c.ListSessions(ctx, "", "", ""); return err }, "GET", "/api/v1/sessions", ""},
		{"ListSessions filtered", func(ctx context.Context, c *Client) error {
			_, err := c.ListSessions(ctx, "p", "agent", "running")
			return err
		}, "GET", "/api/v1/sessions?kind=agent&project=p&status=running", ""},
		{"GetSession", func(ctx context.Context, c *Client) error { _, err := c.GetSession(ctx, "s1"); return err }, "GET", "/api/v1/sessions/s1", ""},
		{"SessionExec", func(ctx context.Context, c *Client) error { _, err := c.SessionExec(ctx, "s1"); return err }, "GET", "/api/v1/sessions/s1/exec", ""},
		{"StopSession", func(ctx context.Context, c *Client) error { _, err := c.StopSession(ctx, "s1"); return err }, "POST", "/api/v1/sessions/s1/stop", ""},
		{"StartSession", func(ctx context.Context, c *Client) error { _, err := c.StartSession(ctx, "s1"); return err }, "POST", "/api/v1/sessions/s1/start", ""},
		{"RemoveSession", func(ctx context.Context, c *Client) error { return c.RemoveSession(ctx, "s1", false) }, "DELETE", "/api/v1/sessions/s1", ""},
		{"RemoveSession keep", func(ctx context.Context, c *Client) error { return c.RemoveSession(ctx, "s1", true) }, "DELETE", "/api/v1/sessions/s1?keep_worktree=true", ""},
		{"ListProjects", func(ctx context.Context, c *Client) error { _, err := c.ListProjects(ctx); return err }, "GET", "/api/v1/projects", ""},
		{"ListProfiles", func(ctx context.Context, c *Client) error { _, err := c.ListProfiles(ctx); return err }, "GET", "/api/v1/profiles", ""},
		{"PrepareLogin", func(ctx context.Context, c *Client) error {
			_, err := c.PrepareLogin(ctx, "default", false)
			return err
		}, "POST", "/api/v1/profiles/default/login", ""},
		{"PrepareLogin force", func(ctx context.Context, c *Client) error { _, err := c.PrepareLogin(ctx, "default", true); return err }, "POST", "/api/v1/profiles/default/login?force=true", ""},
		{"ListSecrets", func(ctx context.Context, c *Client) error { _, err := c.ListSecrets(ctx); return err }, "GET", "/api/v1/secrets", ""},
		{"SetSecret", func(ctx context.Context, c *Client) error { return c.SetSecret(ctx, "k", "v", []string{"h"}) }, "PUT", "/api/v1/secrets/k", `{"hosts":["h"],"value":"v"}`},
		{"DeleteSecret", func(ctx context.Context, c *Client) error { return c.DeleteSecret(ctx, "k") }, "DELETE", "/api/v1/secrets/k", ""},
		{"Do", func(ctx context.Context, c *Client) error {
			return c.Do(ctx, "PATCH", "/x", map[string]int{"a": 1}, nil)
		}, "PATCH", "/x", `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := fakeServer(t, "{}")
			if err := tc.call(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if rec.method != tc.method || rec.uri != tc.uri {
				t.Fatalf("request = %s %s, want %s %s", rec.method, rec.uri, tc.method, tc.uri)
			}
			if rec.auth != "Bearer tok" {
				t.Fatalf("auth = %q", rec.auth)
			}
			if tc.body != "" {
				var got, want any
				if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
					t.Fatalf("body %q: %v", rec.body, err)
				}
				_ = json.Unmarshal([]byte(tc.body), &want)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("body = %s, want %s", rec.body, tc.body)
				}
			}
		})
	}
}

func TestContentTypesAndDecoding(t *testing.T) {
	ctx := context.Background()
	c, rec := fakeServer(t, `{"valid":false,"name":"x","issues":[{"path":"steps","message":"bad"}]}`)
	res, err := c.Validate(ctx, "name: x")
	if err != nil || res.Valid || res.Name != "x" || len(res.Issues) != 1 {
		t.Fatalf("validate = %+v %v", res, err)
	}
	if rec.contentType != "application/x-yaml" || rec.body != "name: x" {
		t.Fatalf("yaml request: %+v", rec)
	}

	c, rec = fakeServer(t, "raw bytes")
	raw, err := c.StepLogs(ctx, "r", "s", 0, 0)
	if err != nil || string(raw) != "raw bytes" {
		t.Fatalf("raw = %q %v", raw, err)
	}
	if rec.contentType != "" {
		t.Fatalf("GET without body sent content type %q", rec.contentType)
	}

	c, _ = fakeServer(t, `{"workflows":[{"name":"a"},{"name":"b"}]}`)
	wfs, err := c.ListWorkflows(ctx)
	if err != nil || len(wfs) != 2 || wfs[1].Name != "b" {
		t.Fatalf("list = %+v %v", wfs, err)
	}

	// An empty 2xx body leaves out untouched.
	c, _ = fakeServer(t, "")
	if _, err := c.GetRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}

	c, _ = fakeServer(t, "not json")
	if _, err := c.GetRun(ctx, "r"); err == nil {
		t.Fatal("malformed JSON must fail")
	}
}

func TestNoTokenSendsNoAuthorization(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	c := NewWithHTTP(srv.URL+"/", "", srv.Client())
	if c.BaseURL != srv.URL {
		t.Fatalf("base URL not trimmed: %q", c.BaseURL)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "" {
		t.Fatalf("authorization = %q", got)
	}
}

func errorServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

func TestErrors(t *testing.T) {
	ctx := context.Background()

	err := errorServer(t, 404, `{"error":"workflow not found"}`).DeleteWorkflow(ctx, "w")
	if !IsNotFound(err) || err.Error() != "workflow not found (HTTP 404)" {
		t.Fatalf("404 = %v", err)
	}
	if IsNotFound(errors.New("x")) || IsNotFound(nil) {
		t.Fatal("IsNotFound on non-API errors")
	}

	// A body that is not an error document falls back to the status text.
	err = errorServer(t, 500, "boom").Health(ctx)
	var ae *Error
	if !errors.As(err, &ae) || ae.Msg != "Internal Server Error" || IsNotFound(err) {
		t.Fatalf("500 = %#v", err)
	}

	_, err = errorServer(t, 422, `{"error":"invalid workflow","issues":[{"path":"a","message":"m1"},{"path":"b","message":"m2"}]}`).ApplyWorkflow(ctx, "x")
	if !errors.As(err, &ae) || len(ae.Issues) != 2 {
		t.Fatalf("422 = %#v", err)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "invalid workflow:\n  ") || !strings.Contains(msg, "\n  ") || strings.Contains(msg, "; ") {
		t.Fatalf("issues message = %q", msg)
	}

	_, err = errorServer(t, 400, `{"error":"missing trigger fields","fields":["pr","repo"]}`).Run(ctx, "w", nil)
	if !errors.As(err, &ae) || strings.Join(ae.Fields, ",") != "pr,repo" {
		t.Fatalf("400 = %#v", err)
	}

	// Transport and request-building errors.
	dead := New("http://127.0.0.1:1", "")
	if err := dead.Health(ctx); err == nil {
		t.Fatal("unreachable server must fail")
	}
	bad := New("http://bad host", "")
	if err := bad.Health(ctx); err == nil {
		t.Fatal("invalid URL must fail")
	}
	if err := bad.Do(ctx, "GET", "/", func() {}, nil); err == nil {
		t.Fatal("unmarshalable body must fail")
	}
}

func TestReadBodyError(t *testing.T) {
	// The server promises more bytes than it sends, so reading fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer srv.Close()
	if err := New(srv.URL, "").Health(context.Background()); err == nil {
		t.Fatal("truncated body must fail")
	}
}

func sseServer(t *testing.T, check func(*http.Request), stream string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok")
}

func TestEventsStopsAtEnd(t *testing.T) {
	stream := "event: snapshot\ndata: {\"id\":\"r\"}\n\n" +
		": comment\n\n" +
		"event: step\ndata: {\"step\":\"a\"}\n\n" +
		"event: end\ndata: {\"status\":\"succeeded\"}\n\n" +
		"event: step\ndata: {}\n\n"
	var uri, accept, auth string
	c := sseServer(t, func(r *http.Request) {
		uri, accept, auth = r.URL.RequestURI(), r.Header.Get("Accept"), r.Header.Get("Authorization")
	}, stream)
	var got []string
	err := c.Events(context.Background(), "r", true, func(e Event) bool {
		got = append(got, e.Name+"="+string(e.Data))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `snapshot={"id":"r"} step={"step":"a"} end={"status":"succeeded"}`
	if strings.Join(got, " ") != want {
		t.Fatalf("events = %v", got)
	}
	if uri != "/api/v1/runs/r/events?logs=1" || accept != "text/event-stream" || auth != "Bearer tok" {
		t.Fatalf("request: %s %s %s", uri, accept, auth)
	}
}

func TestEventsCallbackStops(t *testing.T) {
	var uri string
	c := sseServer(t, func(r *http.Request) { uri = r.URL.RequestURI() }, "event: step\ndata: 1\n\nevent: step\ndata: 2\n\n")
	n := 0
	err := c.Events(context.Background(), "r", false, func(Event) bool { n++; return false })
	if err != nil || n != 1 || uri != "/api/v1/runs/r/events" {
		t.Fatalf("n=%d err=%v uri=%s", n, err, uri)
	}
}

func TestEventsStreamEndsWithoutEnd(t *testing.T) {
	// The server closes the stream early: no error unless ctx ended.
	c := sseServer(t, nil, "event: step\ndata: 1\n\n")
	if err := c.Events(context.Background(), "r", false, func(Event) bool { return true }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Events(ctx, "r", false, func(Event) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v", err)
	}
}

func TestEventsErrors(t *testing.T) {
	ctx := context.Background()
	err := errorServer(t, 404, `{"error":"run not found"}`).Events(ctx, "r", false, nil)
	if !IsNotFound(err) {
		t.Fatalf("404 = %v", err)
	}
	if err := New("http://bad host", "").Events(ctx, "r", false, nil); err == nil {
		t.Fatal("invalid URL must fail")
	}
	if err := New("http://127.0.0.1:1", "").Events(ctx, "r", false, nil); err == nil {
		t.Fatal("unreachable server must fail")
	}
	// A line over the scanner's 8 MiB cap is a stream error.
	big := "data: " + strings.Repeat("x", 9<<20) + "\n"
	if err := sseServer(t, nil, big).Events(ctx, "r", false, func(Event) bool { return true }); err == nil {
		t.Fatal("oversized line must fail")
	}
}

func TestWaitRun(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			_, _ = io.WriteString(w, `{"id":"r","status":"running"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r","status":"succeeded"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	run, err := c.WaitRun(context.Background(), "r", time.Millisecond)
	if err != nil || run.Status != "succeeded" || calls.Load() != 3 {
		t.Fatalf("wait = %+v %v calls=%d", run, err, calls.Load())
	}

	// Cancellation returns the last observed run.
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"r","status":"running"}`)
	}))
	defer stuck.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	run, err = New(stuck.URL, "").WaitRun(ctx, "r", 0) // 0 means the 1s default
	if !errors.Is(err, context.DeadlineExceeded) || run == nil || run.Status != "running" {
		t.Fatalf("cancelled wait = %+v %v", run, err)
	}

	if _, err := errorServer(t, 404, `{"error":"no"}`).WaitRun(context.Background(), "r", time.Millisecond); !IsNotFound(err) {
		t.Fatalf("404 wait = %v", err)
	}
}
