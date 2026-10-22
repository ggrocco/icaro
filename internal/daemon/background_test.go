package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"icaro/internal/paths"
	"icaro/internal/platform"
)

// StartBackground re-executes os.Executable(), which in tests is this test
// binary. TestMain turns it into a fake daemon selected by childEnv.
const (
	childEnv  = "ICARO_DAEMON_TEST_CHILD"
	markerEnv = "ICARO_DAEMON_TEST_MARKER"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(runChild(mode))
	}
	os.Exit(m.Run())
}

func runChild(mode string) int {
	l := paths.Default() // ICARO_HOME is set by StartBackground
	switch mode {
	case "exit":
		return 1
	case "marker":
		_ = os.WriteFile(os.Getenv(markerEnv), nil, 0o600)
		return 0
	case "hang":
		_ = platform.WritePID(l.PID())
		time.Sleep(10 * time.Second)
		return 0
	case "serve":
		inst, err := Acquire(l)
		if err != nil {
			return 2
		}
		ln, err := inst.ListenUnix()
		if err != nil {
			return 2
		}
		srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
		go func() { _ = srv.Serve(ln) }()
		time.Sleep(30 * time.Second) // the test kills us long before
		return 0
	}
	return 3
}

// childMode selects the fake daemon and kills whatever child wrote the
// PID file when the test ends.
func childMode(t *testing.T, l paths.Layout, mode string) {
	t.Helper()
	t.Setenv(childEnv, mode)
	t.Cleanup(func() {
		if pid := platform.ReadPID(l.PID()); pid > 0 && pid != os.Getpid() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestEnsureRunningStartsAndReuses(t *testing.T) {
	l := paths.New(shortTempDir(t))
	childMode(t, l, "serve")
	ctx := context.Background()
	if err := EnsureRunning(ctx, l, []string{"--config", "x.yaml"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := platform.ReadPID(l.PID())
	if pid <= 0 || pid == os.Getpid() || !platform.ProcessAlive(pid) {
		t.Fatalf("child pid %d", pid)
	}
	// A second call finds the running daemon and starts nothing.
	if err := EnsureRunning(ctx, l, nil); err != nil {
		t.Fatal(err)
	}
	if got := platform.ReadPID(l.PID()); got != pid {
		t.Fatalf("pid changed %d -> %d", pid, got)
	}
	if _, err := os.Stat(l.DaemonLog()); err != nil {
		t.Fatalf("daemon log: %v", err)
	}
}

func TestStartBackgroundChildExits(t *testing.T) {
	l := paths.New(shortTempDir(t))
	childMode(t, l, "exit")
	err := StartBackground(context.Background(), l, nil, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "daemon exited during startup") || !strings.Contains(err.Error(), l.DaemonLog()) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBackgroundRacingDaemonWins(t *testing.T) {
	// Our child exits, but another daemon answers by then: that is success.
	l := paths.New(shortTempDir(t))
	childMode(t, l, "marker")
	marker := filepath.Join(filepath.Dir(l.Root), "exited")
	t.Setenv(markerEnv, marker)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", l.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var reqs atomic.Int32
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reqs.Add(1) > 1 {
			return
		}
		// Fail the first probe, and only once the child is on its way out.
		for i := 0; i < 150; i++ {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	if err := StartBackground(context.Background(), l, nil, 10*time.Second); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBackgroundTimesOut(t *testing.T) {
	l := paths.New(shortTempDir(t))
	childMode(t, l, "hang")
	err := StartBackground(context.Background(), l, nil, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not become ready within 300ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBackgroundContextCancelled(t *testing.T) {
	l := paths.New(shortTempDir(t))
	childMode(t, l, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := StartBackground(ctx, l, nil, 10*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBackgroundSetupErrors(t *testing.T) {
	ctx := context.Background()

	// The state root is a file.
	file := filepath.Join(t.TempDir(), "root")
	_ = os.WriteFile(file, nil, 0o600)
	if err := StartBackground(ctx, paths.New(file), nil, time.Second); err == nil {
		t.Fatal("unusable root must fail")
	}

	// The daemon log cannot be opened.
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.DaemonLog(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := StartBackground(ctx, l, nil, time.Second); err == nil {
		t.Fatal("unopenable log must fail")
	}

	// The child cannot be started (NUL in an argument).
	l = paths.New(filepath.Join(t.TempDir(), "root"))
	if err := StartBackground(ctx, l, []string{"bad\x00arg"}, time.Second); err == nil {
		t.Fatal("invalid argument must fail")
	}
}
