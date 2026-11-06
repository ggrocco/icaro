package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func newKey(t *testing.T) *crypto.Key {
	t.Helper()
	k, err := crypto.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// remote is a fake Ícaro daemon: the two endpoints the registry calls,
// behind a bearer token.
type remote struct {
	token     string
	workflows []string
	// hold, when set, runs before every workflow listing answers.
	hold func()
	srv  *httptest.Server
}

func newRemote(t *testing.T, token string, workflows ...string) *remote {
	t.Helper()
	r := &remote{token: token, workflows: workflows}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+r.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch req.URL.Path {
		case "/api/v1/ui/me":
			_, _ = io.WriteString(w, `{"name":"ci","scope":"write"}`)
		case "/api/v1/workflows":
			if r.hold != nil {
				r.hold()
			}
			out := []*service.WorkflowInfo{}
			for _, n := range r.workflows {
				out = append(out, &service.WorkflowInfo{Name: n, Version: 1, Status: "active"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflows": out})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func newRegistry(t *testing.T, local ...string) (*Registry, *store.Store) {
	t.Helper()
	st := openStore(t)
	reg := New(Options{
		Store: st, Key: newKey(t), LocalLabel: "laptop", Log: quiet, Timeout: 2 * time.Second,
		Local: func(context.Context) ([]*service.WorkflowInfo, error) {
			out := []*service.WorkflowInfo{}
			for _, n := range local {
				out = append(out, &service.WorkflowInfo{Name: n})
			}
			return out, nil
		},
	})
	return reg, st
}

func TestLoginListLogout(t *testing.T) {
	ctx := context.Background()
	reg, _ := newRegistry(t)
	prod := newRemote(t, "tok-prod")
	lab := newRemote(t, "tok-lab")

	h, err := reg.Login(ctx, Login{Alias: "prod", Label: " Production ", URL: prod.srv.URL + "/", Token: " tok-prod\n"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Alias != "prod" || h.Label != "Production" || h.URL != prod.srv.URL || h.Principal != "ci" || h.Scope != "write" || h.SignedInAt == nil || h.Local {
		t.Fatalf("login: %+v", h)
	}
	// Many hosts can be signed in at once; the label defaults to the alias.
	if h, err = reg.Login(ctx, Login{Alias: "lab", URL: lab.srv.URL, Token: "tok-lab"}); err != nil || h.Label != "lab" {
		t.Fatalf("second login: %+v %v", h, err)
	}

	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var aliases []string
	for _, h := range list {
		aliases = append(aliases, h.Alias)
	}
	if strings.Join(aliases, ",") != "local,lab,prod" || !list[0].Local || list[0].Label != "laptop" || list[0].URL != "" {
		t.Fatalf("list: %+v", list)
	}

	if err := reg.Logout(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Logout(ctx, "prod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("logout twice: %v", err)
	}
	if err := reg.Logout(ctx, LocalAlias); !errors.Is(err, ErrInvalid) {
		t.Fatalf("logout local: %v", err)
	}
	if list, _ = reg.List(ctx); len(list) != 2 || list[1].Alias != "lab" {
		t.Fatalf("after logout: %+v", list)
	}
}

func TestLoginRefusals(t *testing.T) {
	ctx := context.Background()
	reg, st := newRegistry(t)
	good := newRemote(t, "tok")
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer broken.Close()
	notIcaro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer notIcaro.Close()
	redirect := httptest.NewServer(http.RedirectHandler(good.srv.URL+"/api/v1/ui/me", http.StatusFound))
	defer redirect.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	for _, tc := range []struct {
		name string
		in   Login
		want error
		msg  string
	}{
		{"bad alias", Login{Alias: "Prod", URL: good.srv.URL, Token: "tok"}, ErrInvalid, "must match"},
		{"alias with colon", Login{Alias: "a:b", URL: good.srv.URL, Token: "tok"}, ErrInvalid, "must match"},
		{"local alias", Login{Alias: "local", URL: good.srv.URL, Token: "tok"}, ErrInvalid, "needs no sign-in"},
		{"long label", Login{Alias: "a", Label: strings.Repeat("x", 101), URL: good.srv.URL, Token: "tok"}, ErrInvalid, "label"},
		{"no token", Login{Alias: "a", URL: good.srv.URL, Token: "  "}, ErrInvalid, "token is required"},
		{"no scheme", Login{Alias: "a", URL: "example.com", Token: "tok"}, ErrInvalid, "http://"},
		{"ftp", Login{Alias: "a", URL: "ftp://example.com", Token: "tok"}, ErrInvalid, "http://"},
		{"no host", Login{Alias: "a", URL: "http://", Token: "tok"}, ErrInvalid, "no host"},
		{"credentials", Login{Alias: "a", URL: "https://u:p@example.com", Token: "tok"}, ErrInvalid, "credentials"}, //nolint:gosec // a URL the sign-in must refuse
		{"query", Login{Alias: "a", URL: "https://example.com/?x=1", Token: "tok"}, ErrInvalid, "query"},
		{"unparsable", Login{Alias: "a", URL: "http://[::1", Token: "tok"}, ErrInvalid, "url"},
		{"wrong token", Login{Alias: "a", URL: good.srv.URL, Token: "nope"}, ErrRefused, "refused the token"},
		{"server error", Login{Alias: "a", URL: broken.URL, Token: "tok"}, ErrUnreachable, "boom"},
		{"not icaro", Login{Alias: "a", URL: notIcaro.URL, Token: "tok"}, ErrUnreachable, "did not answer as an Ícaro daemon"},
		{"redirect", Login{Alias: "a", URL: redirect.URL, Token: "tok"}, ErrUnreachable, "redirect"},
		{"down", Login{Alias: "a", URL: gone.URL, Token: "tok"}, ErrUnreachable, gone.URL},
	} {
		_, err := reg.Login(ctx, tc.in)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want %v containing %q", tc.name, err, tc.want, tc.msg)
		}
	}
	if rows, _ := st.ListRemoteHosts(ctx); len(rows) != 0 {
		t.Fatalf("a refused sign-in stored a host: %+v", rows)
	}
}

func TestWorkflowsFromAllHosts(t *testing.T) {
	ctx := context.Background()
	reg, _ := newRegistry(t, "zeta", "alpha")
	prod := newRemote(t, "tok-prod", "deploy", "build")
	lab := newRemote(t, "tok-lab", "build")
	for alias, r := range map[string]*remote{"prod": prod, "lab": lab} {
		if _, err := reg.Login(ctx, Login{Alias: alias, Label: strings.ToUpper(alias), URL: r.srv.URL, Token: r.token}); err != nil {
			t.Fatal(err)
		}
	}

	out, err := reg.Workflows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, w := range out.Workflows {
		refs = append(refs, w.Ref)
	}
	// Local first, then remote hosts by alias; each host's by name.
	if got := strings.Join(refs, " "); got != "local:alpha local:zeta lab:build prod:build prod:deploy" {
		t.Fatalf("refs = %s", got)
	}
	first, last := out.Workflows[0], out.Workflows[4]
	if first.Host != "local" || first.HostLabel != "laptop" || first.UIURL != "" || first.Name != "alpha" {
		t.Fatalf("local workflow: %+v", first)
	}
	if last.Host != "prod" || last.HostLabel != "PROD" || last.UIURL != prod.srv.URL+"/ui/#/workflows/deploy" || last.Version != 1 {
		t.Fatalf("remote workflow: %+v", last)
	}
	if len(out.Hosts) != 3 || out.Hosts[0].Workflows != 2 || out.Hosts[1].Workflows != 1 || out.Hosts[2].Workflows != 2 {
		t.Fatalf("hosts: %+v", out.Hosts)
	}

	// The JSON flattens the workflow next to its host reference.
	b, _ := json.Marshal(last)
	for _, want := range []string{`"name":"deploy"`, `"host":"prod"`, `"host_label":"PROD"`, `"ref":"prod:deploy"`, `"ui_url":`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json %s lacks %s", b, want)
		}
	}

	// One host only.
	one, err := reg.Workflows(ctx, "lab")
	if err != nil || len(one.Workflows) != 1 || one.Workflows[0].Ref != "lab:build" || len(one.Hosts) != 1 {
		t.Fatalf("lab only: %+v %v", one, err)
	}
	if one, err = reg.Workflows(ctx, LocalAlias); err != nil || len(one.Workflows) != 2 || len(one.Hosts) != 1 {
		t.Fatalf("local only: %+v %v", one, err)
	}
	if _, err := reg.Workflows(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
}

// Every host is asked at the same time: each fake remote answers only once
// all of them have been asked, which a one-by-one listing never reaches.
func TestWorkflowsAsksHostsInParallel(t *testing.T) {
	ctx := context.Background()
	reg, _ := newRegistry(t)
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	hold := func() {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}
	for _, alias := range []string{"a", "b", "c"} {
		r := newRemote(t, "tok", "w-"+alias)
		r.hold = hold
		if _, err := reg.Login(ctx, Login{Alias: alias, URL: r.srv.URL, Token: "tok"}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for range 3 {
			<-arrived
		}
		close(release)
	}()
	start := time.Now()
	out, err := reg.Workflows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range out.Hosts {
		if h.Error != "" {
			t.Fatalf("host %s: %s", h.Alias, h.Error)
		}
	}
	if len(out.Workflows) != 3 || time.Since(start) > 4*time.Second {
		t.Fatalf("parallel listing: %d workflows in %s", len(out.Workflows), time.Since(start))
	}
}

// A host that fails or is too slow is reported and hides no other host.
func TestWorkflowsReportsFailingHosts(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	key := newKey(t)
	reg := New(Options{
		Store: st, Key: key, LocalLabel: "laptop", Log: quiet, Timeout: 200 * time.Millisecond,
		Local: func(context.Context) ([]*service.WorkflowInfo, error) { return nil, errors.New("local store down") },
	})
	ok := newRemote(t, "tok", "fine")
	slow := newRemote(t, "tok", "late")
	stuck := make(chan struct{})
	defer close(stuck)
	slow.hold = func() {
		select {
		case <-stuck:
		case <-time.After(5 * time.Second):
		}
	}
	revoked := newRemote(t, "tok", "x")
	for alias, r := range map[string]*remote{"ok": ok, "slow": slow, "revoked": revoked} {
		if _, err := reg.Login(ctx, Login{Alias: alias, URL: r.srv.URL, Token: "tok"}); err != nil {
			t.Fatal(err)
		}
	}
	revoked.token = "rotated"
	// A token sealed with another key cannot be opened.
	other := newKey(t)
	ct, _ := other.Seal([]byte("tok"))
	if err := st.PutRemoteHost(ctx, &store.RemoteHostRow{Alias: "sealed", Label: "s", URL: ok.srv.URL, TokenCT: ct}); err != nil {
		t.Fatal(err)
	}

	out, err := reg.Workflows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Workflows) != 1 || out.Workflows[0].Ref != "ok:fine" {
		t.Fatalf("workflows: %+v", out.Workflows)
	}
	errs := map[string]string{}
	for _, h := range out.Hosts {
		errs[h.Alias] = h.Error
	}
	for alias, want := range map[string]string{
		"local": "local store down", "ok": "", "revoked": "refused the token", "slow": "deadline", "sealed": "sign in again",
	} {
		if got, seen := errs[alias]; !seen || !strings.Contains(got, want) || (want == "" && got != "") {
			t.Errorf("host %s error = %q, want %q", alias, got, want)
		}
	}
}

func TestStoreFailures(t *testing.T) {
	ctx := context.Background()
	reg, st := newRegistry(t)
	r := newRemote(t, "tok")
	_ = st.Close()
	if _, err := reg.List(ctx); err == nil {
		t.Error("List on a closed store")
	}
	if _, err := reg.Workflows(ctx, ""); err == nil {
		t.Error("Workflows on a closed store")
	}
	if _, err := reg.Login(ctx, Login{Alias: "a", URL: r.srv.URL, Token: "tok"}); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("Login on a closed store: %v", err)
	}
	if err := reg.Logout(ctx, "a"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Logout on a closed store: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	reg := New(Options{})
	want, _ := os.Hostname()
	if want == "" {
		want = "This machine"
	}
	if reg.localLabel != want || reg.timeout != DefaultTimeout || reg.log == nil || reg.dial == nil {
		t.Fatalf("defaults: %+v", reg)
	}
	if !ValidAlias("prod-2") || ValidAlias("2prod") || ValidAlias(strings.Repeat("a", 33)) {
		t.Fatal("ValidAlias")
	}
	if u, err := NormalizeURL(" https://icaro.example.com/base/ "); err != nil || u != "https://icaro.example.com/base" {
		t.Fatalf("NormalizeURL = %q %v", u, err)
	}
}
