package usage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultSince is the window a report covers when none is given.
const DefaultSince = "7d"

// ReportOptions shape one usage report. The CLI and the daemon's
// /api/v1/usage build reports the same way.
type ReportOptions struct {
	Roots []Root
	// Timezone is usage.timezone ("Local", "UTC", an IANA name).
	Timezone string
	// Since is "Nd" or YYYY-MM-DD in Timezone; empty means DefaultSince.
	Since string
	// LimitUSD is usage.daily_limit_usd; <= 0 disables alerts.
	LimitUSD float64
	// Now is the clock; zero means time.Now.
	Now time.Time
}

// Report is the usage for one window: the records, the alerts they
// raise, and the settings that produced them.
type Report struct {
	Timezone string    `json:"timezone"`
	Since    time.Time `json:"since"`
	LimitUSD float64   `json:"daily_limit_usd,omitempty"`
	Records  []Record  `json:"records"`
	Alerts   []Alert   `json:"alerts"`
}

// BadOption is an invalid since or timezone; callers map it to a usage
// error (CLI) or a 400 (API).
type BadOption struct{ Msg string }

func (e *BadOption) Error() string { return e.Msg }

// BuildReport collects the Claude Code transcripts under opts.Roots.
// Provider failures become unavailable rows, as in Collect; only a bad
// option is an error.
func BuildReport(ctx context.Context, opts ReportOptions) (*Report, error) {
	loc, err := Location(opts.Timezone)
	if err != nil {
		return nil, &BadOption{Msg: fmt.Sprintf("usage.timezone: %v", err)}
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	s := opts.Since
	if s == "" {
		s = DefaultSince
	}
	since, err := ParseSince(s, now.In(loc))
	if err != nil {
		return nil, err
	}
	recs := Collect(ctx, []Provider{&ClaudeCode{Roots: opts.Roots}}, since, loc)
	if recs == nil {
		recs = []Record{}
	}
	alerts := Alerts(recs, opts.LimitUSD)
	if alerts == nil {
		alerts = []Alert{}
	}
	limit := opts.LimitUSD
	if limit < 0 {
		limit = 0
	}
	return &Report{Timezone: loc.String(), Since: since, LimitUSD: limit, Records: recs, Alerts: alerts}, nil
}

// ParseSince accepts "Nd" (midnight N days before now) or a YYYY-MM-DD
// date, both in now's location.
func ParseSince(s string, now time.Time) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && n >= 0 {
			y, m, d := now.Date()
			return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -n), nil
		}
	}
	t, err := time.ParseInLocation("2006-01-02", s, now.Location())
	if err != nil {
		return time.Time{}, &BadOption{Msg: fmt.Sprintf("since %q: want Nd or YYYY-MM-DD", s)}
	}
	return t, nil
}
