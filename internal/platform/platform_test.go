package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if held, err := IsLocked(p); err != nil || !held {
		t.Fatalf("IsLocked = %v, %v", held, err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if held, err := IsLocked(p); err != nil || held {
		t.Fatalf("after release IsLocked = %v, %v", held, err)
	}
	if held, err := IsLocked(filepath.Join(t.TempDir(), "missing", "x")); err != nil || held {
		t.Fatalf("missing dir: %v %v", held, err)
	}
}

func TestPID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pid")
	if ReadPID(p) != 0 {
		t.Fatal("missing pid file should read 0")
	}
	if err := WritePID(p); err != nil {
		t.Fatal(err)
	}
	if got := ReadPID(p); got != os.Getpid() {
		t.Fatalf("pid = %d", got)
	}
	if !ProcessAlive(os.Getpid()) || ProcessAlive(0) {
		t.Fatal("liveness")
	}
	_ = os.WriteFile(p, []byte("junk"), 0o600)
	if ReadPID(p) != 0 {
		t.Fatal("malformed pid should read 0")
	}
}
