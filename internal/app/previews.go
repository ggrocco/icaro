package app

import (
	"context"
	"log/slog"

	"icaro/internal/api"
	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/doctor"
	"icaro/internal/platform"
	"icaro/internal/preview"
	"icaro/internal/store"
)

func init() {
	RegisterWirer(30, "previews", func(_ context.Context, a *App) error {
		if !a.Cfg.Previews.Enabled {
			return nil
		}
		a.Previews = NewPreviewManager(a.Cfg, a.Store, a.Docker, a.Log)
		// The manager runs even while Docker is down: its API answers and
		// compose failures surface as preview errors.
		a.Add(a.Previews)
		if a.Sessions != nil {
			// The container↔preview seam: sessions report the worktrees they
			// create and remove (architecture §7).
			a.Sessions.SetPreviewHook(a.Previews)
		}
		api.RegisterPreviews(a.API, a.Previews)
		for _, c := range PreviewChecks(context.Background(), a.Cfg, a.Store, a.Docker) {
			a.AddCheck(c)
		}
		return nil
	})
}

// NewPreviewManager builds the preview manager from config. It watches
// the session worktrees root plus previews.roots.
func NewPreviewManager(cfg *config.Config, st *store.Store, eng *docker.Engine, log *slog.Logger) *preview.Manager {
	l := cfg.Layout()
	roots := append([]string{l.Worktrees()}, cfg.Previews.Roots...)
	opts := preview.Options{
		Layout:       l,
		Domain:       cfg.Previews.Domain,
		HTTPPort:     cfg.Previews.HTTPPort,
		TraefikImage: cfg.Previews.TraefikImage,
		AllowRoots:   cfg.Mounts.Allow,
		MacOS:        platform.IsMacOS(),
		Roots:        roots,
		Debounce:     cfg.Previews.Debounce,
	}
	var compose preview.Compose = preview.CLI{}
	if cfg.Docker.Host != "" {
		compose = preview.CLI{Env: []string{"DOCKER_HOST=" + cfg.Docker.Host}}
	}
	return preview.New(opts, st, compose, eng, log)
}

// PreviewChecks are the Phase 5 doctor checks: the dedicated proxy
// network, compose files blocked by the lint, and .test DNS.
func PreviewChecks(_ context.Context, cfg *config.Config, st *store.Store, eng *docker.Engine) []doctor.Check {
	if !cfg.Previews.Enabled {
		return nil
	}
	var checks []doctor.Check
	if eng != nil {
		checks = append(checks, preview.NetworkCheck(eng, preview.ProxyName(cfg.Home)))
	}
	if st != nil {
		checks = append(checks, preview.ComposeCheck(st))
	}
	return append(checks, preview.DNSCheck(cfg.Previews.Domain, nil))
}
