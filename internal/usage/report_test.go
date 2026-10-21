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
