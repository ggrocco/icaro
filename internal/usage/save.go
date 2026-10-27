package usage

import (
	"context"

	"icaro/internal/store"
)

// Save persists collected records as snapshots; unavailable rows are
// reported but never stored, so they cannot hide earlier good data, and
// rows read back from snapshots are already stored.
func Save(ctx context.Context, st *store.Store, recs []Record) error {
	rows := make([]store.UsageSnapshot, 0, len(recs))
	for _, r := range recs {
		if r.Error != "" || r.Day == "" || r.Confidence == ConfidenceSnapshot {
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

// MergeSnapshots adds the stored rows that no live record covers: a
// (day, provider, source, model) whose transcript has since been deleted
// or rotated. Only sources in the report (sources) are considered, so a
// report never shows a root it did not ask for. Merged rows carry
// ConfidenceSnapshot.
func MergeSnapshots(live, stored []Record, sources map[string]bool) []Record {
	type key struct{ day, provider, source, model string }
	seen := make(map[key]bool, len(live))
	for _, r := range live {
		if r.Error == "" {
			seen[key{r.Day, r.Provider, r.Source, r.Model}] = true
		}
	}
	out := live
	for _, r := range stored {
		if !sources[r.Source] || seen[key{r.Day, r.Provider, r.Source, r.Model}] {
			continue
		}
		r.Confidence = ConfidenceSnapshot
		r.Error = ""
		out = append(out, r)
	}
	sortRecords(out)
	return out
}
