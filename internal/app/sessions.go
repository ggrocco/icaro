package app

import (
	"context"
	"fmt"
	"strings"

	"icaro/internal/api"
	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/doctor"
	"icaro/internal/secrets"
	"icaro/internal/session"
	"icaro/internal/store"
)

func init() {
	RegisterWirer(20, "sessions", func(_ context.Context, a *App) error {
		a.Egress = &container.DockerEgress{Engine: a.Docker, HelperImage: a.Cfg.Docker.HelperImage, Log: a.Log}
		a.Images = &container.Images{Engine: a.Docker, Pins: a.Store}
		a.Homes = &creds.Homes{Layout: a.Layout, Mode: a.Cfg.Sessions.HomeMode, Volumes: a.Docker}
		// Preview and Memory hooks are attached by their wirers (orders 30
		// and 40) through a.Sessions.SetPreviewHook / SetMemoryAttacher.
		a.Sessions = session.New(session.Options{
			Store: a.Store, Config: a.Cfg, Engine: a.Docker, Egress: a.Egress, Homes: a.Homes, Images: a.Images, Log: a.Log,
		})
		a.API.RegisterSessions(api.SessionDeps{
			Sessions: a.Sessions, Secrets: a.Secrets, Egress: a.Egress, Images: a.Images, Homes: a.Homes, Config: a.Cfg,
		})
		a.Add(&dockerGated{name: "sessions", docker: a.Docker, log: a.Log, run: a.Sessions.Run})
		for _, c := range SessionChecks(a.Cfg, a.Store, a.Docker) {
			a.AddCheck(c)
		}
		return nil
	})
}

// SessionChecks are the Phase 1 doctor checks: the master key backend,
// orphaned sessions and pinned images.
func SessionChecks(cfg *config.Config, st *store.Store, eng *docker.Engine) []doctor.Check {
	checks := []doctor.Check{doctor.Func("master key", func(ctx context.Context) doctor.Result {
		_, info, err := secrets.LoadMasterKey(ctx, secrets.KeyOptions{Dir: cfg.Layout().Secrets(), Keyring: secrets.DefaultKeyring(nil)})
		switch {
		case err != nil:
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		case info.Warning != "":
			return doctor.Result{Severity: doctor.Warning, Detail: info.Warning}
		}
		return doctor.Result{Severity: doctor.OK, Detail: "stored in " + info.Backend}
	})}
	if st == nil {
		return checks
	}
	checks = append(checks, doctor.Func("orphaned sessions", func(ctx context.Context) doctor.Result {
		rows, err := st.ListSessions(ctx, store.SessionFilter{Statuses: []string{session.StatusOrphaned}})
		if err != nil {
			return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
		}
		if len(rows) == 0 {
			return doctor.Result{Severity: doctor.OK, Detail: "none"}
		}
		var ids []string
		for _, r := range rows {
			ids = append(ids, r.ID+" ("+r.Error+")")
		}
		res := doctor.Result{Severity: doctor.Warning, Detail: fmt.Sprintf("%d orphaned: %s; `icaro session start <id>` re-attaches, --fix cleans up", len(rows), strings.Join(ids, ", "))}
		if eng != nil {
			res.Fix = func(ctx context.Context) (string, error) {
				for _, r := range rows {
					if err := session.CleanupOrphan(ctx, st, eng, r.ID); err != nil {
						return "", err
					}
				}
				return fmt.Sprintf("cleaned up %d session(s); worktrees kept", len(rows)), nil
			}
		}
		return res
	}))
	if eng != nil {
		imgs := &container.Images{Engine: eng, Pins: st}
		checks = append(checks, doctor.Func("pinned images", func(ctx context.Context) doctor.Result {
			if err := eng.Ping(ctx); err != nil {
				return doctor.Result{Severity: doctor.Skipped, Detail: "docker unreachable"}
			}
			probs, err := imgs.Verify(ctx)
			if err != nil {
				return doctor.Result{Severity: doctor.Error, Detail: err.Error()}
			}
			if len(probs) == 0 {
				return doctor.Result{Severity: doctor.OK, Detail: "every pinned digest is present"}
			}
			var parts []string
			for _, p := range probs {
				parts = append(parts, p.Ref+": "+p.Reason)
			}
			return doctor.Result{Severity: doctor.Error, Detail: "unverified image: " + strings.Join(parts, "; "), Fix: func(ctx context.Context) (string, error) {
				for _, p := range probs {
					if err := imgs.Repair(ctx, p); err != nil {
						return "", err
					}
				}
				return fmt.Sprintf("re-pulled %d image(s) by digest", len(probs)), nil
			}}
		}))
	}
	return checks
}
