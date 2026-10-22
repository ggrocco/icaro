package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	allow := []string{"api.anthropic.com", "*.npmjs.org"}
	for host, want := range map[string]bool{
		"api.anthropic.com": true, "API.anthropic.com.": true, "registry.npmjs.org": true,
		"npmjs.org": false, "evilnpmjs.org": false, "anthropic.com": false, "x.api.anthropic.com": false,
	} {
		if Allowed(allow, host) != want {
			t.Fatalf("%s: want %v", host, want)
		}
	}
	if Allowed(nil, "a") {
		t.Fatal("empty allowlist allows nothing")
	}
}

// fixture: an upstream server reachable as allowed.test and denied.test
// via an injected dialer.
func fixture(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from "+r.Host) //nolint:gosec // test server, plain text
	}))
	t.Cleanup(up.Close)
	p := New([]string{"allowed.test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	upAddr := up.Listener.Addr().String()
	p.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, upAddr)
	}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)
	return ps, &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
}

func TestForwardHTTP(t *testing.T) {
	_, c := fixture(t)
	resp, err := c.Get("http://allowed.test/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	resp, err = c.Get("http://denied.test/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "allowlist") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestConnect(t *testing.T) {
	ps, _ := fixture(t)
	for host, want := range map[string]string{"allowed.test:443": "200", "denied.test:443": "403"} {
		conn, err := net.Dial("tcp", ps.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "CONNECT "+host+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
		br := bufio.NewReader(conn)
		line, _ := br.ReadString('\n')
		if !strings.Contains(line, want) {
			t.Fatalf("%s: %q", host, line)
		}
		if want == "200" {
			// The tunnel reaches the upstream HTTP server.
			_, _ = br.ReadString('\n')
			_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n")
			rest, _ := io.ReadAll(br)
			if !strings.Contains(string(rest), "hello from allowed.test") {
				t.Fatalf("tunnel: %q", rest)
			}
		}
		_ = conn.Close()
	}
}

func TestBadRequests(t *testing.T) {
	ps, _ := fixture(t)
	resp, err := http.Get(ps.URL + "/relative")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ListenAndServe(ctx, "127.0.0.1:0", New(nil, nil)); err != nil {
		t.Fatal(err)
	}
	// A listen failure is returned, not swallowed.
	if err := ListenAndServe(context.Background(), "256.0.0.1:0", New(nil, nil)); err == nil {
		t.Fatal("bad listen address accepted")
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestForwardErrors(t *testing.T) {
	p := New([]string{"allowed.test"}, quiet())
	p.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") }
	for _, c := range []struct {
		method, target string
		code           int
		body           string
	}{
		// TLS must go through CONNECT; the proxy never forwards https://.
		{http.MethodGet, "https://allowed.test/", http.StatusBadRequest, "only http://"},
		{http.MethodGet, "http://allowed.test/", http.StatusBadGateway, "no route"},
		{http.MethodConnect, "allowed.test", http.StatusBadRequest, "host:port"},
		{http.MethodConnect, "allowed.test:443", http.StatusBadGateway, "no route"},
	} {
		r := httptest.NewRequest(c.method, c.target, nil)
		if c.method == http.MethodConnect {
			r.Host = c.target
		}
		w := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
		p.ServeHTTP(w, r)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
			t.Fatalf("%s %s: %d %q", c.method, c.target, w.Code, w.Body.String())
		}
	}
}

// hijackRecorder is a ResponseRecorder that claims to support hijacking
// but fails, as a closed client connection would.
type hijackRecorder struct{ *httptest.ResponseRecorder }

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack failed")
}

func TestConnectHijack(t *testing.T) {
	p := New([]string{"allowed.test"}, quiet())
	upstream, other := net.Pipe()
	defer func() { _ = other.Close() }()
	p.Dial = func(context.Context, string, string) (net.Conn, error) { return upstream, nil }
	r := httptest.NewRequest(http.MethodConnect, "allowed.test:443", nil)
	r.Host = "allowed.test:443"

	// A writer that cannot be hijacked gets a 500, and nothing is dialed.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatal(rec.Code)
	}
	// A failed hijack closes the upstream connection it already dialed.
	p.ServeHTTP(&hijackRecorder{ResponseRecorder: httptest.NewRecorder()}, r)
	_ = other.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := other.Write([]byte("x")); err == nil {
		t.Fatal("upstream left open after a failed hijack")
	}
}

func TestConnectForwardsPipelinedBytes(t *testing.T) {
	// Bytes the client sends right after CONNECT, before the 200 arrives,
	// are buffered by the server and must still reach the upstream.
	ps, _ := fixture(t)
	conn, err := net.Dial("tcp", ps.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n"+
		"GET / HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n")
	rest, _ := io.ReadAll(conn)
	if !strings.Contains(string(rest), "200 Connection Established") || !strings.Contains(string(rest), "hello from allowed.test") {
		t.Fatalf("%q", rest)
	}
}

func TestForwardStripsHopHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Up", "1")
	}))
	defer up.Close()
	p := New([]string{"allowed.test"}, quiet())
	p.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	r := httptest.NewRequest(http.MethodGet, "http://allowed.test/", nil)
	r.Header.Set("Proxy-Authorization", "secret")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("X-Up") != "1" || w.Header().Get("Keep-Alive") != "" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}
