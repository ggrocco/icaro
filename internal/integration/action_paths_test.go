package integration

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/config"
)

// betaManifest exercises every auth kind, body kind and output path.
const betaManifest = `version: 1
name: beta
hosts: [api.beta.dev]
config:
  base_url: { default: "https://api.beta.dev" }
  region: { default: "eu" }
auth:
  hdr: { kind: header, header: X-Api-Key, prefix: "key ", credential: key }
  basic: { kind: basic, username: "${{ input.user }}", credential: pass }
  pair: { kind: basic, credential: pair }
  optuser: { kind: basic, username: "${{ input.opt }}", credential: pass }
  anon: { kind: none }
actions:
  form:
    inputs:
      a: { type: string, required: true }
    request:
      method: POST
      url: "https://api.beta.dev/form"
      auth: hdr
      headers: { X-Region: "${{ config.region }}" }
      body: { form: { a: "${{ input.a }}", b: "fixed" } }
    expect_status: [202]
    outputs:
      body: "${{ response.body }}"
  raw:
    inputs:
      text: { type: string, required: true }
      user: { type: string, default: "bob" }
    request:
      method: PUT
      url: "https://api.beta.dev/raw"
      auth: basic
      body: { raw: "hello ${{ input.text }}" }
    outputs:
      status: "${{ response.status }}"
  typed:
    request:
      method: POST
      url: "https://api.beta.dev/typed"
      auth: pair
      headers: { Accept: text/plain }
      body: { raw: "<x/>", content_type: application/xml }
  open:
    inputs:
      path: { type: string, default: "/open" }
    request:
      method: GET
      url: "${{ config.base_url }}${{ input.path }}"
      auth: anon
    outputs:
      missing: "${{ response.body.nope }}"
  in-url:
    inputs: { opt: { type: string } }
    request: { method: GET, url: "https://api.beta.dev/${{ input.opt }}" }
  in-query:
    inputs: { opt: { type: string } }
    request: { method: GET, url: "https://api.beta.dev/q", query: { x: "${{ input.opt }}" } }
  in-header:
    inputs: { opt: { type: string } }
    request: { method: GET, url: "https://api.beta.dev/h", headers: { X-Opt: "${{ input.opt }}" } }
  in-form:
    inputs: { opt: { type: string } }
    request: { method: POST, url: "https://api.beta.dev/f", body: { form: { x: "${{ input.opt }}" } } }
  in-json:
    inputs: { opt: { type: string } }
    request: { method: POST, url: "https://api.beta.dev/j", body: { json: { x: "${{ input.opt }}" } } }
  in-raw:
    inputs: { opt: { type: string } }
    request: { method: POST, url: "https://api.beta.dev/r", body: { raw: "${{ input.opt }}" } }
  in-username:
    inputs: { opt: { type: string } }
    request: { method: GET, url: "https://api.beta.dev/u", auth: optuser }
`

// gammaManifest needs a user-supplied base URL (a self-hosted product).
const gammaManifest = `version: 1
name: gamma
config:
  base_url: { description: "your instance" }
actions:
  ping:
    request: { method: GET, url: "${{ config.base_url }}/ping" }
`

func betaFixture(t *testing.T) *execFixture {
	t.Helper()
	f := newExecFixtureWith(t, map[string]string{"beta": betaManifest, "gamma": gammaManifest})
	if errs := f.exec.Registry.Snapshot().Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	return f
}

