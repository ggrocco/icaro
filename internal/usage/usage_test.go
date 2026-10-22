package usage

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func since() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

func find(recs []Record, source, model string) *Record {
	for i := range recs {
		if recs[i].Source == source && recs[i].Model == model {
			return &recs[i]
		}
	}
	return nil
}

func TestClaudeCodeTranscripts(t *testing.T) {
	sp, err := Location("America/Sao_Paulo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	p := &ClaudeCode{Roots: append(ProfileRoots("testdata/home"), Root{Label: "host", Dir: "testdata/hostclaude", Required: true})}
	recs := Collect(context.Background(), []Provider{p}, since(), sp)

	sonnet := find(recs, "profile:claude", "claude-sonnet-4-20250514")
	if sonnet == nil {
		t.Fatalf("no sonnet row: %+v", recs)
	}
	if sonnet.Messages != 1 || sonnet.InputTokens != 100 || sonnet.CacheWriteTokens != 1000 || sonnet.CacheReadTokens != 2000 {
		t.Fatalf("duplicate content blocks must count once and old lines are excluded: %+v", *sonnet)
	}
	if sonnet.Day != "2025-03-01" || sonnet.Confidence != ConfidenceHigh {
		t.Fatalf("day/confidence: %+v", *sonnet)
	}
	// 100*3 + 50*15 + 1000*3*1.25 + 2000*3*0.1 = 300+750+3750+600 = 5400 / 1e6
	if sonnet.CostUSD == nil || math.Abs(*sonnet.CostUSD-0.0054) > 1e-9 {
		t.Fatalf("cost: %v", sonnet.CostUSD)
	}
	opus := find(recs, "profile:claude", "claude-opus-4-5-20251101")
	// 01:00Z is 22:00 the previous day in São Paulo.
	if opus == nil || opus.Day != "2025-03-01" || *opus.CostUSD != 5 {
		t.Fatalf("opus: %+v", opus)
	}
	future := find(recs, "profile:claude", "claude-future-9")
	if future == nil || future.CostUSD != nil {
		t.Fatalf("unpriced model must have no cost: %+v", future)
	}
	if find(recs, "profile:claude", "<synthetic>") != nil {
		t.Fatal("synthetic rows must be skipped")
	}
	host := find(recs, "host", "claude-3-5-haiku-20241022")
	if host == nil || *host.CostUSD != 4.8 {
		t.Fatalf("host: %+v", host)
	}

	utc := Collect(context.Background(), []Provider{p}, since(), time.UTC)
	if o := find(utc, "profile:claude", "claude-opus-4-5-20251101"); o.Day != "2025-03-02" {
		t.Fatalf("UTC day: %s", o.Day)
	}
}

func TestUnavailableSources(t *testing.T) {
	p := &ClaudeCode{Roots: []Root{
		{Label: "optional", Dir: filepath.Join(t.TempDir(), "absent")},
		{Label: "host", Dir: filepath.Join(t.TempDir(), "absent"), Required: true},
	}}
	recs := Collect(context.Background(), []Provider{p, panicky{}, failing{}}, since(), time.UTC)
	if len(recs) != 3 {
		t.Fatalf("want 3 unavailable rows, got %+v", recs)
	}
	for _, r := range recs {
		if r.Confidence != ConfidenceUnavailable || r.Error == "" {
			t.Fatalf("%+v", r)
		}
	}
	if Alerts(recs, 1) != nil {
		t.Fatal("unavailable rows never alert")
	}
}

type panicky struct{}

func (panicky) Name() string { return "panicky" }
func (panicky) Collect(context.Context, time.Time, *time.Location) ([]Record, error) {
	panic("boom")
}

type failing struct{}

func (failing) Name() string { return "failing" }
func (failing) Collect(context.Context, time.Time, *time.Location) ([]Record, error) {
	return nil, errors.New("no access")
}

func TestAlerts(t *testing.T) {
	c := func(v float64) *float64 { return &v }
	recs := []Record{
		{Day: "2025-03-01", CostUSD: c(3)}, {Day: "2025-03-01", CostUSD: c(2.5)},
		{Day: "2025-03-02", CostUSD: c(1)}, {Day: "2025-03-02"},
		{Day: "2025-03-03", CostUSD: c(10)}, {Day: "2025-03-03"},
	}
	got := Alerts(recs, 5)
	if len(got) != 2 || got[0].Day != "2025-03-01" || got[0].SpentUSD != 5.5 || got[0].Partial || !got[1].Partial {
		t.Fatalf("%+v", got)
	}
	if Alerts(recs, 0) != nil {
		t.Fatal("limit 0 disables alerts")
	}
	if _, err := Location("Nowhere/Nope"); err == nil {
		t.Fatal("bad zone")
	}
	if l, _ := Location("Local"); l != time.Local {
		t.Fatal("local")
	}
	if _, ok := PriceFor("claude-opus-4-1-2025"); !ok {
		t.Fatal("prefix")
	}
	if p, _ := PriceFor("claude-opus-4-5-x"); p.Input != 5 {
		t.Fatal("longest prefix wins")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2025, 3, 10, 15, 4, 0, 0, time.UTC)
	got, err := ParseSince("7d", now)
	if err != nil || !got.Equal(time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", got, err)
	}
	got, err = ParseSince("2025-01-02", now)
	if err != nil || got.Day() != 2 {
		t.Fatalf("%v %v", got, err)
	}
	var bad *BadOption
	if _, err := ParseSince("yesterday", now); !errors.As(err, &bad) {
		t.Fatalf("bad input: %v", err)
	}
}
