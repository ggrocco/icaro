package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icaro/internal/paths"
	"icaro/internal/platform"
)

func TestAlreadyRunningError(t *testing.T) {
	if got := (&AlreadyRunningError{PID: 42}).Error(); got != "an icaro daemon is already running (pid 42)" {
		t.Fatal(got)
	}
	if got := (&AlreadyRunningError{}).Error(); got != "an icaro daemon is already running" {
		t.Fatal(got)
	}
}

func TestAcquireErrors(t *testing.T) {
	// The state root is a file.
	file := filepath.Join(t.TempDir(), "root")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := Acquire(paths.New(file)); err == nil {
		t.Fatal("unusable root must fail")
	}

	// The lock path is a directory: a real error, not "already running".
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(l.Lock(), 0o700)
	_, err := Acquire(l)
	var already *AlreadyRunningError
	if err == nil || errors.As(err, &already) || !strings.Contains(err.Error(), "daemon lock") {
		t.Fatalf("lock dir: %v", err)
	}

	// The PID file cannot be written: the lock is released again.
	l = paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(l.PID(), 0o700)
	if _, err := Acquire(l); err == nil {
		t.Fatal("unwritable PID file must fail")
	}
	if held, err := platform.IsLocked(l.Lock()); err != nil || held {
		t.Fatalf("lock still held after failure: %v %v", held, err)
	}
}

func TestListenUnixErrors(t *testing.T) {
	// Over the portable sun_path limit.
	long := paths.New(filepath.Join(t.TempDir(), strings.Repeat("d", 110)))
	inst, err := Acquire(long)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.ListenUnix(); err == nil || !strings.Contains(err.Error(), "Unix limit") {
		t.Fatalf("long path: %v", err)
	}
	_ = inst.Close()

	// A non-empty directory squats the socket path and cannot be removed.
	l := paths.New(shortTempDir(t))
	inst, err = Acquire(l)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	_ = os.MkdirAll(filepath.Join(l.Socket(), "x"), 0o700)
	if _, err := inst.ListenUnix(); err == nil {
		t.Fatal("squatted socket path must fail")
	}

	// The run directory vanished under the daemon.
	_ = os.RemoveAll(l.RunDir())
	if _, err := inst.ListenUnix(); err == nil {
		t.Fatal("missing run dir must fail")
	}
}

func TestListenUnixReplacesStaleSocket(t *testing.T) {
	l := paths.New(shortTempDir(t))
	inst, err := Acquire(l)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	_ = os.WriteFile(l.Socket(), []byte("stale"), 0o600)
	ln, err := inst.ListenUnix()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}

func TestProbeAndCleanErrors(t *testing.T) {
	ctx := context.Background()
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(l.Lock(), 0o700)
	if _, err := Probe(ctx, l); err == nil {
		t.Fatal("unreadable lock must fail Probe")
	}
	if _, err := CleanOrphans(ctx, l); err == nil {
		t.Fatal("unreadable lock must fail CleanOrphans")
	}

	// The PID file is removed, then the socket path cannot be.
	l = paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(l.PID(), []byte("999999\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(l.Socket(), "x"), 0o700)
	removed, err := CleanOrphans(ctx, l)
	if err == nil || len(removed) != 1 || removed[0] != l.PID() {
		t.Fatalf("clean = %v %v", removed, err)
	}
}

func TestPingUnhealthy(t *testing.T) {
	l := paths.New(shortTempDir(t))
	inst, err := Acquire(l)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close() }()
	ln, err := inst.ListenUnix()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	if err := Ping(context.Background(), l); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("ping = %v", err)
	}
	st, err := Probe(context.Background(), l)
	if err != nil || !st.SocketExists || st.SocketOK {
		t.Fatalf("probe = %+v %v", st, err)
	}
}
