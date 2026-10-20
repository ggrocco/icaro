// Package platform holds the small macOS/Linux abstractions the rest of
// the code needs: file locks, process liveness and OS detection.
package platform

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// ErrLocked reports that another process holds a lock.
var ErrLocked = errors.New("lock is held by another process")

// IsMacOS reports whether we run on macOS (Docker Desktop semantics apply).
func IsMacOS() bool { return runtime.GOOS == "darwin" }

// ReadPID reads a pid file; 0 when missing or malformed.
func ReadPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// WritePID writes the current pid (0600).
func WritePID(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}
