package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/secrets"
	"icaro/internal/store"
)

const actManifest = `version: 1
name: acme
hosts: [api.acme.dev]
config:
  base_url: { default: "https://api.acme.dev" }
auth:
  token: { kind: bearer, credential: token }
  key: { kind: query, param: api_key, credential: key }
actions:
  close-ticket:
    inputs:
      ticket: { type: number, required: true }
      note: { type: string, default: "closed by icaro" }
      dry: { type: boolean, default: false }
    request:
      method: POST
      url: "${{ config.base_url }}/tickets/${{ input.ticket }}/close"
      auth: token
      body:
        json: { note: "${{ input.note }}", dry: "${{ input.dry }}" }
    outputs:
      status: "${{ response.body.status }}"
      echo: "${{ response.body.echo }}"
  search:
    inputs:
      q: { type: string, required: true }
    request:
      method: GET
      url: "https://api.acme.dev/search"
      query: { q: "${{ input.q }}" }
      auth: key
    success: "response.body.ok == true"
    outputs:
      count: "${{ response.body.count }}"
`

type fakeResolver map[string]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ip, ok := f[host]
	if !ok {
		return nil, errors.New("no such host " + host)
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

type execFixture struct {
	exec     *Executor
	secrets  *secrets.Store
	hits     *atomic.Int32
	last     chan *http.Request
	respond  func(w http.ResponseWriter, r *http.Request)
	settings map[string]config.IntegrationConfig
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p", "acme.yaml"), []byte(actManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir, nil)
	if errs := reg.Snapshot().Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	sec := secrets.New(db, key)

	f := &execFixture{secrets: sec, hits: &atomic.Int32{}, last: make(chan *http.Request, 10)}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.last <- r.Clone(context.Background())
		f.respond(w, r)
	}))
	t.Cleanup(srv.Close)
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = "example.com"
	settings := map[string]config.IntegrationConfig{}
	f.settings = settings
	f.exec = &Executor{
		Registry: reg,
		Secrets:  sec,
		Settings: func(n string) config.IntegrationConfig { return settings[n] },
		Resolver: fakeResolver{"api.acme.dev": "93.184.216.34", "evil.acme.dev": "10.0.0.5", "ghe.corp.example": "10.1.1.1"},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSConfig: tlsCfg,
	}
	return f
}

func TestExecuteAttachesBoundCredentialAndRedacts(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	const token = "acme-token-123456789"
	if err := f.secrets.Set(ctx, "acme.token", token, []string{"api.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "closed", "echo": "you sent " + r.Header.Get("Authorization") + " and ghp_abcdefghijklmnopqrstuvwxyz0123"})
	}
	out, err := f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": "42"})
	if err != nil {
		t.Fatal(err)
	}
	req := <-f.last
	if req.Header.Get("Authorization") != "Bearer "+token || req.URL.Path != "/tickets/42/close" || req.Host != "api.acme.dev" {
		t.Fatalf("request: %v %v %v", req.Header, req.URL, req.Host)
	}
	if out["status"] != "closed" {
		t.Fatalf("%v", out)
	}
	echo := out["echo"].(string)
	if strings.Contains(echo, token) || strings.Contains(echo, "ghp_") || !strings.Contains(echo, "***") {
		t.Fatalf("secret leaked into outputs: %q", echo)
	}

	// Error responses are redacted too.
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"bad token ` + token + `"}`))
	}
	_, err = f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": 1})
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Status != 403 || strings.Contains(err.Error(), token) {
		t.Fatalf("error not redacted: %v", err)
	}
}

func TestExecuteRefusesCredentialOutsideBinding(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	if err := f.secrets.Set(ctx, "acme.token", "acme-token-123456789", []string{"other.example"}); err != nil {
		t.Fatal(err)
	}
	f.respond = func(w http.ResponseWriter, r *http.Request) { t.Error("request must not be sent") }
	_, err := f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": 1})
	if err == nil || !strings.Contains(err.Error(), "outside the secret's binding") || f.hits.Load() != 0 {
		t.Fatalf("binding not enforced: %v (hits %d)", err, f.hits.Load())
	}
	// A missing credential names the command that adds it.
	_ = f.secrets.Delete(ctx, "acme.token")
	_, err = f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": 1})
	if err == nil || !strings.Contains(err.Error(), "icaro credential add acme token") {
		t.Fatal(err)
	}
}

func TestExecuteSSRFGuard(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	f.respond = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"ok","echo":""}`)) }
	if err := f.secrets.Set(ctx, "acme.token", "acme-token-123456789", []string{"api.acme.dev", "ghe.corp.example"}); err != nil {
		t.Fatal(err)
	}
	// The user points base_url at a self-hosted instance in private space.
	f.settings["acme"] = config.IntegrationConfig{BaseURL: "https://ghe.corp.example/api"}
	_, err := f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": 1})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("private range must be refused without opt-in: %v", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("guard must stop the dial")
	}
	f.settings["acme"] = config.IntegrationConfig{BaseURL: "https://ghe.corp.example/api", AllowPrivate: true}
	if _, err := f.exec.Execute(ctx, "acme.close-ticket", map[string]any{"ticket": 1}); err != nil {
		t.Fatalf("opt-in: %v", err)
	}
	if r := <-f.last; r.URL.Path != "/api/tickets/1/close" {
		t.Fatal(r.URL.Path)
	}
	for _, ip := range []string{"127.0.0.1", "10.2.3.4", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "fe80::1", "0.0.0.0", "::ffff:127.0.0.1"} {
		if !IsBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s must be blocked", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "2606:4700::1111"} {
		if IsBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s must be allowed", ip)
		}
	}
}

func TestExecuteQueryAuthSuccessAndInputs(t *testing.T) {
	f := newExecFixture(t)
	ctx := context.Background()
	_ = f.secrets.Set(ctx, "acme.key", "query-key-abcdef", []string{"api.acme.dev"})
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		ok := r.URL.Query().Get("api_key") == "query-key-abcdef" && r.URL.Query().Get("q") == "bug"
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "count": 3})
	}
	out, err := f.exec.Execute(ctx, "acme.search", map[string]any{"q": "bug"})
	if err != nil || out["count"] != float64(3) {
		t.Fatalf("%v %v", out, err)
	}
	f.respond = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":false,"error":"nope"}`)) }
	if _, err := f.exec.Execute(ctx, "acme.search", map[string]any{"q": "bug"}); err == nil || !strings.Contains(err.Error(), "success condition failed") {
		t.Fatal(err)
	}
	for in, want := range map[string]map[string]any{
		"missing required input":         {},
		`did you mean "q"`:               {"q": "x", "qq": 1},
		"want string":                    {"q": []any{1}},
		`integration acme has no action`: nil,
	} {
		ref := "acme.search"
		if want == nil {
			ref = "acme.serch"
		}
		_, err := f.exec.Execute(ctx, ref, want)
		if err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("%q: %v", in, err)
		}
	}
	if _, err := f.exec.Execute(ctx, "nope.x", nil); err == nil {
		t.Fatal("unknown integration")
	}
	got, err := CheckInputs(map[string]Input{"n": {Type: "number"}, "b": {Type: "boolean"}, "a": {Type: "array"}, "s": {Type: "string"}}, map[string]any{"n": "4", "b": "true", "a": `["x"]`, "s": 5})
	if err != nil || got["n"] != 4.0 || got["b"] != true || got["s"] != "5" {
		t.Fatalf("%v %v", got, err)
	}
}
