package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// newTrustedUI serves the UI handler with daemon.ui_trust_loopback on, as
// the UI listener does (ADR 0033).
func newTrustedUI(t *testing.T) (*uiEnv, *httptest.Server) {
	t.Helper()
	e := newUIEnv(t)
	s := New(e.svc, quietLog())
	s.RegisterUI(UIOptions{Assets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}, TrustLoopback: true})
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.ConnContext = TrustLoopbackConnContext
	srv.Start()
	t.Cleanup(srv.Close)
	return e, srv
}

func trustedDo(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"yaml":"x"}`))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTrustedLoopbackNeedsNoSignIn(t *testing.T) {
	_, srv := newTrustedUI(t)
	origin := "http://" + strings.TrimPrefix(srv.URL, "http://")

	// No token, no cookie: the local user, with the socket's scope.
	code, body := trustedDo(t, srv, "GET", "/api/v1/ui/me", nil)
	if code != 200 || !strings.Contains(body, `"local": true`) || !strings.Contains(body, `"admin"`) {
		t.Fatalf("me = %d %s", code, body)
	}
	if code, _ := trustedDo(t, srv, "GET", "/api/v1/workflows", map[string]string{"Host": "localhost:8787"}); code != 200 {
		t.Fatalf("read via localhost = %d", code)
	}
	// Mutations need the CSRF header; with it they pass auth (and fail on
	// the body, which is not a workflow).
	if code, body := trustedDo(t, srv, "POST", "/api/v1/workflows", nil); code != 403 || !strings.Contains(body, CSRFHeader) {
		t.Fatalf("mutation without CSRF = %d %s", code, body)
	}
	if code, _ := trustedDo(t, srv, "POST", "/api/v1/workflows", map[string]string{CSRFHeader: "1", "Origin": origin}); code == 401 || code == 403 {
		t.Fatalf("same-origin mutation = %d", code)
	}

	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want string
	}{
		// A DNS-rebinding page sends its own hostname.
		{"rebinding", map[string]string{"Host": "evil.example.com"}, "not a loopback"},
		{"rebinding with port", map[string]string{"Host": "evil.example.com:8787"}, "not a loopback"},
		{"cross origin", map[string]string{"Origin": "https://evil.example.com"}, "cross-origin"},
		{"bad origin", map[string]string{"Origin": "::"}, "cross-origin"},
	} {
		if code, body := trustedDo(t, srv, "GET", "/api/v1/workflows", tc.hdr); code != 403 || !strings.Contains(body, tc.want) {
			t.Errorf("%s = %d %s", tc.name, code, body)
		}
	}

	// A bearer token still decides who the caller is.
	if code, _ := trustedDo(t, srv, "GET", "/api/v1/workflows", map[string]string{"Authorization": "Bearer icaro_nope"}); code != 401 {
		t.Fatalf("bad bearer = %d", code)
	}
}

// Without the trusted ConnContext, the same handler still requires a
// sign-in: the trust is a property of the UI listener, not of the routes.
func TestLoopbackTrustIsPerListener(t *testing.T) {
	e := newUIEnv(t)
	s := New(e.svc, quietLog())
	s.RegisterUI(UIOptions{Assets: fstest.MapFS{}, TrustLoopback: true})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if code, _ := trustedDo(t, srv, "GET", "/api/v1/ui/me", nil); code != 401 {
		t.Fatalf("untrusted listener = %d", code)
	}
}

func TestTrustedLinkHasNoToken(t *testing.T) {
	e := newUIEnv(t)
	s := New(e.svc, quietLog())
	s.RegisterUI(UIOptions{Assets: fstest.MapFS{}, TrustLoopback: true, Open: func() (string, error) { return "http://127.0.0.1:8787", nil }})
	c := serveUnix(t, s.Handler())
	code, body := doReq(t, c, "POST", "http://icaro/api/v1/ui/link", nil, nil)
	if code != 200 || !strings.Contains(body, `"url": "http://127.0.0.1:8787/ui/"`) || strings.Contains(body, "token=") || !strings.Contains(body, `"trusted": true`) {
		t.Fatalf("link = %d %s", code, body)
	}
}

func TestLoopbackHelpers(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost": true, "LOCALHOST:1": true, "[::1]:8787": true, "localhost.:80": true,
		"127.1.2.3": true, "10.0.0.1:80": false, "example.com": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v", host, got)
		}
	}
	// A non-loopback peer is not marked, even on a trusted listener.
	ctx := TrustLoopbackConnContext(context.Background(), fakeConn{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1")}})
	if trustedLoopback(ctx) {
		t.Fatal("remote peer marked as loopback")
	}
	ctx = TrustLoopbackConnContext(context.Background(), fakeConn{addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}})
	if !trustedLoopback(ctx) || IsLocal(ctx) {
		t.Fatal("loopback peer: trusted, but never the socket's local")
	}
	if IsLoopbackPrincipal(context.Background()) {
		t.Fatal("no principal")
	}
}

type fakeConn struct {
	net.Conn
	addr net.Addr
}

func (c fakeConn) RemoteAddr() net.Addr { return c.addr }
