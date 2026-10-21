package app

import (
	"context"

	"icaro/internal/api"
	"icaro/internal/usage"
)

func init() {
	RegisterWirer(45, "usage", func(_ context.Context, a *App) error {
		api.RegisterUsage(a.API, func(ctx context.Context, since string) (*usage.Report, error) {
			// Profile homes only: the host's ~/.claude stays an explicit
			// CLI opt-in (icaro usage --host-claude).
			rep, err := usage.BuildReport(ctx, usage.ReportOptions{
				Roots:    usage.ProfileRoots(a.Layout.Homes()),
				Timezone: a.Cfg.Usage.Timezone,
				Since:    since,
				LimitUSD: a.Cfg.Usage.DailyLimit,
			})
			if err != nil {
				return nil, err
			}
			// Snapshots outlive transcripts; a failed save never hides
			// the report.
			if err := usage.Save(ctx, a.Store, rep.Records); err != nil {
				a.Log.Warn("usage: could not save snapshots", "err", err)
			}
			return rep, nil
		})
		return nil
	})
}
