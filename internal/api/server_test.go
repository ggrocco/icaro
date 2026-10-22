package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/workflow"
)

func TestWriteErrorMapping(t *testing.T) {
	if New(nil, nil).log == nil {
		t.Fatal("no default logger")
	}
	s := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := workflow.Issues{{Path: "/steps", Message: "required"}}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		msg    string
	}{
		{"missing trigger fields", &service.MissingTriggerFieldsError{Fields: []string{"who"}}, 400, "missing trigger field(s): who"},
		{"conflict type", &service.Conflict{Msg: "already running"}, 409, "already running"},
		{"conflict sentinel", fmt.Errorf("wrap: %w", service.ErrConflict), 409, "wrap: conflict"},
		{"validation", &service.ValidationError{Issues: issues}, 422, "validation failed"},
		{"bad request", &service.BadRequest{Msg: "nope"}, 400, "nope"},
		{"not found", fmt.Errorf("%w: run x", service.ErrNotFound), 404, "not found: run x"},
		{"unauthorized", service.ErrUnauthorized, 401, "unauthorized"},
		{"forbidden", service.ErrForbidden, 403, "forbidden"},
		// Anything unclassified is a 500 that never leaks its message.
		{"internal", errors.New("sql: database is closed"), 500, "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.WriteError(rec, tc.err)
			body := errBody(t, rec.Body.String())
			if rec.Code != tc.status || body.Error != tc.msg {
				t.Fatalf("%d %+v", rec.Code, body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
			if tc.status == 422 && len(body.Issues) != 1 {
				t.Fatalf("issues %+v", body.Issues)
			}
			if tc.status == 400 && strings.HasPrefix(tc.msg, "missing") && len(body.Fields) != 1 {
				t.Fatalf("fields %+v", body.Fields)
			}
		})
	}
}

