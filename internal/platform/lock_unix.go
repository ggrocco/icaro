//go:build unix

package platform

import (
	"errors"
	"os"
	"syscall"
)

// Lock is an exclusive advisory flock on a file. The kernel releases it
// when the process dies, so a crashed daemon never leaves it held.
type Lock struct {
	f *os.File
}

// TryLock takes an exclusive non-blocking flock on path, creating it.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}

// IsLocked probes whether another process holds the lock on path.
func IsLocked(path string) (bool, error) {
	l, err := TryLock(path)
	if errors.Is(err, ErrLocked) {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, l.Release()
}

// ProcessAlive reports whether a process with pid exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Detach returns process attributes that start a child in its own session,
// so it survives the parent's terminal.
func Detach() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
