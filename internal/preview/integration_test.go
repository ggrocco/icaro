//go:build integration

package preview

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container/docker"
	"icaro/internal/paths"
	"icaro/internal/store"
)

// Images come from the GCR mirror: Docker Hub rate-limits anonymous pulls.
const (
	itTraefik = "mirror.gcr.io/library/traefik:v3.1"
	itBusybox = "mirror.gcr.io/library/busybox:1.36"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func get(port int, host string) (int, string, error) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	req.Host = host
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func eventually(t *testing.T, what string, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, last)
}

func TestIntegrationPreviewLifecycle(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // registered first, so it runs after the removals below
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Ping(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.EnsureImage(ctx, itBusybox); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	l := paths.New(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cleanupDirs := map[string]string{}
	m := New(Options{Layout: l, HTTPPort: port, TraefikImage: itTraefik}, st, CLI{}, eng, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	go func() { _ = m.Run(ctx) }()
	t.Cleanup(func() {
		c := context.Background()
		_ = eng.RemoveContainer(c, ProxyName(l.Root))
		_ = eng.RemoveNetwork(c, Network)
	})
	t.Cleanup(func() {
		// Safety net if the test failed midway.
		for _, p := range []string{"a", "b", "evil"} {
			if dir, ok := cleanupDirs[p]; ok {
				if err := m.Remove(context.Background(), dir); err != nil {
					t.Errorf("cleanup %s: %v", dir, err)
				}
			}
		}
	})

	compose := `services:
  web:
    image: ` + itBusybox + `
    command: ["httpd", "-f", "-p", "8000", "-h", "/www"]
    labels:
      icaro.preview.port: "8000"
    volumes:
      - ./site:/www:ro
`
	mk := func(parent string) string {
		dir := filepath.Join(parent, "shop")
		if err := os.MkdirAll(filepath.Join(dir, "site"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "site", "index.html"), []byte("v1 "+parent), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a, b := mk(t.TempDir()), mk(t.TempDir())
	cleanupDirs["a"], cleanupDirs["b"] = a, b

	// Creating a worktree with a compose file brings up a preview.
	pa, err := m.Add(ctx, AddRequest{Path: a, Wait: true})
	if err != nil {
		t.Fatalf("add: %v (%+v)", err, pa)
	}
	if pa.Status != store.PreviewRunning {
		t.Fatalf("%+v", pa)
	}
	eventually(t, "preview A reachable through Traefik", 30*time.Second, func() error {
		code, body, err := get(port, pa.Hostname)
		if err != nil {
			return err
		}
		if code != 200 || !strings.HasPrefix(body, "v1") {
			return fmt.Errorf("HTTP %d %q", code, body)
		}
		return nil
	})

	// An edit in the worktree is visible through the preview (bind mount).
	if err := os.WriteFile(filepath.Join(a, "site", "index.html"), []byte("v2 edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "edit visible", 10*time.Second, func() error {
		_, body, err := get(port, pa.Hostname)
		if err != nil {
			return err
		}
		if body != "v2 edited" {
			return fmt.Errorf("body %q", body)
		}
		return nil
	})

	// Two worktrees with the same slug do not collide.
	pb, err := m.Add(ctx, AddRequest{Path: b, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if pb.Hostname == pa.Hostname || pb.ComposeProject == pa.ComposeProject {
		t.Fatalf("collision %s %s", pa.Hostname, pb.Hostname)
	}
	eventually(t, "preview B reachable", 30*time.Second, func() error {
		_, body, err := get(port, pb.Hostname)
		if err != nil {
			return err
		}
		if !strings.Contains(body, filepath.Dir(b)) {
			return fmt.Errorf("B served %q", body)
		}
		return nil
	})
	if _, body, _ := get(port, pa.Hostname); body != "v2 edited" {
		t.Fatalf("A changed after B came up: %q", body)
	}

	// Re-syncing an unchanged preview does not recreate its container.
	before := containerIDs(t, pa.ComposeProject)
	if _, err := m.Add(ctx, AddRequest{Path: a, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if after := containerIDs(t, pa.ComposeProject); after != before {
		t.Fatalf("unchanged config restarted the stack: %s -> %s", before, after)
	}

	// A dangerous compose file is blocked and explains why.
	evil := filepath.Join(t.TempDir(), "evil")
	cleanupDirs["evil"] = evil
	_ = os.MkdirAll(evil, 0o750)
	_ = os.WriteFile(filepath.Join(evil, "compose.yaml"), []byte("services:\n  web:\n    image: "+itBusybox+"\n    privileged: true\n    volumes: [\"/var/run/docker.sock:/s\"]\n"), 0o600)
	pe, err := m.Add(ctx, AddRequest{Path: evil, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if pe.Status != store.PreviewBlocked || !strings.Contains(pe.Error, "privileged") || !strings.Contains(pe.Error, "socket") {
		t.Fatalf("%+v", pe)
	}
	if ids := containerIDs(t, pe.ComposeProject); ids != "" {
		t.Fatal("blocked compose started containers")
	}
	_ = m.Remove(ctx, evil)

	// Removing a worktree runs an idempotent cleanup.
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Remove(ctx, a); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
	}
	if ids := containerIDs(t, pa.ComposeProject); ids != "" {
		t.Fatalf("containers left: %s", ids)
	}
	eventually(t, "route removed", 10*time.Second, func() error {
		code, _, err := get(port, pa.Hostname)
		if err != nil {
			return err
		}
		if code != http.StatusNotFound {
			return fmt.Errorf("HTTP %d", code)
		}
		return nil
	})

	if err := m.Remove(ctx, b); err != nil {
		t.Fatal(err)
	}
	if ids := containerIDs(t, pb.ComposeProject); ids != "" {
		t.Fatalf("containers left for B: %s", ids)
	}

	// Doctor: network present, nothing blocked after cleanup.
	if r := NetworkCheck(eng, ProxyName(l.Root)).Run(ctx); r.Severity != "ok" {
		t.Fatalf("network check: %+v", r)
	}
}

func containerIDs(t *testing.T, project string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output() //nolint:gosec // fixed binary; project names come from NameFor
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