func TestExecuteAuthKindsAndBodies(t *testing.T) {
	f := betaFixture(t)
	ctx := context.Background()
	for name, v := range map[string]string{"beta.key": "k-123456789", "beta.pass": "p-123456789", "beta.pair": "alice:p-987654321"} {
		if err := f.secrets.Set(ctx, name, v, []string{"api.beta.dev"}); err != nil {
			t.Fatal(err)
		}
	}

	var body string
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued"))
	}
	out, err := f.exec.Execute(ctx, "beta.form", map[string]any{"a": "1 2"})
	if err != nil {
		t.Fatal(err)
	}
	req := <-f.last
	if req.Header.Get("X-Api-Key") != "key k-123456789" || req.Header.Get("X-Region") != "eu" || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("header auth / config header / form type: %v", req.Header)
	}
	if body != "a=1+2&b=fixed" {
		t.Fatalf("form body %q", body)
	}
	if out["body"] != "queued" {
		t.Fatalf("a non-JSON body is passed through as a string: %v", out)
	}

	// expect_status replaces the 2xx default: a 200 is a failure here.
	f.respond = func(w http.ResponseWriter, r *http.Request) {}
	_, err = f.exec.Execute(ctx, "beta.form", map[string]any{"a": "x"})
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Status != 200 || ae.Msg != "(empty body)" {
		t.Fatalf("expect_status: %v", err)
	}
	<-f.last

	f.respond = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	}
	out, err = f.exec.Execute(ctx, "beta.raw", map[string]any{"text": "world"})
	if err != nil || out["status"] != float64(200) {
		t.Fatalf("%v %v", out, err)
	}
	req = <-f.last
	if u, p, ok := req.BasicAuth(); !ok || u != "bob" || p != "p-123456789" {
		t.Fatalf("basic auth with a username template: %v", req.Header)
	}
	if body != "hello world" || req.Header.Get("Content-Type") != "text/plain; charset=utf-8" || req.Method != http.MethodPut {
		t.Fatalf("raw body %q %v", body, req.Header)
	}

	// A user:password credential; the encoded pair is redacted like the value.
	pair := base64.StdEncoding.EncodeToString([]byte("alice:p-987654321"))
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("echo Basic " + pair))
	}
	_, err = f.exec.Execute(ctx, "beta.typed", nil)
	if err == nil || strings.Contains(err.Error(), pair) || !strings.Contains(err.Error(), "Basic ***") {
		t.Fatalf("encoded basic credential must be redacted: %v", err)
	}
	req = <-f.last
	if req.Header.Get("Authorization") != "Basic "+pair || req.Header.Get("Content-Type") != "application/xml" || req.Header.Get("Accept") != "text/plain" {
		t.Fatalf("explicit content type and accept: %v", req.Header)
	}
}

func TestExecuteRefusesUnsafeURLs(t *testing.T) {
	f := betaFixture(t)
	ctx := context.Background()
	f.respond = func(w http.ResponseWriter, r *http.Request) { t.Errorf("no request may be sent, got %s", r.URL) }
	cases := []struct {
		ref      string
		input    map[string]any
		settings config.IntegrationConfig
		want     string
	}{
		{ref: "beta", want: "must be <integration>.<action>"},
		// An input cannot move the request to another host.
		{ref: "beta.open", input: map[string]any{"path": "@evil.example/x"}, want: `refusing request to "evil.example"`},
		{ref: "beta.open", input: map[string]any{"path": "%zz"}, want: "invalid request URL"},
		{ref: "gamma.ping", want: "needs a base URL: set integrations.gamma.base_url"},
		{ref: "gamma.ping", settings: config.IntegrationConfig{BaseURL: "http://gamma.internal"}, want: "actions use https"},
	}
	for _, c := range cases {
		f.settings["gamma"] = c.settings
		_, err := f.exec.Execute(ctx, c.ref, c.input)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: want %q, got %v", c.ref, c.input, c.want, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("refused requests must not reach the network")
	}
}

func TestExecuteReportsRenderErrors(t *testing.T) {
	f := betaFixture(t)
	ctx := context.Background()
	_ = f.secrets.Set(ctx, "beta.pass", "p-123456789", []string{"api.beta.dev"})
	f.respond = func(w http.ResponseWriter, r *http.Request) { t.Errorf("no request may be sent, got %s", r.URL) }
	// An optional input without a default is undefined when omitted; using
	// it anywhere in the request is an error, not an empty string.
	for action, want := range map[string]string{
		"in-url":      "render url",
		"in-query":    "render query x",
		"in-header":   "render header X-Opt",
		"in-form":     "render form x",
		"in-json":     "render body",
		"in-raw":      "input.opt is not defined",
		"in-username": "input.opt is not defined",
	} {
		_, err := f.exec.Execute(ctx, "beta."+action, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", action, want, err)
		}
	}
}

