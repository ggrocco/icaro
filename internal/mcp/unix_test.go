package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortDir(t *testing.T) string {
	t.Helper()
	// t.TempDir paths can exceed the 104-byte socket limit on macOS.
	dir, err := os.MkdirTemp("", "mcpsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestServeUnix(t *testing.T) {
	s := NewServer("icaro", "test", "")
	s.AddTool(Tool{Name: "whoami", Description: "caller", Handler: func(ctx context.Context, _ json.RawMessage) (*ToolResult, error) {
		c, _ := CallerFrom(ctx)
		return TextResult(c.SessionID + "/" + c.Scope), nil
	}})
	path := filepath.Join(shortDir(t), "sub", "mcp.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := ServeUnix(ctx, s, Caller{SessionID: "s1", Scope: "write"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v %v", fi, err)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	c := HTTPClient(hc, "http://icaro/mcp", nil)
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(ctx, "whoami", map[string]any{})
	if err != nil || res.Content[0].Text != "s1/write" {
		t.Fatalf("%+v %v", res, err)
	}

	// Serving the same path again replaces the old socket.
	stop2, err := ServeUnix(ctx, s, Caller{SessionID: "s2"}, path)
	if err != nil {
		t.Fatal(err)
	}
	stop2()
	stop2()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
	stop()

	// Cancelling the context stops the server too.
	ctx3, cancel3 := context.WithCancel(context.Background())
	if _, err := ServeUnix(ctx3, s, Caller{}, path); err != nil {
		t.Fatal(err)
	}
	cancel3()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not removed after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeUnixErrors(t *testing.T) {
	s := NewServer("icaro", "test", "")
	ctx := context.Background()
	if _, err := ServeUnix(ctx, s, Caller{}, "/"+strings.Repeat("x", MaxSocketPath)); !errors.Is(err, ErrSocketPath) {
		t.Fatalf("long path: %v", err)
	}
	dir := shortDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServeUnix(ctx, s, Caller{}, filepath.Join(file, "mcp.sock")); err == nil {
		t.Fatal("parent is a file")
	}
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "mcp.sock", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ServeUnix(ctx, s, Caller{}, filepath.Join(full, "mcp.sock")); err == nil {
		t.Fatal("a non-empty directory in the way")
	}
	if os.Getuid() != 0 {
		ro := filepath.Join(dir, "ro")
		if err := os.MkdirAll(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := ServeUnix(ctx, s, Caller{}, filepath.Join(ro, "mcp.sock")); err == nil {
			t.Fatal("read-only directory")
		}
	}
}
