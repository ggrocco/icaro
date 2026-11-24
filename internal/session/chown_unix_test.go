//go:build unix

package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A root daemon hands a session's paths to the container user, whatever
// uid the tests run as; a non-root daemon leaves them alone.
func TestChownForContainer(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 4242, 4242
	}
	owner := func() int {
		t.Helper()
		fi, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		return int(fi.Sys().(*syscall.Stat_t).Uid)
	}
	before := owner()
	m := &Manager{o: Options{UID: uid, GID: gid}}

	daemonUID = func() int { return 1000 }
	m.chownForContainer(dir)
	if owner() != before {
		t.Fatal("a non-root daemon changed ownership")
	}

	daemonUID = func() int { return 0 }
	defer func() { daemonUID = os.Getuid }()
	m.chownForContainer(dir, filepath.Join(dir, "missing"))
	if owner() != uid {
		t.Fatalf("owner %d, want %d", owner(), uid)
	}
}
