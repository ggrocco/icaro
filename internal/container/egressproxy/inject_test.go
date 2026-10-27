package egressproxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDaemon serves h on a Unix socket, as the daemon's injector does.
func fakeDaemon(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "egi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "inject.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestForwarderRelaysToTheDaemon(t *testing.T) {
	release := make(chan struct{})
	sock := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Seen-Path", r.URL.RequestURI())
		w.Header().Set("X-Seen-Key", r.Header.Get("X-Api-Key"))
		w.Header().Set("X-Seen-Proxy-Auth", r.Header.Get("Proxy-Authorization"))
		_, _ = io.WriteString(w, "event: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: two\n\n")
	}))
	relay := httptest.NewServer(NewForwarder(sock, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer relay.Close()

	req, _ := http.NewRequest("POST", relay.URL+"/anthropic/v1/messages?beta=true", strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "icaro-injected.S.t")
	req.Header.Set("Proxy-Authorization", "Basic x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("X-Seen-Path") != "/anthropic/v1/messages?beta=true" || resp.Header.Get("X-Seen-Key") != "icaro-injected.S.t" {
		t.Fatalf("the relay must pass the request through unchanged: %v", resp.Header)
	}
	if resp.Header.Get("X-Seen-Proxy-Auth") != "" {
		t.Fatal("proxy credentials must not reach the daemon")
	}
	rd := bufio.NewReader(resp.Body)
	got := make(chan string, 1)
	go func() { l, _ := rd.ReadString('\n'); got <- l }()
	select {
	case l := <-got:
		if l != "event: one\n" {
			t.Fatal(l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay buffered the event stream")
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "event: two") {
		t.Fatalf("%q", rest)
	}
}

func TestForwarderRefusals(t *testing.T) {
	sock := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	relay := httptest.NewServer(NewForwarder(sock, nil))
	defer relay.Close()
	for path, want := range map[string]int{"/openai/v1/responses": 204, "/": 404, "/v1/messages": 404, "/anthropicx/v1": 404} {
		resp, err := http.Post(relay.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	// CONNECT is not proxied here.
	conn, err := net.Dial("tcp", relay.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("%v %v", resp, err)
	}
	// The daemon is down: 502, not a hang.
	down := httptest.NewServer(NewForwarder(filepath.Join(t.TempDir(), "missing.sock"), nil))
	defer down.Close()
	r2, err := http.Post(down.URL+"/anthropic/v1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusBadGateway {
		t.Fatal(r2.StatusCode)
	}
}

func TestServeWithInjection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeWithInjection(ctx, "127.0.0.1:0", New(nil, nil), "127.0.0.1:0", NewForwarder("/nonexistent", nil))
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A listener that cannot start stops both.
	if err := ServeWithInjection(context.Background(), "127.0.0.1:0", New(nil, nil), "256.0.0.1:0", NewForwarder("/x", nil)); err == nil {
		t.Fatal("a bad injection address must fail")
	}
}
