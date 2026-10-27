// Package usage collects agent usage from local logs (Phase 9). It is a
// cost tracker, not a compute limiter: it reads what agent CLIs already
// write (codeburn-style), records where each number came from and how far
// to trust it, and never presents a cost as fact — costs are estimates
// from a pricing table and are absent for unpriced models.
package usage

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Confidence levels.
const (
	// ConfidenceHigh: token counts read from the provider's own usage fields.
	ConfidenceHigh = "high"
	// ConfidenceUnavailable marks a source that could not be read.
	ConfidenceUnavailable = "unavailable"
	// ConfidenceSnapshot marks a row read back from a stored snapshot
	// because its transcript is gone: the numbers are as of the last
	// collection, not re-read from the provider.
	ConfidenceSnapshot = "snapshot"
)

// Record is usage for one (provider, source, day, model).
type Record struct {
	Provider         string   `json:"provider"`
	Source           string   `json:"source"`
	Day              string   `json:"day"`
	Model            string   `json:"model"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	Messages         int64    `json:"messages"`
	CostUSD          *float64 `json:"cost_usd_estimate,omitempty"`
	Confidence       string   `json:"confidence"`
	// Error is set on "source unavailable" rows.
	Error string `json:"error,omitempty"`
}

// Provider reads one kind of local log.
type Provider interface {
	Name() string
	// Collect returns records for activity at or after since, bucketed by
	// day in loc. Per-source failures should be returned as Error rows.
	Collect(ctx context.Context, since time.Time, loc *time.Location) ([]Record, error)
}

// Collect runs every provider. A failing or panicking provider becomes a
// single unavailable row; it never fails the collection.
func Collect(ctx context.Context, providers []Provider, since time.Time, loc *time.Location) []Record {
	var out []Record
	for _, p := range providers {
		recs, err := safeCollect(ctx, p, since, loc)
		if err != nil {
			out = append(out, Record{Provider: p.Name(), Source: "*", Confidence: ConfidenceUnavailable, Error: err.Error()})
			continue
		}
		out = append(out, recs...)
	}
	sortRecords(out)
	return out
}

// sortRecords orders records by day, provider, source and model.
func sortRecords(out []Record) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Model < b.Model
	})
}

func safeCollect(ctx context.Context, p Provider, since time.Time, loc *time.Location) (recs []Record, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("provider panicked: %v", r)
		}
	}()
	return p.Collect(ctx, since, loc)
}

// Alert reports a day whose estimated cost reached the limit.
type Alert struct {
	Day      string  `json:"day"`
	SpentUSD float64 `json:"spent_usd_estimate"`
	LimitUSD float64 `json:"limit_usd"`
	// Partial is true when some usage that day had no price, so the real
	// spend is at least SpentUSD.
	Partial bool `json:"partial,omitempty"`
}

// Alerts returns the days whose estimated spend is at or above limit
// (limit <= 0 disables alerts). Days are already in the configured zone.
func Alerts(recs []Record, limit float64) []Alert {
	if limit <= 0 {
		return nil
	}
	type day struct {
		spent   float64
		partial bool
	}
	days := map[string]*day{}
	for _, r := range recs {
		if r.Error != "" || r.Day == "" {
			continue
		}
		d := days[r.Day]
		if d == nil {
			d = &day{}
			days[r.Day] = d
		}
		if r.CostUSD == nil {
			d.partial = true
			continue
		}
		d.spent += *r.CostUSD
	}
	var out []Alert
	for k, d := range days {
		if d.spent >= limit {
			out = append(out, Alert{Day: k, SpentUSD: d.spent, LimitUSD: limit, Partial: d.partial})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// Location resolves a configured timezone ("Local", "UTC", IANA name).
func Location(name string) (*time.Location, error) {
	switch name {
	case "", "Local", "local":
		return time.Local, nil
	}
	return time.LoadLocation(name)
}
