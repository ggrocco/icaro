package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// forEachDriverInSchema is forEachDriver with a private Postgres schema,
// so it can run alongside other packages' tests on a shared server.
func forEachDriverInSchema(t *testing.T, schema string, fn func(t *testing.T, s *Store)) {
	t.Helper()
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteStore(t)) })
	dsn := os.Getenv("ICARO_TEST_POSTGRES_DSN")
	if dsn == "" {
		return
	}
	t.Run("postgres", func(t *testing.T) {
		admin, err := Open(ctx, Postgres, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
			t.Fatal(err)
		}
		_ = admin.Close()
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		s, err := Open(ctx, Postgres, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
}

func TestMemoryStore(t *testing.T) {
	forEachDriverInSchema(t, "test_store_memory", func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		past := now.Add(-time.Hour)
		a := &MemoryRow{ProjectID: "pa", Branch: "main", Agent: "claude", Origin: "agent", Kind: "decision", Title: "Use SQLite", Body: "portable sql", Source: "src/db_1.go", DocLen: 4}
		b := &MemoryRow{ProjectID: "pa", Origin: "ingest", Kind: "file", Title: "x.go", Source: "x.go", ContentHash: "h1", DocLen: 2}
		h := &MemoryRow{ProjectID: "pa", Branch: "feat", Origin: "agent", Kind: "handoff", Title: "next", Source: "agent:x", Status: "pending", DocLen: 1}
		exp := &MemoryRow{ProjectID: "pa", Origin: "agent", Kind: "finding", Title: "old", Source: "agent:x", ExpiresAt: &past, DocLen: 1}
		other := &MemoryRow{ProjectID: "pb", Origin: "agent", Kind: "decision", Title: "Use SQLite too", Source: "s", DocLen: 4}
		err := s.PutMemories(ctx, []MemoryBundle{
			{Memory: a, Terms: []MemoryTerm{{"sqlite", 2}, {"portable", 1}}, Edges: []MemoryEdge{{ToRef: "file:x.go", Kind: "about"}}},
			{Memory: b, Terms: []MemoryTerm{{"x", 2}}},
			{Memory: h, Terms: []MemoryTerm{{"next", 2}}},
			{Memory: exp, Terms: []MemoryTerm{{"sqlite", 3}}},
			{Memory: other, Terms: []MemoryTerm{{"sqlite", 2}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutMemories(ctx, []MemoryBundle{{Memory: &MemoryRow{Title: "x"}}}); !errors.Is(err, ErrMemoryScope) {
			t.Fatalf("missing project: %v", err)
		}

		hits, err := s.SearchMemoryTerms(ctx, MemoryFilter{ProjectID: "pa"}, []string{"sqlite"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].MemoryID != a.ID || hits[0].TF != 2 {
			t.Fatalf("hits = %+v (expired and other-project rows must not match)", hits)
		}
		posts, df, err := s.TermPostings(ctx, "pa", "sqlite", 1)
		if err != nil || df != 2 || len(posts) != 1 || posts[0].MemoryID != exp.ID {
			t.Fatalf("postings %+v df=%d %v (unfiltered index read, highest tf first)", posts, df, err)
		}
		if p, df, _ := s.TermPostings(ctx, "pa", "absent", 5); df != 0 || p != nil {
			t.Fatal("absent term")
		}
		if _, _, err := s.TermPostings(ctx, "", "x", 1); !errors.Is(err, ErrMemoryScope) {
			t.Fatal("unscoped postings")
		}
		fl, err := s.FilterMemoryIDs(ctx, MemoryFilter{ProjectID: "pa", Kinds: []string{"decision"}}, []string{a.ID, b.ID, exp.ID, other.ID})
		if err != nil || len(fl) != 1 || fl[0].ID != a.ID {
			t.Fatalf("filter ids %v %v", fl, err)
		}
		n, avg, err := s.MemoryCorpusStats(ctx, MemoryFilter{ProjectID: "pa"})
		if err != nil || n != 3 || avg < 2.3 || avg > 2.4 {
			t.Fatalf("stats n=%d avg=%v err=%v", n, avg, err)
		}

		if _, err := s.GetMemory(ctx, "pb", a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-project get must be not found")
		}
		got, err := s.GetMemoriesByIDs(ctx, "pb", []string{a.ID, other.ID})
		if err != nil || len(got) != 1 || got[0].ID != other.ID {
			t.Fatalf("by ids: %v %v", got, err)
		}

		list, err := s.ListMemories(ctx, MemoryFilter{ProjectID: "pa", Branch: "main"})
		if err != nil || len(list) != 2 {
			t.Fatalf("branch filter: %d %v", len(list), err)
		}
		list, _ = s.ListMemories(ctx, MemoryFilter{ProjectID: "pa", SourcePrefix: "src/db_"})
		if len(list) != 1 || list[0].ID != a.ID {
			t.Fatalf("source prefix: %+v", list)
		}
		list, _ = s.ListMemories(ctx, MemoryFilter{ProjectID: "pa", SourcePrefix: "src/db%"})
		if len(list) != 0 {
			t.Fatal("LIKE wildcards must be escaped")
		}
		since := now.Add(time.Hour)
		list, _ = s.ListMemories(ctx, MemoryFilter{ProjectID: "pa", Since: &since})
		if len(list) != 0 {
			t.Fatal("since")
		}
		list, _ = s.ListMemories(ctx, MemoryFilter{ProjectID: "pa", Kinds: []string{"handoff"}, Status: "pending", Agent: "", Origin: "agent"})
		if len(list) != 1 {
			t.Fatal("kind/status")
		}
		if _, err := s.ListMemories(ctx, MemoryFilter{}); !errors.Is(err, ErrMemoryScope) {
			t.Fatal("unscoped list must fail")
		}

		edges, err := s.ListMemoryEdges(ctx, "pa", a.ID)
		if err != nil || len(edges) != 1 || edges[0].ToRef != "file:x.go" {
			t.Fatalf("edges %v %v", edges, err)
		}

		acc, err := s.AcceptHandoff(ctx, "pa", h.ID, "codex", now)
		if err != nil || acc.Status != "accepted" || acc.AcceptedBy != "codex" || acc.AcceptedAt == nil {
			t.Fatalf("accept %+v %v", acc, err)
		}
		if _, err := s.AcceptHandoff(ctx, "pa", h.ID, "codex", now); !errors.Is(err, ErrConflict) {
			t.Fatalf("double accept: %v", err)
		}
		if _, err := s.AcceptHandoff(ctx, "pb", h.ID, "codex", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-project accept: %v", err)
		}

		// Replacing a memory replaces its postings.
		b.ContentHash = "h2"
		if err := s.PutMemories(ctx, []MemoryBundle{{Memory: b, Terms: []MemoryTerm{{"y", 2}}}}); err != nil {
			t.Fatal(err)
		}
		if hits, _ := s.SearchMemoryTerms(ctx, MemoryFilter{ProjectID: "pa"}, []string{"x"}); len(hits) != 0 {
			t.Fatal("stale postings after replace")
		}
		keys, err := s.ListMemoryKeys(ctx, "pa", "ingest")
		if err != nil || len(keys) != 1 || keys[0].ContentHash != "h2" {
			t.Fatalf("keys %v %v", keys, err)
		}

		expired, err := s.ExpiredMemoryIDs(ctx, now, 0)
		if err != nil || len(expired["pa"]) != 1 {
			t.Fatalf("expired %v %v", expired, err)
		}
		if err := s.DeleteMemories(ctx, "pa", expired["pa"]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetMemory(ctx, "pa", exp.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("delete")
		}
		// Deleting with the wrong project is a no-op.
		_ = s.DeleteMemories(ctx, "pb", []string{a.ID})
		if _, err := s.GetMemory(ctx, "pa", a.ID); err != nil {
			t.Fatal("cross-project delete removed a row")
		}
	})
}
