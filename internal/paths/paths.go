// Package paths lays out Ícaro's local state under one root directory
// (default ~/.icaro). Every component asks this package for its paths so
// the layout is defined in one place.
package paths

import (
	"os"
	"path/filepath"
)

// RootEnv overrides the state root.
const RootEnv = "ICARO_HOME"

// Layout is the on-disk layout rooted at Root.
type Layout struct {
	Root string
}

// Default returns the layout at $ICARO_HOME or ~/.icaro.
func Default() Layout {
	if v := os.Getenv(RootEnv); v != "" {
		return Layout{Root: v}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{Root: ".icaro"}
	}
	return Layout{Root: filepath.Join(home, ".icaro")}
}

// New returns the layout at root.
func New(root string) Layout { return Layout{Root: root} }

// RunDir holds the daemon lock, PID and socket.
func (l Layout) RunDir() string { return filepath.Join(l.Root, "run") }

// Lock is the daemon lock file; holding its flock is the source of truth
// for "a daemon is running".
func (l Layout) Lock() string { return filepath.Join(l.RunDir(), "icaro.lock") }

// PID is informational: the pid of the daemon that holds the lock.
func (l Layout) PID() string { return filepath.Join(l.RunDir(), "icaro.pid") }

// Socket is the daemon's Unix socket.
func (l Layout) Socket() string { return filepath.Join(l.RunDir(), "icaro.sock") }

// ConfigDir holds the config file and the database.
func (l Layout) ConfigDir() string { return filepath.Join(l.Root, "config") }

// ConfigFile is the default config file.
func (l Layout) ConfigFile() string { return filepath.Join(l.ConfigDir(), "icaro.yaml") }

// DB is the default SQLite database.
func (l Layout) DB() string { return filepath.Join(l.ConfigDir(), "icaro.db") }

// Logs holds step logs and the daemon log.
func (l Layout) Logs() string { return filepath.Join(l.Root, "logs") }

// DaemonLog is where a background daemon writes its log.
func (l Layout) DaemonLog() string { return filepath.Join(l.Logs(), "daemon.log") }

// Homes holds the per-profile persistent homes (ADR 0005).
func (l Layout) Homes() string { return filepath.Join(l.Root, "home") }

// Home is one profile's home directory.
func (l Layout) Home(profile string) string { return filepath.Join(l.Homes(), profile) }

// Worktrees holds the git worktrees created for sessions.
func (l Layout) Worktrees() string { return filepath.Join(l.Root, "worktrees") }

// Repos holds the repository cache (bare clones keyed by remote).
func (l Layout) Repos() string { return filepath.Join(l.Root, "repos") }

// Runs holds per-run scratch state (workspaces of runs without a repository, I/O dirs).
func (l Layout) Runs() string { return filepath.Join(l.Root, "runs") }

// Integrations holds the integration packs (ADR 0007).
func (l Layout) Integrations() string { return filepath.Join(l.Root, "integrations") }

// Secrets holds the encrypted-file secret store fallback.
func (l Layout) Secrets() string { return filepath.Join(l.Root, "secrets") }

// Previews holds generated compose overrides.
func (l Layout) Previews() string { return filepath.Join(l.Root, "previews") }

// Skills holds skill bundles written by `icaro skill install`.
func (l Layout) Skills() string { return filepath.Join(l.Root, "skills") }

// PrivateDirs are created with 0700.
func (l Layout) PrivateDirs() []string {
	return []string{l.Root, l.RunDir(), l.ConfigDir(), l.Logs(), l.Homes(), l.Worktrees(), l.Repos(), l.Runs(), l.Integrations(), l.Secrets(), l.Previews()}
}

// Ensure creates the private directories and tightens their mode.
func (l Layout) Ensure() error {
	for _, d := range l.PrivateDirs() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil { //nolint:gosec // directories need the execute bit
			return err
		}
	}
	return nil
}
