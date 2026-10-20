package usage

import (
	"context"

	"icaro/internal/store"
)

// Save persists collected records as snapshots; unavailable rows are
// reported but never stored, so they cannot hide earlier good data.
func Save(ctx context.Context, st *store.Store, recs []Record) error {
	rows := make([]store.UsageSnapshot, 0, len(recs))
	for _, r := range recs {
		if r.Error != "" || r.Day == "" {
			continue
		}
		rows = append(rows, store.UsageSnapshot{
			Provider: r.Provider, Source: r.Source, Day: r.Day, Model: r.Model,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CacheReadTokens: r.CacheReadTokens,
			CacheWriteTokens: r.CacheWriteTokens, Messages: r.Messages, CostUSD: r.CostUSD, Confidence: r.Confidence,
		})
	}
	return st.PutUsageSnapshots(ctx, rows)
}

// Load reads stored snapshots from sinceDay (YYYY-MM-DD).
func Load(ctx context.Context, st *store.Store, sinceDay string) ([]Record, error) {
	rows, err := st.ListUsageSnapshots(ctx, sinceDay)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, Record{
			Provider: r.Provider, Source: r.Source, Day: r.Day, Model: r.Model,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CacheReadTokens: r.CacheReadTokens,
			CacheWriteTokens: r.CacheWriteTokens, Messages: r.Messages, CostUSD: r.CostUSD, Confidence: r.Confidence,
		})
	}
	return out, nil
}