func TestExecuteResponseHandling(t *testing.T) {
	f := betaFixture(t)
	ctx := context.Background()

	f.respond = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }
	if _, err := f.exec.Execute(ctx, "beta.open", nil); err == nil || !strings.Contains(err.Error(), "output missing") {
		t.Fatalf("an output over a missing field must fail: %v", err)
	}

	// Redirects are never followed: they could leave the declared hosts.
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}
	var ae *ActionError
	if _, err := f.exec.Execute(ctx, "beta.open", nil); !errors.As(err, &ae) || ae.Status != http.StatusFound {
		t.Fatalf("redirect: %v", err)
	}

	long := strings.Repeat("x", 600)
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(long))
	}
	if _, err := f.exec.Execute(ctx, "beta.open", nil); !errors.As(err, &ae) || len(ae.Msg) > 520 || !strings.HasSuffix(ae.Msg, "…") {
		t.Fatalf("error bodies are truncated: %v", err)
	}

	f.exec.MaxResponse = 16
	f.respond = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(long)) }
	if _, err := f.exec.Execute(ctx, "beta.open", nil); err == nil || !strings.Contains(err.Error(), "response exceeds 16 bytes") {
		t.Fatalf("response limit: %v", err)
	}
	f.exec.MaxResponse = 0

	// The server promises more bytes than it sends.
	f.respond = func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
		_ = buf.Flush()
		_ = conn.Close()
	}
	if _, err := f.exec.Execute(ctx, "beta.open", nil); err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("truncated response: %v", err)
	}

	f.exec.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }
	if _, err := f.exec.Execute(ctx, "beta.open", nil); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("dial failure: %v", err)
	}
}

func TestExecuteWithoutSecretStore(t *testing.T) {
	f := betaFixture(t)
	f.exec.Secrets = nil
	_, err := f.exec.Execute(context.Background(), "beta.raw", map[string]any{"text": "x"})
	if err == nil || !strings.Contains(err.Error(), "no secret store configured") {
		t.Fatal(err)
	}
}

// With allow_private the executor talks plain http to a private address
// through the default resolver and dialer.
func TestExecuteAllowPrivateHTTPWithDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p", "gamma.yaml"), []byte(gammaManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ex := &Executor{
		Registry: NewRegistry(dir, nil),
		Settings: func(string) config.IntegrationConfig {
			return config.IntegrationConfig{BaseURL: srv.URL + "/api/", AllowPrivate: true}
		},
	}
	if _, err := ex.Execute(context.Background(), "gamma.ping", nil); err != nil {
		t.Fatal(err)
	}
	// Without the opt-in the same loopback target is refused.
	ex.Settings = func(string) config.IntegrationConfig { return config.IntegrationConfig{BaseURL: srv.URL} }
	if _, err := ex.Execute(context.Background(), "gamma.ping", nil); err == nil {
		t.Fatal("plain http to loopback needs allow_private")
	}
	if configuredHost("", "x") || configuredHost("::bad", "x") {
		t.Fatal("an empty or invalid base_url configures no host")
	}
}

func TestGuardedDial(t *testing.T) {
	ctx := context.Background()
	dialed := ""
	ok := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	res := fakeResolver{"public.example": "93.184.216.34", "private.example": "10.0.0.1"}
	dial := guardedDial(res, ok, false)
	if _, err := dial(ctx, "tcp", "no-port"); err == nil {
		t.Fatal("an address without a port is an error")
	}
	if _, err := dial(ctx, "tcp", "unknown.example:443"); err == nil {
		t.Fatal("resolution errors are returned")
	}
	for _, addr := range []string{"127.0.0.1:443", "private.example:443", "[::1]:443"} {
		if _, err := dial(ctx, "tcp", addr); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: want blocked, got %v", addr, err)
		}
	}
	c, err := dial(ctx, "tcp", "public.example:443")
	if err != nil || dialed != "93.184.216.34:443" {
		t.Fatalf("the resolved address is dialed: %v %s", err, dialed)
	}
	_ = c.Close()
	failing := guardedDial(res, func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }, false)
	if _, err := failing(ctx, "tcp", "public.example:443"); err == nil || err.Error() != "refused" {
		t.Fatalf("the dial error is returned: %v", err)
	}
	if !IsBlockedIP(nil) {
		t.Fatal("an unparseable address is blocked")
	}
}
