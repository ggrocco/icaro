package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/crypto"
	"icaro/internal/hosts"
	"icaro/internal/service"
	"icaro/internal/store"
)

// fakeRemoteDaemon answers the two calls a sign-in and a listing make.
func fakeRemoteDaemon(t *testing.T, token string, workflows ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ui/me":
			_, _ = io.WriteString(w, `{"name":"remote-user","scope":"read"}`)
		case "/api/v1/workflows":
			out := []*service.WorkflowInfo{}
			for _, n := range workflows {
				out = append(out, &service.WorkflowInfo{Name: n, Version: 3})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": out})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHostsEnv(t *testing.T) (*env, *apiclient.Client) {
	t.Helper()
	e := newEnv(t)
	key, _ := crypto.NewKey()
	s := New(e.svc, quietLog())
	RegisterHosts(s, hosts.New(hosts.Options{Store: e.st, Key: key, Local: e.svc.ListWorkflows, LocalLabel: "laptop", Log: quietLog()}))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	mk := func(name, scope string) *apiclient.Client {
		plain, hash, _ := crypto.NewToken()
		if _, err := e.st.CreateToken(context.Background(), name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(srv.URL, plain)
	}
	e.admin, e.reader = mk("admin2", store.ScopeAdmin), mk("reader2", store.ScopeRead)
	return e, mk("writer2", store.ScopeWrite)
}

func TestHostRoutes(t *testing.T) {
	ctx := context.Background()
	e, writer := newHostsEnv(t)
	if _, err := e.admin.ApplyWorkflow(ctx, wfYAML); err != nil {
		t.Fatal(err)
	}
	prod := fakeRemoteDaemon(t, "tok-prod", "deploy")
	lab := fakeRemoteDaemon(t, "tok-lab", "build", "lint")

	// Signing in stores another daemon's token: admin only.
	if _, err := writer.LoginHost(ctx, hosts.Login{Alias: "prod", URL: prod.URL, Token: "tok-prod"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("write-scope login: %v", err)
	}
	h, err := e.admin.LoginHost(ctx, hosts.Login{Alias: "prod", Label: "Production", URL: prod.URL, Token: "tok-prod"})
	if err != nil || h.Principal != "remote-user" || h.Scope != "read" || h.Label != "Production" {
		t.Fatalf("login: %+v %v", h, err)
	}
	if _, err := e.admin.LoginHost(ctx, hosts.Login{Alias: "lab", Label: "Lab", URL: lab.URL, Token: "tok-lab"}); err != nil {
		t.Fatal(err)
	}

	list, err := e.reader.ListHosts(ctx)
	if err != nil || len(list) != 3 || list[0].Alias != "local" || list[0].Label != "laptop" || list[1].Alias != "lab" {
		t.Fatalf("hosts: %+v %v", list, err)
	}
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "tok-") {
		t.Fatalf("a host listing carries a token: %s", b)
	}

	all, err := e.reader.HostWorkflows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, w := range all.Workflows {
		refs = append(refs, w.Ref+"@"+w.HostLabel)
	}
	if got := strings.Join(refs, " "); got != "local:hello@laptop lab:build@Lab lab:lint@Lab prod:deploy@Production" {
		t.Fatalf("refs = %s", got)
	}
	one, err := e.reader.HostWorkflows(ctx, "prod")
	if err != nil || len(one.Workflows) != 1 || one.Workflows[0].UIURL != prod.URL+"/ui/#/workflows/deploy" {
		t.Fatalf("prod only: %+v %v", one, err)
	}

	for _, tc := range []struct {
		call func() error
		want string
	}{
		{func() error { _, err := e.reader.HostWorkflows(ctx, "nope"); return err }, `not found: no host "nope" is signed in (HTTP 404)`},
		{func() error {
			_, err := e.admin.LoginHost(ctx, hosts.Login{Alias: "Bad", URL: prod.URL, Token: "x"})
			return err
		}, "must match"},
		{func() error {
			_, err := e.admin.LoginHost(ctx, hosts.Login{Alias: "prod", URL: prod.URL, Token: "wrong"})
			return err
		}, "refused the token: unauthorized (HTTP 400)"},
		{func() error {
			_, err := e.admin.LoginHost(ctx, hosts.Login{Alias: "gone", URL: "http://127.0.0.1:1", Token: "x"})
			return err
		}, "502"},
		{func() error {
			return e.admin.Do(ctx, "POST", "/api/v1/hosts", map[string]any{"alias": "x", "extra": 1}, nil)
		}, "400"},
		{func() error { return writer.LogoutHost(ctx, "prod") }, "403"},
		{func() error { return e.admin.LogoutHost(ctx, "nope") }, "404"},
		{func() error { return e.admin.LogoutHost(ctx, "local") }, "no sign-in"},
	} {
		if err := tc.call(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}

	if err := e.admin.LogoutHost(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if list, _ = e.reader.ListHosts(ctx); len(list) != 2 {
		t.Fatalf("after logout: %+v", list)
	}

}

// A store failure is an opaque 500.
func TestHostRouteStoreFailure(t *testing.T) {
	e := newEnv(t)
	key, _ := crypto.NewKey()
	reg := hosts.New(hosts.Options{Store: e.st, Key: key, Local: e.svc.ListWorkflows, Log: quietLog()})
	h := &hostHandlers{s: New(e.svc, quietLog()), reg: reg}
	_ = e.st.Close()
	for name, fn := range map[string]http.HandlerFunc{"list": h.list, "workflows": h.workflows} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest("GET", "/api/v1/hosts", nil))
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal error") {
			t.Errorf("%s on a closed store = %d %s", name, rec.Code, rec.Body)
		}
	}
}
