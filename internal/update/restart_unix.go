//go:build unix

package update

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"icaro/internal/daemon"
	"icaro/internal/paths"
	"icaro/internal/platform"
)

// RestartDaemon stops a running daemon gracefully (SIGTERM, then waits for
// the lock) and starts exe in the background. Sessions re-attach through
// the startup reconciliation pass (ADR 0006). It reports false when no
// daemon was running.
func RestartDaemon(ctx context.Context, l paths.Layout, exe string, globalArgs []string) (bool, error) {
	st, err := daemon.Probe(ctx, l)
	if err != nil {
		return false, err
	}
	if !st.Running {
		return false, nil
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
