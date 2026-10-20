package store

import (
	"context"
	"database/sql"
	"time"
)

// UsageSnapshot is one aggregated usage row (Phase 9).
type UsageSnapshot struct {
	Provider         string
	Source           string
	Day              string // YYYY-MM-DD in the configured timezone
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Messages         int64
	CostUSD          *float64 // estimate; nil when unpriced
	Confidence       string
	CollectedAt      time.Time
}

// PutUsageSnapshots upserts rows in one transaction.
func (s *Store) PutUsageSnapshots(ctx context.Context, rows []UsageSnapshot) error {
	now := ts(Now())
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			var cost any
			if r.CostUSD != nil {
				cost = *r.CostUSD
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO usage_snapshots
				(provider, source, day, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, messages, cost_usd, confidence, collected_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT (provider, source, day, model) DO UPDATE SET
				input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
				cache_read_tokens = excluded.cache_read_tokens, cache_write_tokens = excluded.cache_write_tokens,
				messages = excluded.messages, cost_usd = excluded.cost_usd, confidence = excluded.confidence,
				collected_at = excluded.collected_at`),
				r.Provider, r.Source, r.Day, r.Model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
				r.Messages, cost, r.Confidence, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListUsageSnapshots returns rows with day >= sinceDay, ordered by day.
func (s *Store) ListUsageSnapshots(ctx context.Context, sinceDay string) ([]UsageSnapshot, error) {
	rows, err := s.query(ctx, `SELECT provider, source, day, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		messages, cost_usd, confidence, collected_at FROM usage_snapshots WHERE day >= ? ORDER BY day, provider, source, model`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageSnapshot
	for rows.Next() {
		var r UsageSnapshot
		var cost sql.NullFloat64
		var collected string
		if err := rows.Scan(&r.Provider, &r.Source, &r.Day, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
			&r.CacheWriteTokens, &r.Messages, &cost, &r.Confidence, &collected); err != nil {
			return nil, err
		}
		if cost.Valid {
			v := cost.Float64
			r.CostUSD = &v
		}
		r.CollectedAt = parseTS(collected)
		out = append(out, r)
	}
	return out, rows.Err()
}