func TestReadJSONAndYAML(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	req := func(ct, body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", ct)
		return r
	}
	if err := ReadJSON(req("application/json", ""), &v); err != nil {
		t.Fatalf("an empty body is allowed: %v", err)
	}
	for _, body := range []string{`{"a":`, `{"b":1}`, `[1]`} {
		var br *service.BadRequest
		if err := ReadJSON(req("application/json", body), &v); !errors.As(err, &br) || !strings.HasPrefix(br.Msg, "invalid JSON body") {
			t.Fatalf("%s: %v", body, err)
		}
	}

	for _, tc := range []struct {
		ct, body, want, err string
	}{
		{"text/yaml", "name: x\n", "name: x\n", ""},
		{"application/json", `{"yaml":"name: y"}`, "name: y", ""},
		{"application/json; charset=utf-8", `{"yaml":`, "", "invalid JSON body"},
		{"application/json", `{"other":"x"}`, "", `body must be {"yaml": "..."} or raw YAML`},
		{"text/plain", " \n\t", "", "empty body"},
	} {
		got, err := readYAML(req(tc.ct, tc.body))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("%q: %v", tc.body, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.body, got, err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(errBoom))
	if _, err := readYAML(r); !errors.Is(err, errBoom) {
		t.Fatalf("read error: %v", err)
	}
}

func TestQueryInt(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?n=7&bad=x", nil)
	if queryInt(r, "n", 1) != 7 || queryInt(r, "bad", 3) != 3 || queryInt(r, "missing", 5) != 5 {
		t.Fatal("queryInt")
	}
}

func TestRecovererAndResponseController(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	f.srv.Mux().HandleFunc("GET /test/panic", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	f.srv.Mux().HandleFunc("GET /test/deadline", func(w http.ResponseWriter, _ *http.Request) {
		// http.ResponseController reaches the connection through the
		// logging wrapper's Unwrap.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	code, body := f.call("GET", "/test/panic", nil)
	if code != 500 || errBody(t, body).Error != "internal error" || strings.Contains(body, "kaboom") {
		t.Fatalf("panic: %d %s", code, body)
	}
	if code, body := f.call("GET", "/test/deadline", nil); code != http.StatusNoContent {
		t.Fatalf("deadline: %d %s", code, body)
	}
}

func TestConnContextMarksOnlyUnixSockets(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if IsLocal(ConnContext(context.Background(), a)) {
		t.Fatal("a non-Unix connection is local")
	}
	if TokenFrom(context.Background()) != nil {
		t.Fatal("token without auth")
	}
}

func TestLocalSocketIsTheAdminPrincipal(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	// No token: the socket alone authenticates, at admin scope.
	code, body := f.call("GET", "/api/v1/ui/me", nil)
	if code != 200 || !strings.Contains(body, `"name": "local"`) || !strings.Contains(body, `"scope": "admin"`) {
		t.Fatalf("me: %d %s", code, body)
	}
	if code, body := f.call("PUT", "/api/v1/secrets/gh", map[string]any{"value": "ghp_123456"}); code != http.StatusNoContent {
		t.Fatalf("admin route: %d %s", code, body)
	}
	// The same route over TCP needs a token.
	code, _ = doReq(t, f.tcp.Client(), "GET", f.tcp.URL+"/api/v1/secrets", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("tcp without token: %d", code)
	}
	// Runs started over the socket are attributed to the local principal.
	if code, body := f.call("POST", "/api/v1/workflows", wfYAML); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	if code, body := f.call("POST", "/api/v1/workflows", wfYAML); code != http.StatusOK || !strings.Contains(body, `"version": 1`) {
		t.Fatalf("re-apply unchanged: %d %s", code, body)
	}
	code, body = f.call("POST", "/api/v1/workflows/hello/runs", map[string]any{"trigger": map[string]any{"who": "me"}})
	if code != http.StatusAccepted || !strings.Contains(body, `"actor": "token:local"`) {
		t.Fatalf("run: %d %s", code, body)
	}
}

func TestCookieAuthEdges(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	ctx := context.Background()
	plain, hash, _ := crypto.NewToken()
	if _, err := f.st.CreateToken(ctx, "viewer", hash, store.ScopeRead); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := f.svc.UILogin(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	c := f.tcp.Client()
	withCookie := func(v string, csrf bool) http.Header {
		h := http.Header{"Cookie": {SessionCookie + "=" + v}}
		if csrf {
			h.Set(CSRFHeader, "1")
		}
		return h
	}
	url := f.tcp.URL + "/api/v1/workflows"
	if code, _ := doReq(t, c, "GET", url, nil, withCookie(cookie, false)); code != 200 {
		t.Fatalf("read session read: %d", code)
	}
	// A read session cannot mutate even with the CSRF header.
	if code, _ := doReq(t, c, "POST", url, wfYAML, withCookie(cookie, true)); code != http.StatusForbidden {
		t.Fatalf("read session write: %d", code)
	}
	// An unknown or expired session is unauthenticated.
	if code, _ := doReq(t, c, "GET", url, nil, withCookie("bogus", false)); code != http.StatusUnauthorized {
		t.Fatalf("bogus cookie: %d", code)
	}
	// A bearer token wins over a cookie and is checked on its own.
	h := withCookie(cookie, false)
	h.Set("Authorization", "Bearer "+plain)
	if code, _ := doReq(t, c, "POST", url, wfYAML, h); code != http.StatusForbidden {
		t.Fatalf("bearer read token write: %d", code)
	}
	code, body := doReq(t, c, "GET", url, nil, nil)
	if code != http.StatusUnauthorized || !strings.Contains(body, "unauthorized") {
		t.Fatalf("anonymous: %d %s", code, body)
	}
}

func TestHealthReportsDatabase(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	if code, body := f.call("GET", "/healthz", nil); code != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("%d %s", code, body)
	}
	_ = f.st.Close()
	if code, body := f.call("GET", "/healthz", nil); code != http.StatusServiceUnavailable || !strings.Contains(body, "db unavailable") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestWorkflowHandlerInputErrors(t *testing.T) {
	t.Parallel()
	f := newFull(t)
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"POST", "/api/v1/workflows", " ", 400},
		{"POST", "/api/v1/workflows/validate", " ", 400},
		{"GET", "/api/v1/workflows/hello/versions/one", nil, 400},
		{"POST", "/api/v1/workflows/hello/runs", []byte(`{"trigger":`), 400},
		{"POST", "/api/v1/workflows/missing/enable", nil, 404},
		{"GET", "/api/v1/workflows/missing/versions", nil, 404},
		{"GET", "/api/v1/workflows/missing/versions/1", nil, 404},
		{"GET", "/api/v1/runs/missing", nil, 404},
		{"POST", "/api/v1/runs/missing/cancel", nil, 404},
		{"GET", "/api/v1/runs/missing/steps/x/logs", nil, 404},
	} {
		if code, body := f.call(tc.method, tc.path, tc.body); code != tc.status {
			t.Errorf("%s %s = %d %s", tc.method, tc.path, code, body)
		}
	}
	// A valid spec reports its name; ?workflow= filters runs.
	if code, body := f.call("POST", "/api/v1/workflows", wfYAML); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := f.call("POST", "/api/v1/workflows/validate", wfYAML)
	if code != 200 || !strings.Contains(body, `"name": "hello"`) || !strings.Contains(body, `"issues": []`) {
		t.Fatalf("validate: %d %s", code, body)
	}
	if code, body := f.call("GET", "/api/v1/runs?workflow=hello&status=queued", nil); code != 200 || !strings.Contains(body, `"runs": []`) {
		t.Fatalf("runs: %d %s", code, body)
	}
}
