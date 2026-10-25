package app

import (
	"context"
	"time"

	"icaro/internal/api"
	"icaro/internal/usage"
)

// usageSnapshotEvery is how often the daemon saves usage snapshots.
var usageSnapshotEvery = time.Hour

func init() {
	RegisterWirer(45, "usage", func(_ context.Context, a *App) error {
		// Profile homes only: the host's ~/.claude stays an explicit CLI
		// opt-in (icaro usage --host-claude).
		opts := func(since string) usage.ReportOptions {
			return usage.ReportOptions{
				Roots:    usage.ProfileRoots(a.Layout.Homes()),
				Timezone: a.Cfg.Usage.Timezone,
				Since:    since,
				LimitUSD: a.Cfg.Usage.DailyLimit,
				Store:    a.Store,
			}
		}
		// GET /api/v1/usage is read scope, so it only reads: live
		// transcripts plus stored snapshots for rows whose transcript is
		// gone. Snapshots are written by the periodic job below and by
		// `icaro usage`.
		api.RegisterUsage(a.API, func(ctx context.Context, since string) (*usage.Report, error) {
			return usage.BuildReport(ctx, opts(since))
		})
		a.Add(&usageSnapshots{a: a, opts: opts})
		return nil
	})
}

// usageSnapshots saves the default window's usage at start and then
// hourly, so snapshots outlive transcripts even when nobody runs
// `icaro usage`.
type usageSnapshots struct {
	a    *App
	opts func(since string) usage.ReportOptions
}

func (u *usageSnapshots) Name() string { return "usage-snapshots" }

func (u *usageSnapshots) Run(ctx context.Context) error {
	for {
		u.save(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(usageSnapshotEvery):
		}
	}
}

// save never fails the daemon: a bad usage.timezone or a store error is
// logged and retried on the next tick.
func (u *usageSnapshots) save(ctx context.Context) {
	rep, err := usage.BuildReport(ctx, u.opts(usage.DefaultSince))
	if err == nil {
		err = usage.Save(ctx, u.a.Store, rep.Records)
	}
	if err != nil && ctx.Err() == nil {
		u.a.Log.Warn("usage: could not save snapshots", "err", err)
	}
}
