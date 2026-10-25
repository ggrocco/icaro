// Package desktop is the logic of the desktop app (Phase 7, ADR 0027): it
// attaches to or starts the daemon, polls it into a State, turns the State
// into a tray menu and into notifications, and carries out menu actions.
// It has no UI dependency; the Wails shell in desktop/ renders what this
// package decides, so everything here is tested without a display.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"icaro/internal/apiclient"
	"icaro/internal/config"
	"icaro/internal/daemon"
	"icaro/internal/paths"
)

// Options say how the app reaches the daemon. They mirror the CLI's
// global flags so the app and the CLI resolve the same daemon.
type Options struct {
	ConfigPath string // --config; empty uses the default
	Home       string // --home; empty uses ICARO_HOME or the default
	// Icaro is the icaro binary that starts the daemon; empty searches
	// for it (FindIcaro).
	Icaro string
}

// Daemon is the app's handle on the daemon. The app never runs an engine
// of its own (architecture §1): it talks to the daemon over the same HTTP
// API as the CLI and, when none answers, starts `icaro serve` the way the
// CLI does. The daemon's lock keeps that to one daemon, whoever starts it.
type Daemon struct {
	Client *apiclient.Client
	Layout paths.Layout
	// Remote is set when daemon.url points at a TCP daemon, which the app
	// reaches but never starts.
	Remote bool

	icaro string
	args  []string
	start func(ctx context.Context, l paths.Layout, exe string, args []string, wait time.Duration) error
	ping  func(ctx context.Context, l paths.Layout) error
}

// Open resolves the configuration like the CLI does and returns a handle;
// it does not contact or start the daemon.
func Open(o Options) (*Daemon, error) {
	if o.Home != "" {
		// The home flag also decides where the default config file lives.
		_ = os.Setenv(paths.RootEnv, o.Home)
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	if o.Home != "" {
		cfg.Home = o.Home
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	d := &Daemon{Layout: cfg.Layout(), icaro: o.Icaro, start: daemon.StartProgram, ping: daemon.Ping}
	if o.ConfigPath != "" {
		d.args = append(d.args, "--config", o.ConfigPath)
	}
	if o.Home != "" {
		d.args = append(d.args, "--home", o.Home)
	}
	if cfg.Daemon.URL != "" {
		if cfg.Daemon.Token == "" {
			return nil, errors.New("daemon.url is set but no token: set daemon.token or ICARO_DAEMON__TOKEN")
		}
		d.Remote = true
		d.Client = apiclient.New(cfg.Daemon.URL, cfg.Daemon.Token)
		return d, nil
	}
	d.Client = apiclient.NewWithHTTP(daemon.SocketBaseURL, "", daemon.HTTPClient(d.Layout, 30*time.Second))
	return d, nil
}

// Healthy reports whether the daemon answers.
func (d *Daemon) Healthy(ctx context.Context) error {
	if d.Remote {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return d.Client.Health(ctx)
	}
	return d.ping(ctx, d.Layout)
}

// Ensure attaches to a running daemon, or starts one when none answers.
// A daemon that is already running is never started twice: Ensure only
// starts one after a failed health check, and a racing start (the CLI,
// launchd) loses the lock and exits, which StartProgram treats as success.
func (d *Daemon) Ensure(ctx context.Context) error {
	err := d.Healthy(ctx)
	if err == nil {
		return nil
	}
	if d.Remote {
		return fmt.Errorf("daemon at %s: %w", d.Client.BaseURL, err)
	}
	exe := d.icaro
	if exe == "" {
		if exe, err = FindIcaro(); err != nil {
			return err
		}
	}
	return d.start(ctx, d.Layout, exe, d.args, 15*time.Second)
}

// IcaroEnv names the icaro binary explicitly.
const IcaroEnv = "ICARO_BIN"

// ErrNoIcaro reports that no icaro binary was found.
var ErrNoIcaro = errors.New("icaro binary not found: install the CLI or set " + IcaroEnv)

// FindIcaro locates the icaro binary that starts the daemon: $ICARO_BIN,
// then the one bundled next to the app (Icaro.app/Contents/MacOS/icaro),
// then the install locations of the installer and Homebrew, then PATH. A
// GUI app does not inherit the shell's PATH on macOS, hence the fixed list.
func FindIcaro() (string, error) {
	home, _ := os.UserHomeDir()
	self, _ := os.Executable()
	return findIcaro(os.Getenv(IcaroEnv), self, home, exec.LookPath)
}

func findIcaro(env, self, home string, lookPath func(string) (string, error)) (string, error) {
	if env != "" {
		if isExecutable(env) {
			return env, nil
		}
		return "", fmt.Errorf("%s=%s is not an executable file", IcaroEnv, env)
	}
	var candidates []string
	if self != "" {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "icaro"))
	}
	candidates = append(candidates, "/opt/homebrew/bin/icaro", "/usr/local/bin/icaro")
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "icaro"))
	}
	for _, c := range candidates {
		if c != self && isExecutable(c) {
			return c, nil
		}
	}
	if p, err := lookPath("icaro"); err == nil {
		return p, nil
	}
	return "", ErrNoIcaro
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
