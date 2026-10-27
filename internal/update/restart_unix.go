//go:build unix

package update

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"icaro/internal/autostart"
	"icaro/internal/daemon"
	"icaro/internal/paths"
	"icaro/internal/platform"
)

// Supervisor is the login service that may own the daemon
// (*autostart.Manager; faked in tests).
type Supervisor interface {
	Status(ctx context.Context) (autostart.Status, error)
	Restart(ctx context.Context) error
}

// RestartDaemon restarts a running daemon on the new binary. When the
// login service (sv, may be nil) is installed and active, the service
// manager restarts it, so it stays supervised. Otherwise it stops the
// daemon gracefully (SIGTERM, then waits for the lock) and starts exe in
// the background. Sessions re-attach through the startup reconciliation
// pass (ADR 0006). It reports false when no daemon was running.
func RestartDaemon(ctx context.Context, l paths.Layout, exe string, globalArgs []string, sv Supervisor) (bool, error) {
	st, err := daemon.Probe(ctx, l)
	if err != nil {
		return false, err
	}
	if !st.Running {
		return false, nil
	}
	if sv != nil {
		if s, err := sv.Status(ctx); err == nil && s.Installed && s.Active {
			if err := sv.Restart(ctx); err != nil {
				return true, fmt.Errorf("restart the login service: %w", err)
			}
			return true, nil
		}
	}
	if st.PID <= 0 {
		return true, errors.New("daemon holds the lock but its pid is unknown; restart it manually")
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
		return true, fmt.Errorf("stop daemon (pid %d): %w", st.PID, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		held, err := platform.IsLocked(l.Lock())
		if err != nil {
			return true, err
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("daemon (pid %d) did not stop within 30s", st.PID)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	args := append(append([]string{}, globalArgs...), "serve", "--background")
	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput() //nolint:gosec // our own binary
	if err != nil {
		return true, fmt.Errorf("start daemon: %v: %s", err, out)
	}
	return true, nil
}
