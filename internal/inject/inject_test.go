package inject

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icaro/internal/logs"
)

const (
	realAnthropic = "sk-ant-api03-REALKEY-0123456789abcdefghijklmnopqrstuvwxyz"
	realOpenAI    = "sk-proj-REALKEY-abcdefghijklmnopqrstuvwxyz0123456789"
	tokenA        = TokenPrefix + "SESSIONA.secret-a"
	tokenB        = TokenPrefix + "SESSIONB.secret-b"
)

// fakeAuth knows two tokens: A on profile "claude", B on profile "other".
type fakeAuth struct{ fail error }

func (f fakeAuth) Authorize(_ context.Context, profile, token string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	switch {
	case token == tokenA && profile == "claude":
		return "SESSIONA", nil
	case token == tokenB && profile == "other":
		return "SESSIONB", nil
	}
	return "", fmt.Errorf("%w: unknown token", ErrRefused)
}

// fakeKeys holds secrets bound to one host each, like secrets.ResolveFor.
type fakeKeys map[string][2]string // name -> value, host

func (f fakeKeys) ResolveFor(_ context.Context, name, host string) (string, error) {
	v, ok := f[name]
	if !ok {
		return "", errors.New("secret not found: " + name)
	}
	if v[1] != host {
		return "", fmt.Errorf("host is outside the secret's binding: %s may be sent to %s, not %s", name, v[1], host)
	}
	return v[0], nil
}

// memAudit collects entries on a channel (the handler records from its
// own goroutine).
type memAudit struct{ ch chan Entry }

func (m memAudit) Record(e Entry) { m.ch <- e }

// seen is what the fake provider received.
type seen struct {
	method, path, query, body string
	header                    http.Header
}

type rig struct {
	srv      *httptest.Server // the injector, as the proxy reaches it
	upstream *httptest.Server // the fake provider (TLS)
	got      chan seen
	calls    *atomic.Int64
	audit    memAudit
	logs     *bytes.Buffer
	h        *Handler
}

