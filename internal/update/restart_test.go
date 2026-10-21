//go:build unix

package update

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/daemon"
	"icaro/internal/paths"
	"icaro/internal/platform"
)

func TestRestartDaemonWithoutDaemon(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	restarted, err := RestartDaemon(context.Background(), l, "/nonexistent", nil)
	if err != nil || restarted {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
}

// TestHelperDaemon is not a test: it is the fake daemon process that
// holds the lock until it is terminated.
func TestHelperDaemon(t *testing.T) {
	root := os.Getenv("ICARO_TEST_HELPER_ROOT")
	if root == "" {
		t.Skip("helper process only")
	}
	inst, err := daemon.Acquire(paths.New(root))
	if err != nil {
		os.Exit(2)
	}
	defer func() { _ = inst.Close() }()
	_, _ = os.Stdout.WriteString("ready\n")
	time.Sleep(time.Minute)
	os.Exit(1)
}

// startDaemon runs a helper that holds l's lock and waits for SIGTERM.
func startDaemon(t *testing.T, l paths.Layout) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemon$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "ICARO_TEST_HELPER_ROOT="+l.Root)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper did not start: %q", line)
	}
	return cmd
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "icaro")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // fixture binary must be executable
		t.Fatal(err)
	}
	return p
}

func TestRestartDaemonStopsAndStarts(t *testing.T) {
	ctx := context.Background()
	l := paths.New(filepath.Join(t.TempDir(), "r"))
	cmd := startDaemon(t, l)
	argsFile := filepath.Join(t.TempDir(), "args")
	exe := script(t, `echo "$@" > `+argsFile)

	restarted, err := RestartDaemon(ctx, l, exe, []string{"--home", l.Root})
	if err != nil || !restarted {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
	if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("the old daemon must be stopped by SIGTERM: %v", err)
	}
	got, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(got)) != "--home "+l.Root+" serve --background" {
		t.Fatalf("new daemon args: %q", got)
	}
}

func TestRestartDaemonStartFailure(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "r"))
	startDaemon(t, l)
	restarted, err := RestartDaemon(context.Background(), l, script(t, "echo cannot start; exit 1"), nil)
	if !restarted || err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
}

// holdLock takes l's lock in this process, with pid written to the PID
// file (0 leaves it absent).
func holdLock(t *testing.T, l paths.Layout, pid int) {
	t.Helper()
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := platform.TryLock(l.Lock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	if pid != 0 {
		if err := os.WriteFile(l.PID(), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestartDaemonRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown pid", func(t *testing.T) {
		l := paths.New(filepath.Join(t.TempDir(), "r"))
		holdLock(t, l, 0)
		if restarted, err := RestartDaemon(ctx, l, "/nonexistent", nil); !restarted || err == nil || !strings.Contains(err.Error(), "pid is unknown") {
			t.Fatalf("restarted=%v err=%v", restarted, err)
		}
	})

	t.Run("signal fails", func(t *testing.T) {
		l := paths.New(filepath.Join(t.TempDir(), "r"))
		// A pid no process can have.
		holdLock(t, l, 1<<22+1)
		if _, err := RestartDaemon(ctx, l, "/nonexistent", nil); err == nil || !strings.Contains(err.Error(), "stop daemon") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		l := paths.New(filepath.Join(t.TempDir(), "r"))
		// The signalled process exits but the lock stays held (by us), so
		// the wait only ends with the context.
		sleeper := exec.Command("sleep", "30")
		if err := sleeper.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
		holdLock(t, l, sleeper.Process.Pid)
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if _, err := RestartDaemon(cctx, l, "/nonexistent", nil); err != context.DeadlineExceeded {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("probe fails", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "r")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		l := paths.New(root)
		if err := os.WriteFile(filepath.Dir(l.Lock()), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RestartDaemon(ctx, l, "/nonexistent", nil); err == nil {
			t.Fatal("an unprobeable run dir must fail")
		}
	})
}
