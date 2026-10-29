package app

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"icaro/internal/container"
	"icaro/internal/container/sandbox"
	"icaro/internal/session"
	"icaro/internal/workflow"
	"icaro/internal/workflow/engine"
	"icaro/internal/workflow/events"
)

// WorkflowWiring is filled by earlier wirers (sessions at 20,
// integrations at 50) and consumed by the workflows wirer at 60. Nil
// fields degrade safely: no Sessions => scratch workspaces under
// ~/.icaro/runs, no Egress => step containers get network "none", no
// Actions => uses steps fail, no Catalog => uses/event checks skipped.
type WorkflowWiring struct {
	Sessions session.RunSessions
	Egress   container.Egress
	Actions  workflow.ActionExecutor
	Catalog  workflow.Catalog

	Events *events.Bus
	Engine *engine.Engine
}

func init() {
	RegisterWirer(60, "workflows", wireWorkflows)
}

func wireWorkflows(_ context.Context, a *App) error {
	cfg := a.Cfg
	profile := cfg.Workflows.Profile
	p, ok := cfg.Profiles[profile]
	if !ok {
		return fmt.Errorf("workflows.profile %q is not a profile", profile)
	}
	busCtx, stop := context.WithCancel(context.Background())
	a.AddCloser(func() error { stop(); return nil })
	bus := events.New(busCtx)

	w := &a.Workflows
	w.Events = bus
	deps := engine.Deps{
		Store: a.Store, Docker: a.Docker, Sessions: w.Sessions, Egress: w.Egress,
		Actions: w.Actions, Catalog: w.Catalog, Events: bus, Log: a.Log,
	}
	if a.Secrets != nil {
		deps.Secrets = a.Secrets
	}
	w.Engine = engine.New(engine.Config{
		Concurrency:    cfg.Workflows.Concurrency,
		MaxRuns:        cfg.Workflows.MaxRuns,
		RunParallelism: cfg.Workflows.RunParallelism,
		StepTimeout:    cfg.Limits.Timeout,
		Limits: sandbox.Limits{
			MemoryBytes: cfg.Limits.MemoryMB << 20,
			NanoCPUs:    int64(cfg.Limits.CPUs * 1e9),
			Pids:        cfg.Limits.Pids,
			NoFile:      cfg.Limits.NoFile,
		},
		Profile: profile,
		Allow:   p.Egress,
		Runtime: p.Runtime,
		User:    stepUser(a),
		LogsDir: a.Layout.Logs(),
		RunsDir: a.Layout.Runs(),
	}, deps)

	a.Service.SetRunner(w.Engine)
	a.Service.SetEvents(bus)
	if a.Secrets != nil {
		a.Service.SetSecrets(a.Secrets)
	}
	if w.Catalog != nil {
		a.Service.SetCatalog(w.Catalog)
	}
	a.Add(&dockerGated{name: "workflow-engine", docker: a.Docker, log: a.Log, run: w.Engine.Run})
	return nil
}

// stepUser is the uid:gid step containers run as. With the session spine
// it is the user the run workspace is owned by (never root, so a root
// daemon still produces non-root steps that can write /workspace).
func stepUser(a *App) string {
	if a.Sessions != nil {
		return a.Sessions.User()
	}
	return containerUser()
}

// containerUser runs step containers as the daemon's user on Linux so
// files written to the bind-mounted workspace keep the user's ownership.
// Docker Desktop on macOS maps ownership itself.
func containerUser() string {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