// newRig builds an injector for profile "claude" in front of a fake TLS
// provider. respond writes the provider's answer.
func newRig(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *rig {
	t.Helper()
	rg := &rig{got: make(chan seen, 16), calls: &atomic.Int64{}, audit: memAudit{ch: make(chan Entry, 16)}, logs: &bytes.Buffer{}}
	rg.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		rg.got <- seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b), header: r.Header.Clone()}
		respond(w, r)
	}))
	t.Cleanup(rg.upstream.Close)
	host := "127.0.0.1"
	rg.h = &Handler{
		Profile: "claude",
		Secrets: map[string]string{"anthropic": "anthropic-key", "openai": "openai-key"},
		Auth:    fakeAuth{},
		Keys:    fakeKeys{"anthropic-key": {realAnthropic, host}, "openai-key": {realOpenAI, host}},
		Audit:   rg.audit,
		Log:     slog.New(slog.NewTextHandler(rg.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		// The fake provider's certificate is trusted by its own client
		// transport; verification stays on.
		Transport: rg.upstream.Client().Transport,
		Upstreams: map[string]string{"anthropic": rg.upstream.URL, "openai": rg.upstream.URL},
	}
	rg.srv = httptest.NewServer(rg.h)
	t.Cleanup(rg.srv.Close)
	return rg
}

func (rg *rig) do(t *testing.T, method, path string, body io.Reader, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, rg.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(b)
}

// noKey fails if a real key appears anywhere in s.
func noKey(t *testing.T, where, s string) {
	t.Helper()
	for _, k := range []string{realAnthropic, realOpenAI} {
		if strings.Contains(s, k) {
			t.Fatalf("the real key leaked into %s: %s", where, s)
		}
	}
}

func TestAnthropicInjection(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
		// A hostile or buggy upstream echoes the key in a header, a
		// cookie and the body; none of it may reach the agent.
		w.Header().Set("X-Echo", "key="+realAnthropic)
		w.Header().Set("Set-Cookie", "s=1")
		w.Header().Set("Request-Id", "req_1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","echo":"%s"}`, realAnthropic)
	})
	resp, body := rg.do(t, "POST", "/anthropic/v1/messages?beta=true", strings.NewReader(`{"model":"m","messages":[]}`), map[string]string{
		"X-Api-Key":           tokenA,
		"Authorization":       "Bearer sk-ant-smuggled-0123456789abcdef",
		"Cookie":              "a=b",
		"Anthropic-Version":   "2023-06-01",
		"Openai-Organization": "org-x",
		"X-Forwarded-For":     "1.2.3.4",
		"Content-Type":        "application/json",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	got := <-rg.got
	if got.method != "POST" || got.path != "/v1/messages" || got.query != "beta=true" || got.body != `{"model":"m","messages":[]}` {
		t.Fatalf("%+v", got)
	}
	if k := got.header.Get("X-Api-Key"); k != realAnthropic {
		t.Fatalf("upstream got x-api-key %q", k)
	}
	for _, h := range []string{"Authorization", "Cookie", "Openai-Organization", "X-Forwarded-For"} {
		if got.header.Get(h) != "" {
			t.Fatalf("client header %s reached the provider: %q", h, got.header.Get(h))
		}
	}
	if got.header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("API headers must pass")
	}
	noKey(t, "the response body", body)
	for k, vs := range resp.Header {
		noKey(t, "response header "+k, strings.Join(vs, " "))
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Echo") != "" || resp.Header.Get("Request-Id") != "req_1" {
		t.Fatalf("response headers: %v", resp.Header)
	}
	if !strings.Contains(body, `"echo":"`+logs.Redacted+`"`) {
		t.Fatalf("body: %s", body)
	}
	e := <-rg.audit.ch
	if e.Session != "SESSIONA" || e.Profile != "claude" || e.Provider != "anthropic" || e.Method != "POST" ||
		e.Path != "/anthropic/v1/messages" || e.Status != 200 || e.RequestBytes != int64(len(got.body)) || e.ResponseBytes == 0 {
		t.Fatalf("audit: %+v", e)
	}
	line, _ := json.Marshal(e)
	noKey(t, "the audit entry", string(line))
	noKey(t, "the daemon log", rg.logs.String())
}

func TestOpenAIInjection(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) })
	resp, body := rg.do(t, "POST", "/openai/v1/responses", strings.NewReader(`{}`), map[string]string{"Authorization": "Bearer " + tokenA})
	if resp.StatusCode != 200 || body != `{"ok":true}` {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	got := <-rg.got
	if got.path != "/v1/responses" || got.header.Get("Authorization") != "Bearer "+realOpenAI || got.header.Get("X-Api-Key") != "" {
		t.Fatalf("%+v", got)
	}
	if resp, _ := rg.do(t, "GET", "/openai/v1/models/gpt-5", nil, map[string]string{"Authorization": "bearer " + tokenA}); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	<-rg.got
}

func TestPathAllowlist(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	auth := map[string]string{"X-Api-Key": tokenA}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/anthropic/v1/messages", 204},
		{"POST", "/anthropic/v1/messages/count_tokens", 204},
		{"GET", "/anthropic/v1/models", 204},
		{"GET", "/anthropic/v1/models/claude-x-1@2025", 204},
		{"GET", "/anthropic/v1/messages", 403},
		{"POST", "/anthropic/v1/complete", 403},
		{"POST", "/anthropic/v1/messages/batches", 403},
		{"GET", "/anthropic/v1/models/a/b", 403},
		{"GET", "/anthropic/v1/models/a%2Fb", 403},
		{"POST", "/anthropic/v1//messages", 403},
		{"GET", "/anthropic/v1/models/.hidden", 403},
		{"GET", "/anthropic/v1/models/a$b", 403},
		{"DELETE", "/openai/v1/models/x", 403},
		{"POST", "/openai/v1/files", 403},
		{"GET", "/anthropic/v1/models/" + strings.Repeat("a", 201), 403},
	} {
		resp, body := rg.do(t, c.method, c.path, nil, auth)
		if resp.StatusCode != c.want {
			t.Fatalf("%s %s: %d %s", c.method, c.path, resp.StatusCode, body)
		}
		if c.want == 204 {
			<-rg.got
		}
		if e := <-rg.audit.ch; e.Status != c.want || e.Session != "SESSIONA" {
			t.Fatalf("every authorised request is audited: %+v", e)
		}
	}
	if n := rg.calls.Load(); n != 4 {
		t.Fatalf("refused requests reached the provider: %d calls", n)
	}
}

func TestTokensAndProviders(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
		want int
	}{
		"no token":        {"/anthropic/v1/messages", nil, 401},
		"unknown token":   {"/anthropic/v1/messages", map[string]string{"X-Api-Key": TokenPrefix + "x.y"}, 401},
		"real-looking":    {"/anthropic/v1/messages", map[string]string{"X-Api-Key": realAnthropic}, 401},
		"other profile":   {"/anthropic/v1/messages", map[string]string{"X-Api-Key": tokenB}, 401},
		"basic auth":      {"/openai/v1/responses", map[string]string{"Authorization": "Basic " + tokenA}, 401},
		"unknown":         {"/gemini/v1/x", map[string]string{"X-Api-Key": tokenA}, 404},
		"root":            {"/", map[string]string{"X-Api-Key": tokenA}, 404},
		"not configured":  {"/openai/v1/responses", map[string]string{"Authorization": "Bearer " + tokenA}, 404},
		"configured, ok":  {"/anthropic/v1/messages", map[string]string{"X-Api-Key": tokenA}, 200},
		"prefix mismatch": {"/anthropicx/v1/messages", map[string]string{"X-Api-Key": tokenA}, 404},
	} {
		h := *rg.h
		if name == "not configured" {
			h.Secrets = map[string]string{"anthropic": "anthropic-key"}
		}
		srv := httptest.NewServer(&h)
		req, _ := http.NewRequest("POST", srv.URL+c.path, nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		srv.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		if c.want != 200 && !strings.Contains(string(b), `"message":"icaro: `) {
			t.Fatalf("%s: errors use the providers' error shape: %s", name, b)
		}
	}
	if n := rg.calls.Load(); n != 1 {
		t.Fatalf("only the authorised request may reach the provider: %d", n)
	}
	// An authorizer that cannot answer is a 503, not a refusal.
	h := *rg.h
	h.Auth = fakeAuth{fail: errors.New("database is locked")}
	srv := httptest.NewServer(&h)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/anthropic/v1/messages", nil)
	req.Header.Set("X-Api-Key", tokenA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
}

