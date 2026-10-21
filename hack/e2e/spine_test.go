//go:build e2e

// Package e2e drives the built icaro binary against a real Docker engine
// through the session spine milestone (04-roadmap): a worktree, an
// isolated agent container, a live preview of the agent's edit, and
// memory shared between the session and a host CLI over MCP.
//
//	make build && go test -tags e2e ./hack/e2e/
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/container"
	"icaro/internal/preview"
)

const busybox = "mirror.gcr.io/library/busybox:1.36"

type harness struct {
	t    *testing.T
	bin  string
	home string
	repo string
	env  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// make cover-e2e points ICARO_E2E_BIN at a coverage-instrumented build.
	bin := os.Getenv("ICARO_E2E_BIN")
	if bin == "" {
		bin = "../../bin/icaro"
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip(bin + " not built; run make build")
	}
	if err := command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	// Short paths keep Unix sockets under the macOS limit.
	home, err := os.MkdirTemp("/tmp", "ice2e-")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := os.MkdirTemp("/tmp", "ice2e-repo-")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	h := &harness{t: t, bin: bin, home: home, repo: repo,
		env: append(os.Environ(), "ICARO_HOME="+home, "ICARO_PASSWORD=e2e-password", "ICARO_CONFIG=")}
	writeFile(t, filepath.Join(home, "config", "icaro.yaml"), fmt.Sprintf(`version: 1
sessions:
  default_profile: shell
profiles:
  shell:
    image: %s
    egress: []
previews:
  http_port: %d
  traefik_image: mirror.gcr.io/library/traefik:v3.1
`, busybox, port))
	t.Cleanup(func() {
		h.icaro(context.Background(), "session", "list") // keep output on failure
		if pid, err := os.ReadFile(filepath.Join(home, "run", "icaro.pid")); err == nil {
			stopDaemon(strings.TrimSpace(string(pid)))
		}
		// The proxies are named per state root, so this removes only ours.
		_ = command("docker", "rm", "-f", preview.ProxyName(home)).Run()
		ns := container.Namespace(home)
		if out, err := command("docker", "ps", "-aq", "--filter", "label="+container.LabelEgress+"="+ns+"/shell").Output(); err == nil {
			for _, id := range strings.Fields(string(out)) {
				_ = command("docker", "rm", "-f", id).Run()
			}
		}
		_ = command("docker", "network", "rm", "icaro-egress-"+ns+"-shell").Run()
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(repo)
	})
	return h
}

func (h *harness) icaro(ctx context.Context, args ...string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...) //nolint:gosec // test binary
	cmd.Dir, cmd.Env = h.repo, h.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("icaro %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestSessionSpine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A repository with a compose project that serves site/.
	writeFile(t, filepath.Join(h.repo, "site", "index.html"), "<h1>v1</h1>\n")
	writeFile(t, filepath.Join(h.repo, "compose.yaml"), fmt.Sprintf(`services:
  web:
    image: %s
    command: httpd -f -p 8080 -h /site
    volumes:
      - ./site:/site:ro
    labels:
      icaro.preview.port: "8080"
`, busybox))
	git(t, h.repo, "init", "-q", "-b", "main")
	git(t, h.repo, "add", "-A")
	git(t, h.repo, "-c", "user.email=e2e@icaro", "-c", "user.name=e2e", "commit", "-qm", "init")

	h.icaro(ctx, "init")
	h.icaro(ctx, "serve", "--background")
	h.icaro(ctx, "session", "create", "spine")

	var sessions []struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		WorktreePath string `json:"worktree_path"`
	}
	if err := json.Unmarshal([]byte(h.icaro(ctx, "session", "list", "--json")), &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %+v", err, sessions)
	}
	s := sessions[0]
	if s.Status != "running" {
		t.Fatalf("session status %s", s.Status)
	}

	var previews []struct {
		URL      string `json:"url"`
		Hostname string `json:"hostname"`
	}
	waitFor(t, "preview listed", func() bool {
		_ = json.Unmarshal([]byte(h.icaro(ctx, "preview", "list", "--json")), &previews)
		return len(previews) == 1
	})
	host := previews[0].Hostname
	if host == "" {
		host = strings.TrimPrefix(strings.Split(previews[0].URL, ":")[1], "//")
	}
	port := previews[0].URL[strings.LastIndex(previews[0].URL, ":")+1:]
	waitFor(t, "preview serves v1", func() bool { return strings.Contains(get(host, port), "v1") })

	// The agent edits code inside its own container (non-root, read-only
	// root filesystem, /workspace = the session worktree).
	out := h.icaro(ctx, "sh", s.ID, "--", "sh", "-c", `id -u; echo '<h1>v2 from agent</h1>' > /workspace/site/index.html; touch /etc/x 2>&1 || true`)
	if strings.HasPrefix(strings.TrimSpace(out), "0") || !strings.Contains(out, "Read-only") {
		t.Fatalf("agent container is not confined: %s", out)
	}
	waitFor(t, "preview shows the agent's edit", func() bool { return strings.Contains(get(host, port), "v2 from agent") })

	// Memory: a handoff written over the session socket is recovered by a
	// host CLI in the same repository (same project scope).
	sock := filepath.Join(h.home, "run", "sessions", s.ID, "mcp.sock")
	mcpPost(t, sock, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}`)
	mcpPost(t, sock, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_write","arguments":{"kind":"handoff","title":"spine handoff","body":"v2 is live"}}}`)

	cmd := exec.Command(h.bin, "mcp") //nolint:gosec // test binary
	cmd.Dir, cmd.Env = h.repo, h.env
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"host","version":"1"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_query","arguments":{"kinds":["handoff"]}}}
`)
	got, err := cmd.Output()
	if err != nil || !strings.Contains(string(got), "spine handoff") {
		t.Fatalf("host CLI did not see the session's handoff: %v\n%s", err, got)
	}

	// Removing the session cleans up its preview idempotently.
	h.icaro(ctx, "session", "rm", s.ID, "--yes")
	waitFor(t, "preview removed", func() bool {
		_ = json.Unmarshal([]byte(h.icaro(ctx, "preview", "list", "--json")), &previews)
		return len(previews) == 0
	})
}

func mcpPost(t *testing.T, sock, body string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(http.MethodPost, "http://icaro/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("mcp socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("mcp %d: %s", resp.StatusCode, b)
	}
}

func get(host, port string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/", nil)
	req.Host = host
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// command runs a tool with test-controlled arguments.
func command(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:gosec // arguments come from the test itself
}

// stopDaemon sends SIGTERM and waits for a clean exit, which is also when
// a coverage-instrumented daemon writes its counters.
func stopDaemon(pid string) {
	_ = command("kill", pid).Run()
	for i := 0; i < 150 && command("kill", "-0", pid).Run() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
}
