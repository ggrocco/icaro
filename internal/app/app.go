// Package app composes the daemon: it builds every component from the
// configuration, owns their lifecycle, and exposes one HTTP handler and
// one list of doctor checks. Components never construct each other.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"icaro/internal/api"
	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/crypto"
	"icaro/internal/doctor"
	"icaro/internal/mcp"
	"icaro/internal/memory"
	"icaro/internal/paths"
	"icaro/internal/runner"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/store"
)

// Component is a long-running part of the daemon. Run blocks until ctx is
// cancelled; returning an error stops the daemon.
type Component interface {
	Name() string
	Run(ctx context.Context) error
}

// App is the composed daemon.
type App struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Layout  paths.Layout
	Store   *store.Store
	Key     *crypto.Key
	Docker  *docker.Engine
	Secrets *secrets.Store
	Memory  *memory.Engine
	MCP     *mcp.Server // host MCP server at /mcp; Phase 4 adds authoring tools
	Service *service.Service
	API     *api.Server

	components []Component
	checks     []doctor.Check
	closers    []func() error
}

// New builds the daemon from cfg: layout, store (migrated), secrets,
// Docker client, service layer, API and background components.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, Log: log, Layout: cfg.Layout()}
	if err := a.Layout.Ensure(); err != nil {
		return nil, err
	}
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	a.Store = st
	a.closers = append(a.closers, st.Close)
	if err := st.Migrate(ctx); err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	key, err := crypto.LoadMasterKey(a.Layout.Secrets(), true)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.Key = key

	eng, err := docker.New(cfg.Docker.Host)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.Docker = eng
	a.closers = append(a.closers, eng.Close)

	rn := runner.New(runner.Config{
		RunnerID:      "daemon",
		Concurrency:   cfg.Workflows.Concurrency,
		StepTimeout:   cfg.Limits.Timeout,
		MemoryBytes:   cfg.Limits.MemoryMB << 20,
		NanoCPUs:      int64(cfg.Limits.CPUs * 1e9),
		Pids:          cfg.Limits.Pids,
		NoFile:        cfg.Limits.NoFile,
		StrictRuntime: cfg.Docker.StrictRuntime,
		LogsDir:       a.Layout.Logs(),
	}, st, eng, nil, nil, log)
	a.Service = service.New(service.Options{Store: st, Key: key, Runner: rn, LogsDir: a.Layout.Logs(), MaxTimeout: cfg.Limits.Timeout, Log: log})
	a.API = api.New(a.Service, log)
	a.Add(&dockerGated{name: "workflow-runner", docker: eng, log: log, run: rn.Run})

	a.checks = append(a.checks, BaseChecks(cfg, st, eng)...)
	for _, w := range sortedWirers() {
		if err := w.fn(ctx, a); err != nil {
			_ = a.Close()
			return nil, fmt.Errorf("wire %s: %w", w.name, err)
		}
	}
	return a, nil
}

// wirer attaches one area (sessions, previews, memory, ...) to the app.
type wirer struct {
	order int
	name  string
	fn    func(ctx context.Context, a *App) error
}

var wirers []wirer

// RegisterWirer is called from init() in internal/app/<area>.go. Lower
// order runs first: 10 secrets, 20 sessions, 30 previews, 40 memory,
// 50 integrations, 60 workflows, 70 mcp/ui.
func RegisterWirer(order int, name string, fn func(ctx context.Context, a *App) error) {
	wirers = append(wirers, wirer{order: order, name: name, fn: fn})
}