func TestBodyCap(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	rg.h.MaxBody = 16
	auth := map[string]string{"X-Api-Key": tokenA}
	// Declared too large: refused before anything is sent.
	if resp, body := rg.do(t, "POST", "/anthropic/v1/messages", strings.NewReader(strings.Repeat("x", 17)), auth); resp.StatusCode != 413 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	<-rg.audit.ch
	// Within the cap.
	if resp, _ := rg.do(t, "POST", "/anthropic/v1/messages", strings.NewReader("small"), auth); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if got := <-rg.got; got.body != "small" {
		t.Fatal(got.body)
	}
	<-rg.audit.ch
	if rg.calls.Load() != 1 {
		t.Fatalf("an oversized body reached the provider: %d calls", rg.calls.Load())
	}
	// Chunked (no declared length) and too large: the body streams, so the
	// provider may see the start of the request, but it is cut off at the
	// cap and the agent gets 413.
	rg2 := newRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	rg2.h.MaxBody = 16
	resp, body := rg2.do(t, "POST", "/anthropic/v1/messages", io.MultiReader(strings.NewReader(strings.Repeat("x", 64<<10))), auth)
	if resp.StatusCode != 413 {
		t.Fatalf("chunked: %d %s", resp.StatusCode, body)
	}
	if e := <-rg2.audit.ch; e.Status != 413 || e.RequestBytes <= 16 {
		t.Fatalf("%+v", e)
	}
}

