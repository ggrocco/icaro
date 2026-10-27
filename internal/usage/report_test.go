package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/store"
)

func TestBuildReport(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 3, 3, 12, 0, 0, 0, time.UTC)
	roots := []Root{{Label: "host", Dir: "testdata/hostclaude"}}

	r, err := BuildReport(ctx, ReportOptions{Roots: roots, Timezone: "UTC", Since: "7d", LimitUSD: 1, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Timezone != "UTC" || !r.Since.Equal(time.Date(2025, 2, 24, 0, 0, 0, 0, time.UTC)) || r.LimitUSD != 1 {
		t.Fatalf("settings: %+v", r)
	}
	if find(r.Records, "host", "claude-3-5-haiku-20241022") == nil {
		t.Fatalf("records: %+v", r.Records)
	}
	if len(r.Alerts) != 1 {
		t.Fatalf("a 4.8 USD day over a 1 USD limit must alert: %+v", r.Alerts)
	}

	// An empty window still encodes as [] (not null), a negative limit is
	// reported as disabled, and an empty since uses DefaultSince.
	r, err = BuildReport(ctx, ReportOptions{Timezone: "UTC", LimitUSD: -3, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Records == nil || r.Alerts == nil || r.LimitUSD != 0 {
		t.Fatalf("empty report: %+v", r)
	}
	if want := time.Date(2025, 2, 24, 0, 0, 0, 0, time.UTC); !r.Since.Equal(want) {
		t.Fatalf("default since: %v", r.Since)
	}

	// The zero clock falls back to time.Now.
	r, err = BuildReport(ctx, ReportOptions{Timezone: "UTC", Since: "0d"})
	if err != nil {
		t.Fatal(err)
	}
	if y, m, d := time.Now().UTC().Date(); !r.Since.Equal(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("now: %v", r.Since)
	}

	var bad *BadOption
	for _, o := range []ReportOptions{
		{Timezone: "Nowhere/Nope"},
		{Timezone: "UTC", Since: "last week"},
	} {
		if _, err := BuildReport(ctx, o); !errors.As(err, &bad) || bad.Error() == "" {
			t.Fatalf("%+v: want BadOption, got %v", o, err)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cost := 0.5
	recs := []Record{
		{Provider: "claude-code", Source: "profile:claude", Day: "2025-03-01", Model: "m", InputTokens: 7, OutputTokens: 3, CacheReadTokens: 2, CacheWriteTokens: 1, Messages: 2, CostUSD: &cost, Confidence: ConfidenceHigh},
		{Provider: "claude-code", Source: "host", Confidence: ConfidenceUnavailable, Error: "source unavailable"},
		{Provider: "claude-code", Source: "dayless", Model: "m", Confidence: ConfidenceHigh},
	}
	if err := Save(ctx, st, recs); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st, "2025-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("unavailable and dayless rows must not be stored: %+v", got)
	}
	g := got[0]
	if g.Source != "profile:claude" || g.InputTokens != 7 || g.OutputTokens != 3 || g.CacheReadTokens != 2 || g.CacheWriteTokens != 1 ||
		g.Messages != 2 || g.CostUSD == nil || *g.CostUSD != 0.5 || g.Confidence != ConfidenceHigh {
		t.Fatalf("round trip: %+v", g)
	}

	_ = st.Close()
	if _, err := Load(ctx, st, "2025-01-01"); err == nil {
		t.Fatal("a closed store must fail")
	}
}

func TestCollectRootEdges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &ClaudeCode{}

	// A root that cannot be stat'ed for a reason other than absence is an
	// error even when optional.
	if _, err := c.collectRoot(ctx, Root{Dir: filepath.Join(file, "child")}, since(), time.UTC); err == nil {
		t.Fatal("ENOTDIR must be an error")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.collectRoot(cancelled, Root{Dir: "testdata/hostclaude"}, since(), time.UTC); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk: %v", err)
	}

	// An over-long line aborts the file with an error the root reports.
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 17<<20)
	big[0] = '{'
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	recs, _ := c.Collect(ctx, since(), time.UTC)
	if len(recs) != 0 {
		t.Fatalf("no roots: %+v", recs)
	}
	c.Roots = []Root{{Label: "big", Dir: dir}}
	recs, _ = c.Collect(ctx, since(), time.UTC)
	if len(recs) != 1 || recs[0].Confidence != ConfidenceUnavailable {
		t.Fatalf("token too long must make the source unavailable: %+v", recs)
	}

	// An unreadable transcript is skipped, not fatal.
	if err := scanFile(filepath.Join(dir, "absent.jsonl"), func(*ccLine) { t.Fatal("no lines") }); err != nil {
		t.Fatal(err)
	}
}

func TestInProjectsAndProfileRoots(t *testing.T) {
	for p, want := range map[string]bool{
		"/r/projects/a/s.jsonl":                true,
		"/r/home/.claude/projects/a/s.jsonl":   true,
		"/r/x/.config/claude/projects/s.jsonl": true,
		"/r/home/.claude/todos/s.jsonl":        false,
		"/r/notprojects/s.jsonl":               false,
	} {
		if got := inProjects("/r", p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
	if inProjects("/r", "relative/projects/s.jsonl") {
		t.Error("a path that cannot be made relative is outside")
	}
	if ProfileRoots(filepath.Join(t.TempDir(), "absent")) != nil {
		t.Fatal("a missing homes dir has no roots")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "stray"), nil, 0o600)
	_ = os.Mkdir(filepath.Join(dir, "codex"), 0o700)
	if got := ProfileRoots(dir); len(got) != 1 || got[0].Label != "profile:codex" {
		t.Fatalf("only directories are profiles: %+v", got)
	}
}

// TestBuildReportMergesSnapshots is the regression test for snapshots
// that were written but never read: a row whose transcript is gone comes
// back from the store, marked as a snapshot, and live rows win.
func TestBuildReportMergesSnapshots(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 3, 3, 12, 0, 0, 0, time.UTC)
	roots := []Root{{Label: "host", Dir: "testdata/hostclaude"}}
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := BuildReport(ctx, ReportOptions{Roots: roots, Timezone: "UTC", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	liveRow := find(live.Records, "host", "claude-3-5-haiku-20241022")
	if liveRow == nil {
		t.Fatalf("live: %+v", live.Records)
	}
	cost := 9.0
	stale := *liveRow
	stale.InputTokens = 1 // an older collection of a row that is still live
	if err := Save(ctx, st, []Record{
		stale,
		{Provider: "claude-code", Source: "host", Day: "2025-02-25", Model: "gone", InputTokens: 42, CostUSD: &cost, Confidence: ConfidenceHigh},
		{Provider: "claude-code", Source: "profile:other", Day: "2025-02-25", Model: "elsewhere", Confidence: ConfidenceHigh},
		{Provider: "claude-code", Source: "host", Day: "2025-01-01", Model: "too-old", Confidence: ConfidenceHigh},
	}); err != nil {
		t.Fatal(err)
	}

	r, err := BuildReport(ctx, ReportOptions{Roots: roots, Timezone: "UTC", Now: now, LimitUSD: 5, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if got := find(r.Records, "host", "claude-3-5-haiku-20241022"); got == nil || got.InputTokens != liveRow.InputTokens || got.Confidence != ConfidenceHigh {
		t.Fatalf("a live row must win over its snapshot: %+v", got)
	}
	gone := find(r.Records, "host", "gone")
	if gone == nil || gone.Confidence != ConfidenceSnapshot || gone.InputTokens != 42 {
		t.Fatalf("a row without a transcript must come from its snapshot: %+v", r.Records)
	}
	if find(r.Records, "profile:other", "elsewhere") != nil || find(r.Records, "host", "too-old") != nil {
		t.Fatalf("snapshots outside the roots or the window leaked: %+v", r.Records)
	}
	if r.Records[0].Day != "2025-02-25" {
		t.Fatalf("merged rows are sorted by day: %+v", r.Records)
	}
	// Snapshot rows count towards alerts.
	if len(r.Alerts) != 1 || r.Alerts[0].Day != "2025-02-25" {
		t.Fatalf("alerts: %+v", r.Alerts)
	}

	// Saving a merged report does not rewrite snapshot rows as live.
	if err := Save(ctx, st, r.Records); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st, "2025-02-25")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Confidence == ConfidenceSnapshot {
			t.Fatalf("a snapshot row was stored back: %+v", g)
		}
	}

	// A store that cannot be read is an unavailable row, not a failure.
	_ = st.Close()
	r, err = BuildReport(ctx, ReportOptions{Roots: roots, Timezone: "UTC", Now: now, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if find(r.Records, "*", "") == nil {
		t.Fatalf("closed store: %+v", r.Records)
	}
}
