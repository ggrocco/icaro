//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"icaro/internal/update"
)

// TestUpdateReattachesRunningSession is the Phase 8 acceptance test: an
// `icaro update` while a session is running restarts the daemon, and the
// new daemon re-attaches the session's container instead of orphaning it
// (ADR 0006).
func TestUpdateReattachesRunningSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Run an installed copy, so the update swaps it and not bin/icaro.
	installed := filepath.Join(h.home, "bin", "icaro")
	data, err := os.ReadFile(h.bin)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, installed, string(data))
	if err := os.Chmod(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	h.bin = installed

	// A signed release mirror whose next version is the same build.
	const next = "v99.0.0"
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, err := update.PublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	archive := tarGz(t, "icaro", data)
	name := update.ArchiveName(next, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	files := map[string][]byte{
		"/latest":                         []byte(next + "\n"),
		"/" + next + "/" + name:           archive,
		"/" + next + "/checksums.txt":     sums,
		"/" + next + "/checksums.txt.sig": ed25519.Sign(priv, sums),
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(mirror.Close)
	cfg := filepath.Join(h.home, "config", "icaro.yaml")
	conf, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg, string(conf)+fmt.Sprintf("update:\n  base_url: %s\n  public_key: |\n    %s\n",
		mirror.URL, strings.ReplaceAll(strings.TrimSpace(key), "\n", "\n    ")))

	writeFile(t, filepath.Join(h.repo, "README.md"), "update e2e\n")
	git(t, h.repo, "init", "-q", "-b", "main")
	git(t, h.repo, "add", "-A")
	git(t, h.repo, "-c", "user.email=e2e@icaro", "-c", "user.name=e2e", "commit", "-qm", "init")

	h.icaro(ctx, "init")
	h.icaro(ctx, "serve", "--background")
	h.icaro(ctx, "session", "create", "update")
	before := onlySession(t, h)
	if before.Status != "running" || before.ContainerID == "" {
		t.Fatalf("session before the update: %+v", before)
	}
	h.icaro(ctx, "sh", before.ID, "--", "sh", "-c", "echo kept > /workspace/marker")
	pidBefore := daemonPID(t, h)

	out := h.icaro(ctx, "update", "--yes")
	if !strings.Contains(out, "installed "+next) || !strings.Contains(out, "daemon restarted") {
		t.Fatalf("update output:\n%s", out)
	}
	if _, err := os.Stat(update.PrevPath(installed)); err != nil {
		t.Fatalf("previous binary not kept: %v", err)
	}
	if pid := daemonPID(t, h); pid == pidBefore {
		t.Fatalf("daemon was not restarted (pid %s)", pid)
	}

	// Same session, same container, still running and reachable.
	after := onlySession(t, h)
	if after.ID != before.ID || after.ContainerID != before.ContainerID || after.Status != "running" {
		t.Fatalf("session after the update: %+v (before %+v)", after, before)
	}
	running, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", before.ContainerID).Output()
	if err != nil || strings.TrimSpace(string(running)) != "true" {
		t.Fatalf("container after the update: %v %s", err, running)
	}
	if got := h.icaro(ctx, "sh", after.ID, "--", "cat", "/workspace/marker"); !strings.Contains(got, "kept") {
		t.Fatalf("exec after the update: %s", got)
	}
	// The memory socket is served again for the re-attached session.
	sock := filepath.Join(h.home, "run", "sessions", after.ID, "mcp.sock")
	mcpPost(t, sock, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}`)
	log, _ := os.ReadFile(filepath.Join(h.home, "logs", "daemon.log"))
	if !strings.Contains(string(log), "session re-attached") || strings.Contains(string(log), "session orphaned") {
		t.Fatalf("daemon log does not show a re-attach:\n%s", log)
	}
}

type sessionJSON struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ContainerID string `json:"container_id"`
}

func onlySession(t *testing.T, h *harness) sessionJSON {
	t.Helper()
	var list []sessionJSON
	if err := json.Unmarshal([]byte(h.icaro(context.Background(), "session", "list", "--json")), &list); err != nil || len(list) != 1 {
		t.Fatalf("sessions: %v %+v", err, list)
	}
	return list[0]
}

func daemonPID(t *testing.T, h *harness) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.home, "run", "icaro.pid"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}
