package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockEdgeCases(t *testing.T) {
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil release: %v", err)
	}
	l, err := TryLock(filepath.Join(t.TempDir(), "x.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
	// A directory cannot be opened for writing: a real error, not "free".
	if held, err := IsLocked(t.TempDir()); err == nil || held {
		t.Fatalf("IsLocked(dir) = %v, %v", held, err)
	}
}

func TestOSHelpers(t *testing.T) {
	if IsMacOS() != (runtime.GOOS == "darwin") {
		t.Fatal("IsMacOS")
	}
	if a := Detach(); a == nil || !a.Setsid {
		t.Fatalf("Detach = %+v", a)
	}
	if ProcessAlive(-1) {
		t.Fatal("negative pid is not alive")
	}
	p := filepath.Join(t.TempDir(), "pid")
	_ = os.WriteFile(p, []byte("-5\n"), 0o600)
	if ReadPID(p) != 0 {
		t.Fatal("non-positive pid should read 0")
	}
}

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
