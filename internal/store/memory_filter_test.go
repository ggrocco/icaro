package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMemoryScopeAndFilters: every entry point refuses an unscoped call,
// empty inputs short-circuit, and Match mirrors the SQL filter.
func TestMemoryScopeAndFilters(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	for name, err := range map[string]error{
		"GetMemory":        func() error { _, err := s.GetMemory(ctx, "", "x"); return err }(),
		"GetMemoriesByIDs": func() error { _, err := s.GetMemoriesByIDs(ctx, "", []string{"x"}); return err }(),
		"FilterMemoryIDs":  func() error { _, err := s.FilterMemoryIDs(ctx, MemoryFilter{}, []string{"x"}); return err }(),
		"ListMemoryKeys":   func() error { _, err := s.ListMemoryKeys(ctx, "", "ingest"); return err }(),
		"DeleteMemories":   s.DeleteMemories(ctx, "", []string{"x"}),
		"AcceptHandoff":    func() error { _, err := s.AcceptHandoff(ctx, "", "x", "me", time.Now()); return err }(),
		"SearchMemoryTerms": func() error {
			_, err := s.SearchMemoryTerms(ctx, MemoryFilter{}, []string{"x"})
			return err
		}(),
		"MemoryCorpusStats": func() error { _, _, err := s.MemoryCorpusStats(ctx, MemoryFilter{}); return err }(),
	} {
		if !errors.Is(err, ErrMemoryScope) {
			t.Errorf("%s without a project: %v", name, err)
		}
	}
	if err := s.PutMemories(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMemories(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.SearchMemoryTerms(ctx, MemoryFilter{ProjectID: "p"}, nil); err != nil || hits != nil {
		t.Fatal(hits, err)
	}
	if got, err := s.FilterMemoryIDs(ctx, MemoryFilter{ProjectID: "p"}, nil); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if n, avg, err := s.MemoryCorpusStats(ctx, MemoryFilter{ProjectID: "empty"}); err != nil || n != 0 || avg != 0 {
		t.Fatalf("empty corpus: %d %v %v", n, avg, err)
	}

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, branch, agent string, at time.Time) *MemoryRow {
		return &MemoryRow{ID: id, ProjectID: "p", Branch: branch, Agent: agent, Origin: "agent", Kind: "note", Title: id, Source: "src/" + id, Status: "open", CreatedAt: at}
	}
	rows := []*MemoryRow{mk("a", "main", "claude", t0), mk("b", "feat", "codex", t0.Add(time.Hour)), mk("c", "", "claude", t0.Add(2*time.Hour))}
	var bundles []MemoryBundle
	for _, m := range rows {
		bundles = append(bundles, MemoryBundle{Memory: m})
	}
	if err := s.PutMemories(ctx, bundles); err != nil {
		t.Fatal(err)
	}
	ids := []string{"a", "b", "c"}
	since := t0.Add(30 * time.Minute)
	for _, tc := range []struct {
		name string
		f    MemoryFilter
		want string
	}{
		{"agent", MemoryFilter{Agent: "claude"}, "a,c"},
		{"branch keeps project-wide", MemoryFilter{Branch: "main"}, "a,c"},
		{"since", MemoryFilter{Since: &since}, "b,c"},
		{"source prefix", MemoryFilter{SourcePrefix: "src/b"}, "b"},
		{"status", MemoryFilter{Status: "done"}, ""},
		{"origin", MemoryFilter{Origin: "ingest"}, ""},
		{"kind", MemoryFilter{Kinds: []string{"file"}}, ""},
	} {
		tc.f.ProjectID = "p"
		list, err := s.ListMemories(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		filtered, err := s.FilterMemoryIDs(ctx, tc.f, ids)
		if err != nil {
			t.Fatal(err)
		}
		if got := memoryIDs(list); got != tc.want {
			t.Errorf("%s: ListMemories = %q, want %q", tc.name, got, tc.want)
		}
		if got := memoryIDs(filtered); got != tc.want {
			t.Errorf("%s: Match = %q, want %q (must mirror the SQL filter)", tc.name, got, tc.want)
		}
	}
	if (MemoryFilter{}).Match(rows[0]) {
		t.Fatal("an unscoped filter matches nothing")
	}

	for limit, want := range map[int]int{1: 1, 2000: 3} {
		list, err := s.ListMemories(ctx, MemoryFilter{ProjectID: "p", Limit: limit})
		if err != nil || len(list) != want {
			t.Fatalf("limit %d: %d rows, %v", limit, len(list), err)
		}
	}
	if got := limitOf(MemoryFilter{Limit: 2000}, 50, 1000); got != 1000 {
		t.Fatalf("limit is capped: %d", got)
	}
}

func memoryIDs(rows []*MemoryRow) string {
	ids := make([]string, len(rows))
	for i, m := range rows {
		ids[i] = m.ID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestLookupsAndConflicts(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, err := s.ApplyWorkflow(ctx, "w", "v1", "{}", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyWorkflow(ctx, "w", "v2", "{}", "bob"); err != nil {
			t.Fatal(err)
		}
		v, err := s.GetWorkflowVersion(ctx, w.ID, 1)
		if err != nil || v.SpecYAML != "v1" || v.Actor != "alice" || v.CreatedAt.IsZero() {
			t.Fatalf("pinned version: %+v %v", v, err)
		}
		if _, err := s.GetWorkflowVersion(ctx, w.ID, 3); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing version: %v", err)
		}

		if _, err := s.GetRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing run: %v", err)
		}
		if _, err := s.GetProject(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing project: %v", err)
		}
		if _, err := s.GetPinnedImage(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing image: %v", err)
		}
		a := &SessionRow{ID: "S1", Kind: "agent", Profile: "p", Status: "creating"}
		if err := s.CreateSession(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, &SessionRow{ID: "S1", Kind: "agent", Profile: "p", Status: "creating"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate session id: %v", err)
		}

		// Two daemons racing to create a webhook token: the loser reads the
		// winner's token instead of failing. The race is simulated by
		// inserting the competing row from inside the token generator.
		got, err := s.EnsureWebhookToken(ctx, "wf", func() string {
			if _, err := s.exec(ctx, `INSERT INTO webhook_tokens (workflow, token, created_at) VALUES (?,?,?)`, "wf", "winner", ts(Now())); err != nil {
				t.Fatal(err)
			}
			return "loser"
		})
		if err != nil || got.Token != "winner" {
			t.Fatalf("racing create: %+v %v", got, err)
		}
	})
}
