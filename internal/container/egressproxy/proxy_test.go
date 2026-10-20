package egressproxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
		_, _ = io.WriteString(w, "hello from "+r.Host)
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
}
