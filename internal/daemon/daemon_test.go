package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"icaro/internal/paths"
)

func TestAcquireIsExclusiveAndCleansUp(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	inst, err := Acquire(l)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(l)
	var already *AlreadyRunningError
	if !errors.As(err, &already) || already.PID != os.Getpid() {
		t.Fatalf("second Acquire = %v", err)
	}
	ln, err := inst.ListenUnix()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(l.Socket())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })}
	go func() { _ = srv.Serve(ln) }()
	ctx := context.Background()
	if err := Ping(ctx, l); err != nil {
		t.Fatalf("ping: %v", err)
	}
	st, err := Probe(ctx, l)
	if err != nil || !st.Running || !st.SocketOK || st.Orphaned() {
		t.Fatalf("probe running: %+v %v", st, err)
	}
	if _, err := CleanOrphans(ctx, l); err == nil {
		t.Fatal("CleanOrphans must refuse while the lock is held")
	}
	_ = srv.Close()
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Socket()); !os.IsNotExist(err) {
		t.Fatal("socket not removed")
	}
	again, err := Acquire(l)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	_ = again.Close()
}

func TestOrphanDetectionAndCleanup(t *testing.T) {
	l := paths.New(filepath.Join(t.TempDir(), "root"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Simulate a killed daemon: PID file and socket file, no lock holder.
	_ = os.WriteFile(l.PID(), []byte("999999\n"), 0o600)
	_ = os.WriteFile(l.Socket(), nil, 0o600)
	ctx := context.Background()
	st, err := Probe(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || !st.Orphaned() || st.SocketOK || st.PIDAlive {
		t.Fatalf("probe orphan: %+v", st)
	}
	removed, err := CleanOrphans(ctx, l)
	if err != nil || len(removed) != 2 {
		t.Fatalf("clean: %v %v", removed, err)
	}
	st, _ = Probe(ctx, l)
	if st.Orphaned() {
		t.Fatalf("still orphaned: %+v", st)
	}
}
