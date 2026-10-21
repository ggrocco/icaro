package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/daemon"
)

// startDaemon runs the real daemon in-process on a Unix socket under a
// short temp home (macOS caps socket paths at 104 bytes), with Docker
// pointed at a socket that does not exist, so the Docker-gated components
// wait and every API the commands use still answers. Commands run through
// runCLI reach it like a user's terminal would.
func startDaemon(t *testing.T) string {
	t.Helper()
	home := daemonEnv(t)
	cfg, err := (&rootOpts{}).loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	waitDaemon(t, cfg, done)
	return home
}

// waitDaemon waits until the daemon answers on its socket.
func waitDaemon(t *testing.T, cfg *config.Config, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := daemon.Ping(context.Background(), cfg.Layout()); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not answer")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// daemonEnv points ICARO_* at a fresh short home with a master key and an
// unreachable Docker engine.
func daemonEnv(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "ic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	t.Setenv("ICARO_DAEMON__URL", "")
	// An ephemeral UI port, so `icaro ui` never collides with a real daemon.
	t.Setenv("ICARO_DAEMON__UI_LISTEN", "127.0.0.1:0")
	t.Setenv("ICARO_DOCKER__HOST", "unix://"+filepath.Join(home, "no-docker.sock"))
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	t.Setenv("ICARO_MASTER_KEY", base64.StdEncoding.EncodeToString(k))
	return home
}

// runCLICtx is runCLI with a context, for commands that serve until it ends.
func runCLICtx(ctx context.Context, args ...string) (string, error) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

// withStdio replaces os.Stdin with input and os.Stdout with a file for the
// commands that use the process's stdio directly (secret prompts, the MCP
// bridge). The returned func restores both and returns what was written.
func withStdio(t *testing.T, input string) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = r, out
	restored := false
	restore := func() string {
		if !restored {
			restored = true
			os.Stdin, os.Stdout = oldIn, oldOut
			_ = r.Close()
			_ = out.Close()
		}
		b, _ := os.ReadFile(out.Name())
		return string(b)
	}
	t.Cleanup(func() { restore() })
	return restore
}

// mustCLI runs a command that must succeed and returns its output.
func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("icaro %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// wantCLI runs a command and checks that its output contains every want.
func wantCLI(t *testing.T, args []string, want ...string) string {
	t.Helper()
	out := mustCLI(t, args...)
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("icaro %s: output lacks %q:\n%s", strings.Join(args, " "), w, out)
		}
	}
	return out
}

// failCLI runs a command that must fail with an error mentioning want.
func failCLI(t *testing.T, want string, args ...string) {
	t.Helper()
	out, err := runCLI(t, "", args...)
	if err == nil {
		t.Fatalf("icaro %s succeeded:\n%s", strings.Join(args, " "), out)
	}
	if !strings.Contains(err.Error()+out, want) {
		t.Errorf("icaro %s: error %q / output %q lacks %q", strings.Join(args, " "), err, out, want)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDaemonHelperSmoke(t *testing.T) {
	startDaemon(t)
	wantCLI(t, []string{"workflow", "list"}, "NAME", "VERSION")
}