// TestStreaming: server-sent events reach the agent as the provider sends
// them, not when the response ends.
func TestStreaming(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"k\":\""+realAnthropic+"\"}\n\n")
	})
	req, _ := http.NewRequest("POST", rg.srv.URL+"/anthropic/v1/messages", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", tokenA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	rd := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := rd.ReadString('\n')
			b.WriteString(line)
			if err != nil || line == "\n" {
				first <- b.String()
				return
			}
		}
	}()
	select {
	case ev := <-first:
		if !strings.Contains(ev, "message_start") {
			t.Fatal(ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was buffered until the response ended")
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("%q", rest)
	}
	noKey(t, "the event stream", string(rest))
	<-rg.got
}

func TestUpstreamFailures(t *testing.T) {
	t.Parallel()
	rg := newRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	auth := map[string]string{"X-Api-Key": tokenA}
	check := func(name string, h Handler, want int) {
		t.Helper()
		srv := httptest.NewServer(&h)
		defer srv.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/anthropic/v1/messages", strings.NewReader("{}"))
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		noKey(t, name+" response", string(b))
		e := <-rg.audit.ch
		noKey(t, name+" audit", e.Error)
	}
	h := *rg.h
	h.Keys = fakeKeys{"anthropic-key": {realAnthropic, "api.anthropic.com"}}
	check("key bound to another host", h, 502)
	h = *rg.h
	h.Keys = fakeKeys{}
	check("missing secret", h, 502)
	h = *rg.h
	h.Upstreams = map[string]string{"anthropic": "http://127.0.0.1:1"}
	check("plain-HTTP upstream", h, 502)
	h = *rg.h
	h.Upstreams = map[string]string{"anthropic": "https://%zz"}
	check("unparsable upstream", h, 502)
	h = *rg.h
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	h.Upstreams = map[string]string{"anthropic": closed.URL}
	check("unreachable upstream", h, 502)
	// Without a test transport, the real one verifies certificates: the
	// fake provider's self-signed certificate is refused.
	h = *rg.h
	h.Transport = nil
	check("untrusted certificate", h, 502)
	if rg.calls.Load() != 0 {
		t.Fatalf("%d requests reached the provider", rg.calls.Load())
	}
	noKey(t, "the daemon log", rg.logs.String())
}

func TestKeyFilter(t *testing.T) {
	t.Parallel()
	key := "sk-SECRETKEY"
	for _, chunks := range [][]string{
		{"before sk-SECRETKEY after"},
		{"before sk-SEC", "RETKEY after"},
		{"s", "k", "-", "SECRETKEY", " after sk-SECRETKEYsk-SECRETKEY"},
		{"ends with a partial sk-SEC"},
		{"no key at all"},
		{"sk-SECRETKE", "Y"},
	} {
		var out bytes.Buffer
		f := &keyFilter{w: &out, key: []byte(key)}
		for _, c := range chunks {
			if n, err := f.Write([]byte(c)); err != nil || n != len(c) {
				t.Fatal(n, err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		in := strings.Join(chunks, "")
		want := strings.ReplaceAll(in, key, logs.Redacted)
		if out.String() != want {
			t.Fatalf("%q -> %q, want %q", chunks, out.String(), want)
		}
	}
	// Nothing is held back unless it could be the key's start.
	var out bytes.Buffer
	f := &keyFilter{w: &out, key: []byte(key)}
	_, _ = f.Write([]byte("data: {}\n\n"))
	if out.String() != "data: {}\n\n" {
		t.Fatalf("held back %q", out.String())
	}
	// An empty key filters nothing; write errors surface.
	out.Reset()
	f = &keyFilter{w: &out}
	_, _ = f.Write([]byte("x"))
	if out.String() != "x" {
		t.Fatal(out.String())
	}
	f = &keyFilter{w: failWriter{}, key: []byte(key)}
	if _, err := f.Write([]byte("abc")); err == nil {
		t.Fatal("write error swallowed")
	}
	f = &keyFilter{w: failWriter{}, key: []byte(key), held: []byte("sk")}
	if err := f.Close(); err == nil {
		t.Fatal("close error swallowed")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestEnv(t *testing.T) {
	t.Parallel()
	env := Env("http://icaro-egress:3129/", map[string]string{"openai": "o", "anthropic": "a", "gemini": "g"}, "TOKEN")
	want := []string{
		"ANTHROPIC_BASE_URL=http://icaro-egress:3129/anthropic", "ANTHROPIC_API_KEY=TOKEN",
		"OPENAI_BASE_URL=http://icaro-egress:3129/openai/v1", "OPENAI_API_KEY=TOKEN",
	}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Fatalf("%q", env)
	}
	if Env("http://x", nil, "t") != nil {
		t.Fatal("no providers, no env")
	}
}

func TestFileAudit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "inject")
	var logBuf bytes.Buffer
	a := &FileAudit{Dir: dir, Log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	a.Record(Entry{Session: "S1", Method: "POST", Path: "/anthropic/v1/messages", Status: 200, Error: "Bearer abcdefghijklmnopqrstuvwxyz"})
	a.Record(Entry{Session: "S1", Method: "GET", Path: "/anthropic/v1/models", Status: 200})
	b, err := os.ReadFile(a.Path("S1"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"session":"S1"`) || strings.Contains(lines[0], "abcdefghijklmnop") {
		t.Fatalf("%q", lines)
	}
	if fi, _ := os.Stat(a.Path("S1")); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	a.Record(Entry{Session: "../escape"})
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.log")); err == nil || !strings.Contains(logBuf.String(), "odd session id") {
		t.Fatal("a path in the session id must be refused")
	}
	// Unwritable directory: logged, never a panic or an error to the agent.
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	(&FileAudit{Dir: filepath.Join(blocker, "x"), Log: a.Log}).Record(Entry{Session: "S2"})
	(&FileAudit{Dir: filepath.Join(blocker, "y")}).Record(Entry{Session: "S2"})
	_ = os.MkdirAll(filepath.Join(dir, "S3.log"), 0o700)
	a.Record(Entry{Session: "S3"})
	if strings.Count(logBuf.String(), "injection audit") < 3 {
		t.Fatal(logBuf.String())
	}
}

func TestServer(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "key="+r.Header.Get("X-Api-Key")) //nolint:gosec // a test provider echoing the key on purpose
	}))
	defer upstream.Close()
	dir, err := os.MkdirTemp("", "inj")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	audit := memAudit{ch: make(chan Entry, 4)}
	var logBuf bytes.Buffer
	s := &Server{
		Profiles: map[string]map[string]string{
			"claude": {"anthropic": "anthropic-key"},
			"long":   {"anthropic": "anthropic-key"},
		},
		SocketPath: func(p string) string {
			if p == "long" {
				return filepath.Join(dir, strings.Repeat("x", 120), "inject.sock")
			}
			return filepath.Join(dir, p, "inject.sock")
		},
		Auth:      fakeAuth{},
		Keys:      fakeKeys{"anthropic-key": {realAnthropic, "127.0.0.1"}},
		Audit:     audit,
		Log:       slog.New(slog.NewTextHandler(&logBuf, nil)),
		Transport: upstream.Client().Transport,
		Upstreams: map[string]string{"anthropic": upstream.URL},
		UID:       os.Getuid(), GID: os.Getgid(),
	}
	if s.Name() != "inject" {
		t.Fatal(s.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	sock := s.SocketPath("claude")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(sock)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode %v", fi.Mode())
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest("GET", "http://icaro/anthropic/v1/models", nil)
	req.Header.Set("X-Api-Key", tokenA)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "key="+logs.Redacted {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	<-audit.ch
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket left behind")
	}
	if !strings.Contains(logBuf.String(), "credential injection unavailable") || !strings.Contains(logBuf.String(), "long") {
		t.Fatalf("an unusable socket path must be logged: %s", logBuf.String())
	}
	if (&Server{}).logger() == nil || (&Handler{}).log() == nil {
		t.Fatal("default loggers")
	}
}

func TestListenFailures(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	s := &Server{SocketPath: func(string) string { return filepath.Join(blocker, "d", "s.sock") }}
	if _, err := s.Listen("p"); err == nil {
		t.Fatal("socket under a file")
	}
	dir, _ := os.MkdirTemp("", "inj")
	defer func() { _ = os.RemoveAll(dir) }()
	// A directory where the socket should be cannot be replaced.
	_ = os.MkdirAll(filepath.Join(dir, "s.sock", "x"), 0o700)
	s = &Server{SocketPath: func(string) string { return filepath.Join(dir, "s.sock") }}
	if _, err := s.Listen("p"); err == nil {
		t.Fatal("a non-empty directory in place of the socket")
	}
}

func TestHandOver(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(p, nil, 0o600)
	if err := handOver(false, 1234, 1234, p); err != nil {
		t.Fatal("a non-root daemon changes nothing")
	}
	if err := handOver(true, 0, 0, p); err != nil {
		t.Fatal("uid 0 changes nothing")
	}
	// Handing a file to its own owner works without privileges; as root
	// any uid works.
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 1234, 1234
	}
	if err := handOver(true, uid, gid, p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(p); err != nil || fi.Sys() == nil {
		t.Fatal(err)
	}
	if err := handOver(true, uid, gid, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing path must fail")
	}
}

func TestDropHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Connection", "X-Hop, Keep-Alive")
	h.Set("X-Hop", "1")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Upgrade", "websocket")
	h.Set("Authorization", "Bearer x")
	h.Set("Anthropic-Beta", "b")
	dropHeaders(h, requestHeaderDrop)
	if len(h) != 1 || h.Get("Anthropic-Beta") != "b" {
		t.Fatalf("%v", h)
	}
}

// brokenWriter is a ResponseWriter whose client went away.
type brokenWriter struct{ http.ResponseWriter }

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

type errReader struct{ data []byte }

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("upstream reset")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestStreamErrors(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if n, err := stream(brokenWriter{rec}, strings.NewReader("data"), "sk-key"); err == nil || n != 4 {
		t.Fatalf("a gone client must end the stream: %d %v", n, err)
	}
	rec = httptest.NewRecorder()
	n, err := stream(rec, &errReader{data: []byte("part sk-k")}, "sk-key")
	if err == nil || n != 9 || rec.Body.String() != "part sk-k" {
		t.Fatalf("an upstream reset must end the stream after flushing what was held: %d %v %q", n, err, rec.Body.String())
	}
}
