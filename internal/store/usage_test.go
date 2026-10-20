package store

import (
	"context"
	"testing"
)

func TestUsageSnapshots(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		cost := 1.25
		rows := []UsageSnapshot{
			{Provider: "claude-code", Source: "profile:claude", Day: "2025-03-01", Model: "m1", InputTokens: 10, OutputTokens: 5, Messages: 1, CostUSD: &cost, Confidence: "high"},
			{Provider: "claude-code", Source: "profile:claude", Day: "2025-03-02", Model: "m2", InputTokens: 1, Confidence: "high"},
		}
		if err := s.PutUsageSnapshots(ctx, rows); err != nil {
			t.Fatal(err)
		}
		// Re-collection replaces the row instead of adding to it.
		rows[0].InputTokens = 20
		if err := s.PutUsageSnapshots(ctx, rows[:1]); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListUsageSnapshots(ctx, "2025-03-01")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].InputTokens != 20 || got[0].CostUSD == nil || *got[0].CostUSD != 1.25 || got[1].CostUSD != nil {
			t.Fatalf("%+v", got)
		}
		got, _ = s.ListUsageSnapshots(ctx, "2025-03-02")
		if len(got) != 1 || got[0].Model != "m2" {
			t.Fatalf("since filter: %+v", got)
		}
	})
}
