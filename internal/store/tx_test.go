package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// migratedDB is a freshly migrated SQLite file, built once: migrating
// costs about a second under -race, copying a few hundred kilobytes does not.
var migratedDB = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "icaro-store-template")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "t.db")
	s, err := Open(context.Background(), SQLite, path)
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// sqliteStore returns a migrated SQLite store. The tests that use it rely
// on SQLite triggers and dynamic typing to inject failures, so they do not
// run against Postgres.
func sqliteStore(t *testing.T) *Store {
	t.Helper()
	img, err := migratedDB()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "t.db")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// failOn makes every op ("INSERT", "UPDATE", "DELETE") on table abort
// until the returned function drops the trigger.
func failOn(t *testing.T, s *Store, op, table string) func() {
	t.Helper()
	name := "fail_" + strings.ToLower(op) + "_" + table
	if _, err := s.db.Exec(`CREATE TRIGGER ` + name + ` BEFORE ` + op + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.db.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func isInjected(err error) bool {
	return err != nil && strings.Contains(err.Error(), "injected failure")
}

// TestPutMemoriesIsAtomic: a failure at any write of a bundle leaves no
// part of the batch behind, so ingest can simply retry.
func TestPutMemoriesIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	existing := &MemoryRow{ID: "keep", ProjectID: "p", Origin: "agent", Kind: "note", Title: "t", Source: "s"}
	if err := s.PutMemories(ctx, []MemoryBundle{{Memory: existing, Terms: []MemoryTerm{{"old", 1}}}}); err != nil {
		t.Fatal(err)
	}
	batch := func() []MemoryBundle {
		return []MemoryBundle{
			{Memory: &MemoryRow{ID: "new", ProjectID: "p", Origin: "agent", Kind: "note", Title: "n", Source: "s"}},
			{Memory: &MemoryRow{ID: "keep", ProjectID: "p", Origin: "agent", Kind: "note", Title: "t2", Source: "s"},
				Terms: []MemoryTerm{{"fresh", 1}}, Edges: []MemoryEdge{{ToRef: "file:x", Kind: "about"}}},
		}
	}
	for _, tc := range []struct{ op, table string }{
		{"DELETE", "memory_terms"},
		{"INSERT", "memories"},
		{"INSERT", "memory_terms"},
		{"INSERT", "memory_edges"},
	} {
		drop := failOn(t, s, tc.op, tc.table)
		if err := s.PutMemories(ctx, batch()); !isInjected(err) {
			t.Fatalf("%s %s: %v", tc.op, tc.table, err)
		}
		drop()
		if n := count(t, s, `SELECT COUNT(*) FROM memories WHERE id = 'new'`); n != 0 {
			t.Fatalf("%s %s: partial batch persisted", tc.op, tc.table)
		}
		if m, err := s.GetMemory(ctx, "p", "keep"); err != nil || m.Title != "t" {
			t.Fatalf("%s %s: replaced memory not restored: %+v %v", tc.op, tc.table, m, err)
		}
		if n := count(t, s, `SELECT COUNT(*) FROM memory_terms WHERE memory_id = 'keep' AND term = 'old'`); n != 1 {
			t.Fatalf("%s %s: postings lost", tc.op, tc.table)
		}
	}
}

func TestDeleteMemoriesIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	if err := s.PutMemories(ctx, []MemoryBundle{{Memory: &MemoryRow{ID: "m", ProjectID: "p", Origin: "agent", Kind: "note", Title: "t", Source: "s"}, Terms: []MemoryTerm{{"t", 1}}}}); err != nil {
		t.Fatal(err)
	}
	drop := failOn(t, s, "DELETE", "memories")
	if err := s.DeleteMemories(ctx, "p", []string{"m"}); !isInjected(err) {
		t.Fatal(err)
	}
	drop()
	if n := count(t, s, `SELECT COUNT(*) FROM memory_terms WHERE memory_id = 'm'`); n != 1 {
		t.Fatal("postings deleted although the memory was kept")
	}
}

func TestCreateRunIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	r := &Run{WorkflowID: "w", WorkflowName: "w", WorkflowVersion: 1, TriggerKind: "manual"}
	// A duplicate step id fails the second step insert.
	err := s.CreateRun(ctx, r, []StepInit{{StepID: "a", Idx: 0, Kind: "image"}, {StepID: "a", Idx: 1, Kind: "image"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate step: %v", err)
	}
	if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run persisted without its steps: %v", err)
	}
}