func sortedWirers() []wirer {
	out := append([]wirer(nil), wirers...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

// AddCloser registers a cleanup run on Close (reverse order).
func (a *App) AddCloser(fn func() error) { a.closers = append(a.closers, fn) }

// Add registers a background component.
func (a *App) Add(c Component) { a.components = append(a.components, c) }

// AddCheck registers a doctor check.
func (a *App) AddCheck(c doctor.Check) { a.checks = append(a.checks, c) }

// Checks returns every registered doctor check.
func (a *App) Checks() []doctor.Check { return a.checks }

// Handler is the daemon's HTTP handler.
func (a *App) Handler() http.Handler { return a.API.Handler() }

// Run starts every component and waits. The first component error cancels
// the rest.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(a.components))
	for _, c := range a.components {
		go func(c Component) {
			err := c.Run(ctx)
			if err != nil && ctx.Err() == nil {
				err = fmt.Errorf("%s: %w", c.Name(), err)
			} else {
				err = nil
			}
			errCh <- err
		}(c)
	}
	var first error
	for range a.components {
		if err := <-errCh; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// Close releases resources in reverse order.
func (a *App) Close() error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		errs = append(errs, a.closers[i]())
	}
	a.closers = nil
	return errors.Join(errs...)
}

// ConnContext marks Unix-socket connections as local (see api.ConnContext).
func ConnContext(ctx context.Context, c net.Conn) context.Context { return api.ConnContext(ctx, c) }

// OpenStore opens the configured database.
func OpenStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	if err := cfg.Layout().Ensure(); err != nil {
		return nil, err
	}
	dsn := cfg.Database.DSN
	if cfg.Database.Driver == "sqlite" {
		dsn = cfg.SQLitePath()
	}
	return store.Open(ctx, store.Dialect(cfg.Database.Driver), dsn)
}

// dockerGated runs fn once the Docker engine answers, retrying the ping,
// so the daemon (and its API, doctor, memory) works while Docker is down.
type dockerGated struct {
	name   string
	docker interface{ Ping(context.Context) error }
	log    *slog.Logger
	run    func(context.Context) error
}

func (d *dockerGated) Name() string { return d.name }

func (d *dockerGated) Run(ctx context.Context) error {
	warned := false
	for {
		if err := d.docker.Ping(ctx); err == nil {
			return d.run(ctx)
		} else if !warned {
			d.log.Warn("docker is not reachable; waiting", "component", d.name, "err", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// BaseChecks are the Phase 0 doctor checks.
func BaseChecks(cfg *config.Config, st *store.Store, eng interface{ Ping(context.Context) error }) []doctor.Check {
	l := cfg.Layout()
	checks := []doctor.Check{doctor.DaemonCheck(l), doctor.PermissionsCheck(l)}
	if eng != nil {
		checks = append(checks, doctor.Func("docker", func(ctx context.Context) doctor.Result {
			if err := eng.Ping(ctx); err != nil {
				return doctor.Result{Severity: doctor.Error, Detail: "docker is not reachable: " + err.Error() + "; " + dockerHint()}
			}
			return doctor.Result{Severity: doctor.OK, Detail: "engine reachable"}
		}))
	}
	if st != nil {
		checks = append(checks, doctor.Func("database migrations", func(ctx context.Context) doctor.Result {
			n, err := st.PendingMigrations(ctx)
			if err != nil {
				return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
			}
			if n > 0 {
				return doctor.Result{Severity: doctor.Error, Detail: fmt.Sprintf("%d pending migration(s)", n), Fix: func(ctx context.Context) (string, error) {
					return "applied migrations", st.Migrate(ctx)
				}}
			}
			return doctor.Result{Severity: doctor.OK, Detail: "up to date"}
		}))
	}
	return checks
}

func dockerHint() string {
	if _, err := os.Stat("/Applications/Docker.app"); err == nil {
		return "start Docker Desktop"
	}
	return "install Docker Engine and start it (e.g. `sudo systemctl start docker`), and make sure your user can reach the socket"
}

// CheckFactory builds doctor checks for a component from shared handles;
// used by `icaro doctor`, which runs without the daemon.
type CheckFactory func(ctx context.Context, cfg *config.Config, st *store.Store, eng *docker.Engine) []doctor.Check
