//go:build windows

package platform

import (
	"errors"
	"syscall"
)

// ErrNoDaemon reports that the daemon does not run on this platform: on
// Windows icaro is a client of a daemon running elsewhere (ADR 0039).
var ErrNoDaemon = errors.New("the icaro daemon does not run on Windows")

// Lock is the daemon's lock. No daemon runs on Windows, so none is ever
// held; the type exists so the daemon package compiles.
type Lock struct{}

// TryLock always fails: there is no daemon to hold the lock.
func TryLock(string) (*Lock, error) { return nil, ErrNoDaemon }

// Release is a no-op.
func (l *Lock) Release() error { return nil }

// IsLocked reports false: no local daemon can hold the lock.
func IsLocked(string) (bool, error) { return false, nil }

// ProcessAlive reports false: no local daemon process is tracked.
func ProcessAlive(int) bool { return false }

// Detach returns default process attributes.
func Detach() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