func TestRequestCancelFailures(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	queued := &Run{WorkflowID: "w", WorkflowName: "w", WorkflowVersion: 1, TriggerKind: "manual"}
	if err := s.CreateRun(ctx, queued, []StepInit{{StepID: "a", Kind: "image"}}); err != nil {
		t.Fatal(err)
	}
	running := &Run{WorkflowID: "w2", WorkflowName: "w2", WorkflowVersion: 1, TriggerKind: "manual", Status: RunRunning}
	if err := s.CreateRun(ctx, running, nil); err != nil {
		t.Fatal(err)
	}

	drop := failOn(t, s, "UPDATE", "runs")
	for _, id := range []string{queued.ID, running.ID} {
		if _, err := s.RequestCancel(ctx, id); !isInjected(err) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	drop()

	drop = failOn(t, s, "UPDATE", "step_runs")
	if _, err := s.RequestCancel(ctx, queued.ID); !isInjected(err) {
		t.Fatalf("step cancel: %v", err)
	}
	drop()
	if r, _ := s.GetRun(ctx, queued.ID); r.Status != RunQueued || r.CancelRequestedAt != nil {
		t.Fatalf("run cancelled although its steps were not: %+v", r)
	}
}

func TestApplyWorkflowFailures(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	drop := failOn(t, s, "INSERT", "workflows")
	if _, err := s.ApplyWorkflow(ctx, "w", "y", "{}", "me"); !isInjected(err) {
		t.Fatal(err)
	}
	drop()
	if _, err := s.ApplyWorkflow(ctx, "w", "y", "{}", "me"); err != nil {
		t.Fatal(err)
	}
	drop = failOn(t, s, "UPDATE", "workflows")
	if _, err := s.ApplyWorkflow(ctx, "w", "y2", "{}", "me"); !isInjected(err) {
		t.Fatal(err)
	}
	drop()
	drop = failOn(t, s, "INSERT", "workflow_versions")
	if _, err := s.ApplyWorkflow(ctx, "w", "y3", "{}", "me"); !isInjected(err) {
		t.Fatal(err)
	}
	drop()
	w, err := s.GetWorkflow(ctx, "w")
	if err != nil || w.Version != 1 || w.SpecYAML != "y" {
		t.Fatalf("failed applies must not bump the version: %+v %v", w, err)
	}
}

func TestPutUsageSnapshotsIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	drop := failOn(t, s, "INSERT", "usage_snapshots")
	if err := s.PutUsageSnapshots(ctx, []UsageSnapshot{{Provider: "p", Source: "s", Day: "2026-01-01", Model: "m"}}); !isInjected(err) {
		t.Fatal(err)
	}
	drop()
}

// TestCorruptRows: a row whose integer column holds text (a damaged
// database, or a write from a newer schema) must fail the read instead of
// returning a zero value.
func TestCorruptRows(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	w, err := s.ApplyWorkflow(ctx, "w", "y", "{}", "me")
	if err != nil {
		t.Fatal(err)
	}
	r := &Run{WorkflowID: w.ID, WorkflowName: "w", WorkflowVersion: 1, TriggerKind: "manual"}
	if err := s.CreateRun(ctx, r, []StepInit{{StepID: "a", Kind: "image"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLogChunk(ctx, &LogChunk{RunID: r.ID, StepID: "a", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCronState(ctx, &CronState{Workflow: "w", Expr: "@hourly", NextAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUsageSnapshots(ctx, []UsageSnapshot{{Provider: "p", Source: "s", Day: "2026-01-01", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMemories(ctx, []MemoryBundle{{Memory: &MemoryRow{ID: "m", ProjectID: "p", Origin: "ingest", Kind: "file", Title: "t", Source: "s"}, Terms: []MemoryTerm{{"t", 1}}}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE workflows SET version = 'x'`,
		`UPDATE workflow_versions SET version = 'x'`,
		`UPDATE runs SET depth = 'x'`,
		`UPDATE step_runs SET attempt = 'x'`,
		`UPDATE log_chunks SET seq = 'x'`,
		`UPDATE cron_state SET idx = 'x'`,
		`UPDATE usage_snapshots SET input_tokens = 'x'`,
		`UPDATE memories SET source_line = 'x'`,
		`UPDATE memory_terms SET tf = 'x'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	f := MemoryFilter{ProjectID: "p"}
	reads := map[string]func() error{
		"GetWorkflow": func() error { _, err := s.GetWorkflow(ctx, "w"); return err },
		"ApplyWorkflow": func() error {
			_, err := s.ApplyWorkflow(ctx, "w", "y", "{}", "me")
			return err
		},
		"ListWorkflowVersions": func() error { _, err := s.ListWorkflowVersions(ctx, w.ID); return err },
		"ListRuns":             func() error { _, err := s.ListRuns(ctx, RunFilter{}); return err },
		"GetStepRuns":          func() error { _, err := s.GetStepRuns(ctx, r.ID); return err },
		"ListLogChunks":        func() error { _, err := s.ListLogChunks(ctx, r.ID, "a", 1); return err },
		"ListCronStates":       func() error { _, err := s.ListCronStates(ctx); return err },
		"ListUsageSnapshots":   func() error { _, err := s.ListUsageSnapshots(ctx, ""); return err },
		"ListMemories":         func() error { _, err := s.ListMemories(ctx, f); return err },
		"TermPostings":         func() error { _, _, err := s.TermPostings(ctx, "p", "t", 5); return err },
		"SearchMemoryTerms":    func() error { _, err := s.SearchMemoryTerms(ctx, f, []string{"t"}); return err },
	}
	for name, read := range reads {
		if err := read(); err == nil {
			t.Errorf("%s returned a corrupt row without an error", name)
		}
	}
}
