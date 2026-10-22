package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startUI(t *testing.T, addr string) (*uiListener, context.CancelFunc, chan error) {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") })
	u := &uiListener{addr: addr, handler: func() http.Handler { return h }, log: &App{Log: discardLog()}, reqs: make(chan chan uiResult)}
	if u.Name() != "ui" {
		t.Errorf("Name = %q", u.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	return u, cancel, done
}

func TestUIListenerOpensOnDemandOnce(t *testing.T) {
	u, cancel, done := startUI(t, "127.0.0.1:0")
	url, err := u.Open()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url = %q", url)
	}
	// A second Open reuses the running listener.
	again, err := u.Open()
	if err != nil || again != url {
		t.Fatalf("second Open = %q, %v; want %q", again, err, url)
	}
	res, err := httpGet(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(body) != "ui" {
		t.Errorf("body = %q", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	// The server is shut down with the daemon.
	if res, err := httpGet(url); err == nil {
		_ = res.Body.Close()
		t.Error("listener still serving after shutdown")
	}
}

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func TestUIListenerErrors(t *testing.T) {
	u := &uiListener{}
	if _, err := u.Open(); err == nil || !strings.Contains(err.Error(), "daemon.ui_listen is empty") {
		t.Errorf("empty addr: %v", err)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	u, cancel, done := startUI(t, busy.Addr().String())
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	}()
	if _, err := u.Open(); err == nil || !strings.Contains(err.Error(), "open UI listener on") {
		t.Errorf("port in use: %v", err)
	}
	// A failed open leaves the listener able to try again.
	if _, err := u.Open(); err == nil {
		t.Error("second open on a busy port succeeded")
	}
}

func TestHandlerTrustsOnlyTheUnixSocket(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), nil)
	get := func(ctx context.Context, query ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/usage"+strings.Join(query, ""), nil)
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}
	// The usage route is wired, and a Unix-socket connection is the
	// trusted local principal.
	if rec := get(connCtx(t, "unix")); rec.Code != http.StatusOK {
		t.Fatalf("over the socket: %d %s", rec.Code, rec.Body.String())
	}
	// A bad since is the caller's mistake, not a daemon failure.
	if rec := get(connCtx(t, "unix"), "?since=yesterday"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since: %d %s", rec.Code, rec.Body.String())
	}
	// TCP needs a token.
	if rec := get(connCtx(t, "tcp")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("over TCP without a token: %d %s", rec.Code, rec.Body.String())
	}
}

// connCtx is the request context ConnContext gives a real connection of
// the given network.
func connCtx(t *testing.T, network string) context.Context {
	t.Helper()
	addr := "127.0.0.1:0"
	if network == "unix" {
		addr = filepath.Join(shortDir(t), "c.sock")
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	c, err := net.Dial(network, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if s, ok := <-accepted; ok {
		t.Cleanup(func() { _ = s.Close() })
	}
	return ConnContext(context.Background(), c)
}
