package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
)

type uiEnv struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *service.Service
	token string
}

func newUIEnv(t *testing.T) *uiEnv {
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
	s := New(svc, quiet)
	s.RegisterUI(UIOptions{Assets: fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>Ícaro</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}, Open: func() (string, error) { return "http://127.0.0.1:1", nil }})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	plain, hash, _ := crypto.NewToken()
	if _, err := st.CreateToken(ctx, "ui-user", hash, store.ScopeWrite); err != nil {
		t.Fatal(err)
	}
	return &uiEnv{t: t, srv: srv, svc: svc, token: plain}
}

func (e *uiEnv) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *uiEnv) do(c *http.Client, method, path, body string, csrf bool) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(CSRFHeader, "1")
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestUICookieSessionAndCSRF(t *testing.T) {
	e := newUIEnv(t)
	b := e.browser()

	if code, _ := e.do(b, "GET", "/api/v1/ui/me", "", false); code != http.StatusUnauthorized {
		t.Fatalf("me before login = %d", code)
	}
	if code, _ := e.do(b, "POST", "/api/v1/ui/login", `{"token":"`+e.token+`"}`, false); code != http.StatusForbidden {
		t.Fatalf("login without CSRF header = %d", code)
	}
	if code, _ := e.do(b, "POST", "/api/v1/ui/login", `{"token":"nope"}`, true); code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d", code)
	}
	code, body := e.do(b, "POST", "/api/v1/ui/login", `{"token":"`+e.token+`"}`, true)
	if code != http.StatusOK || !strings.Contains(body, `"ui-user"`) {
		t.Fatalf("login = %d %s", code, body)
	}
	if code, body := e.do(b, "GET", "/api/v1/ui/me", "", false); code != http.StatusOK || !strings.Contains(body, "write") {
		t.Fatalf("me = %d %s", code, body)
	}
	// Reads work with the cookie alone; mutations need the CSRF header.
	if code, _ := e.do(b, "GET", "/api/v1/workflows", "", false); code != http.StatusOK {
		t.Fatalf("cookie read = %d", code)
	}
	yaml := `{"yaml":"version: 1\nname: ui-wf\nsteps:\n  - id: say\n    image: alpine:3.20\n    command: echo hi\n"}`
	if code, _ := e.do(b, "POST", "/api/v1/workflows", yaml, false); code != http.StatusForbidden {
		t.Fatalf("cookie mutation without CSRF = %d", code)
	}
	if code, body := e.do(b, "POST", "/api/v1/workflows", yaml, true); code != http.StatusCreated {
		t.Fatalf("cookie mutation with CSRF = %d %s", code, body)
	}
	// Scope still applies: a write session cannot use admin routes.
	if code, _ := e.do(b, "POST", "/api/v1/secrets", `{"name":"x","value":"yyyyyy"}`, true); code != http.StatusForbidden && code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("admin route with write session = %d", code)
	}
	if code, _ := e.do(b, "POST", "/api/v1/ui/logout", "", true); code != http.StatusNoContent {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := e.do(b, "GET", "/api/v1/ui/me", "", false); code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d", code)
	}
}

func TestUILoginLinkIsLocalAndSingleUse(t *testing.T) {
	e := newUIEnv(t)
	if code, _ := e.do(http.DefaultClient, "POST", "/api/v1/ui/link", "", true); code != http.StatusForbidden {
		t.Fatalf("link over TCP = %d", code)
	}
	codeStr, err := e.svc.NewUILoginCode(context.Background(), "local", store.ScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	if code, _ := e.do(b, "POST", "/api/v1/ui/login", `{"token":"`+codeStr+`"}`, true); code != http.StatusOK {
		t.Fatalf("code login = %d", code)
	}
	if code, _ := e.do(e.browser(), "POST", "/api/v1/ui/login", `{"token":"`+codeStr+`"}`, true); code != http.StatusUnauthorized {
		t.Fatalf("code reuse = %d", code)
	}
}

func TestUIServesShellAndAssets(t *testing.T) {
	e := newUIEnv(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/")
	if err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("root redirect: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	for path, want := range map[string]string{"/ui/": "<title>Ícaro", "/ui/runs/x": "<title>Ícaro", "/ui/assets/app.js": "console.log"} {
		code, body := e.do(http.DefaultClient, "GET", path, "", false)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("%s = %d %q", path, code, body)
		}
	}
	resp, _ = http.Get(e.srv.URL + "/ui/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp = %q", csp)
	}
	_ = resp.Body.Close()
}
