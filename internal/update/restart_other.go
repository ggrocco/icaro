//go:build !unix

package update

import (
	"context"

	"icaro/internal/autostart"
	"icaro/internal/paths"
)

// Supervisor is the login service that may own the daemon.
type Supervisor interface {
	Status(ctx context.Context) (autostart.Status, error)
	Restart(ctx context.Context) error
}

// RestartDaemon reports false: no daemon runs on this platform (ADR 0039),
// so there is none to restart.
func RestartDaemon(context.Context, paths.Layout, string, []string, Supervisor) (bool, error) {
	return false, nil
}
