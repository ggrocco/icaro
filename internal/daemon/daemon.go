// Package daemon owns the single-daemon rule (ADR 0001): the exclusive
// lock, the informational PID file, the Unix socket listener and the
// client side that reaches (or starts) the daemon.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"icaro/internal/paths"
	"icaro/internal/platform"
)

// AlreadyRunningError reports a live daemon holding the lock.
type AlreadyRunningError struct {
	PID int
}

func (e *AlreadyRunningError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("an icaro daemon is already running (pid %d)", e.PID)
	}
	return "an icaro daemon is already running"
}

// Instance is a running daemon's claim on the run directory.
type Instance struct {
	layout paths.Layout
	lock   *platform.Lock
}

// Acquire takes the daemon lock and writes the PID file. A second caller
// fails deterministically with *AlreadyRunningError.
func Acquire(l paths.Layout) (*Instance, error) {
	if err := l.Ensure(); err != nil {
		return nil, err
	}
	lock, err := platform.TryLock(l.Lock())
	if errors.Is(err, platform.ErrLocked) {
		return nil, &AlreadyRunningError{PID: platform.ReadPID(l.PID())}
	}
	if err != nil {
		return nil, fmt.Errorf("daemon lock: %w", err)
	}
	if err := platform.WritePID(l.PID()); err != nil {
		_ = lock.Release()
		return nil, err
	}
	return &Instance{layout: l, lock: lock}, nil
}

// ListenUnix binds the daemon socket (0600). A leftover socket from a dead
// daemon is safe to remove because we hold the lock.
func (i *Instance) ListenUnix() (net.Listener, error) {
	p := i.layout.Socket()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Close removes the socket and PID file and releases the lock.
func (i *Instance) Close() error {
	_ = os.Remove(i.layout.Socket())
	_ = os.Remove(i.layout.PID())
	return i.lock.Release()
}

// Status describes the run directory as seen from outside.
type Status struct {
	Running      bool // lock held
	PID          int  // from the PID file
	PIDAlive     bool
	SocketExists bool
	SocketOK     bool // answers /healthz
}

// Orphaned reports leftovers with no daemon behind them.
func (s Status) Orphaned() bool {
	return !s.Running && (s.PID != 0 || s.SocketExists)
}

// Probe inspects the lock, PID file and socket.
func Probe(ctx context.Context, l paths.Layout) (Status, error) {
	var st Status
	held, err := platform.IsLocked(l.Lock())
	if err != nil {
		return st, err
	}
	st.Running = held
	st.PID = platform.ReadPID(l.PID())
	st.PIDAlive = platform.ProcessAlive(st.PID)
	if _, err := os.Stat(l.Socket()); err == nil {
		st.SocketExists = true
		st.SocketOK = Ping(ctx, l) == nil
	}
	return st, nil
}

// CleanOrphans removes a stale PID file and socket. It refuses while a
// daemon holds the lock.
func CleanOrphans(ctx context.Context, l paths.Layout) ([]string, error) {
	st, err := Probe(ctx, l)
	if err != nil {
		return nil, err
	}
	if st.Running {
		return nil, &AlreadyRunningError{PID: st.PID}
	}
	var removed []string
	for _, p := range []string{l.PID(), l.Socket()} {
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
	}
	return removed, nil
}

// HTTPClient returns a client whose requests go to the daemon socket
// regardless of the URL host; use base URL "http://icaro".
func HTTPClient(l paths.Layout, timeout time.Duration) *http.Client {
	sock := l.Socket()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 4,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// SocketBaseURL is the placeholder base URL used with HTTPClient.
const SocketBaseURL = "http://icaro"

// Ping checks that the daemon answers on its socket.
func Ping(ctx context.Context, l paths.Layout) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SocketBaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := HTTPClient(l, 2*time.Second).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// StartBackground launches `icaro serve` detached (logging to the daemon
// log) and waits until the socket answers. extraArgs are passed before
// "serve" (e.g. --config).
func StartBackground(ctx context.Context, l paths.Layout, extraArgs []string, wait time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := l.Ensure(); err != nil {
		return err
	}
	logf, err := os.OpenFile(l.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	args := append(append([]string{}, extraArgs...), "serve")
	cmd := exec.Command(exe, args...) //nolint:gosec // re-executes ourselves
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.Env = append(os.Environ(), paths.RootEnv+"="+l.Root)
	cmd.SysProcAttr = platform.Detach()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if Ping(ctx, l) == nil {
			return nil
		}
		select {
		case err := <-exited:
			// A racing daemon may have won the lock; that is success too.
			if Ping(ctx, l) == nil {
				return nil
			}
			return fmt.Errorf("daemon exited during startup (%v); see %s", err, l.DaemonLog())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not become ready within %s; see %s", wait, l.DaemonLog())
}

// EnsureRunning returns nil when a daemon answers, starting one otherwise.
func EnsureRunning(ctx context.Context, l paths.Layout, extraArgs []string) error {
	if Ping(ctx, l) == nil {
		return nil
	}
	return StartBackground(ctx, l, extraArgs, 15*time.Second)
}
