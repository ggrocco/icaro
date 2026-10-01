package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// forEachDriver runs fn against SQLite and, when ICARO_TEST_POSTGRES_DSN is
// set, against Postgres with a freshly reset schema.
func forEachDriver(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	ctx := context.Background()

	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(ctx, SQLite, filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})

	dsn := os.Getenv("ICARO_TEST_POSTGRES_DSN")
	if dsn == "" {
		return
	}
	t.Run("postgres", func(t *testing.T) {
		s, err := Open(ctx, Postgres, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if _, err := s.db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		fn(t, s)
	})
}

func TestRebind(t *testing.T) {
	s := &Store{dialect: Postgres}
	if got := s.rebind("a = ? AND b = ?"); got != "a = $1 AND b = $2" {
		t.Fatal(got)
	}
	s.dialect = SQLite
	if got := s.rebind("a = ?"); got != "a = ?" {
		t.Fatal(got)
	}
}

func TestTimestampsSortAsText(t *testing.T) {
	a := ts(time.Date(2026, 1, 1, 12, 0, 0, 500_000_000, time.UTC))
	b := ts(time.Date(2026, 1, 1, 12, 0, 0, 510_000_000, time.UTC))
	if a >= b {
		t.Fatalf("%s should sort before %s", a, b)
	}
	if parseTS(a).UnixNano() != time.Date(2026, 1, 1, 12, 0, 0, 500_000_000, time.UTC).UnixNano() {
		t.Fatal("round trip")
	}
}

func TestWorkflows(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, err := s.ApplyWorkflow(ctx, "hello", "name: hello\n", `{"name":"hello"}`, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if w.Version != 1 || w.Status != WorkflowActive || w.ID == "" {
			t.Fatalf("bad insert: %+v", w)
		}
		w2, err := s.ApplyWorkflow(ctx, "hello", "name: hello # v2\n", `{"name":"hello","v":2}`, "bob")
		if err != nil {
			t.Fatal(err)
		}
		if w2.ID != w.ID || w2.Version != 2 {
			t.Fatalf("bad bump: %+v", w2)
		}
		got, err := s.GetWorkflow(ctx, "hello")
		if err != nil || got.Version != 2 || got.SpecYAML != "name: hello # v2\n" {
			t.Fatalf("get: %v %+v", err, got)
		}
		vs, err := s.ListWorkflowVersions(ctx, w.ID)
		if err != nil || len(vs) != 2 || vs[0].Version != 2 || vs[1].Actor != "alice" {
			t.Fatalf("versions: %v %+v", err, vs)
		}
		if _, err := s.ApplyWorkflow(ctx, "other", "x", "{}", "a"); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListWorkflows(ctx)
		if err != nil || len(list) != 2 || list[0].Name != "hello" {
			t.Fatalf("list: %v %+v", err, list)
		}
		if err := s.SetWorkflowStatus(ctx, w.ID, WorkflowDisabled); err != nil {
			t.Fatal(err)
		}
		if err := s.SetWebhook(ctx, w.ID, "tok", []byte{1, 2}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetWorkflowByID(ctx, w.ID)
		if got.Status != WorkflowDisabled || got.WebhookToken != "tok" || len(got.WebhookSecretCT) != 2 {
			t.Fatalf("update: %+v", got)
		}
		if err := s.DeleteWorkflow(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetWorkflow(ctx, "hello"); err != ErrNotFound {
			t.Fatalf("expected not found, got %v", err)
		}
		if vs, _ := s.ListWorkflowVersions(ctx, w.ID); len(vs) != 0 {
			t.Fatal("versions should cascade")
		}
		if err := s.DeleteWorkflow(ctx, w.ID); err != ErrNotFound {
			t.Fatal(err)
		}
	})
}

func TestRunsQueueAndSteps(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		w, _ := s.ApplyWorkflow(ctx, "wf", "x", "{}", "a")
		mk := func() *Run {
			r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: TriggerAPI, InputJSON: `{"a":1}`}
			if err := s.CreateRun(ctx, r, []string{"one", "two"}); err != nil {
				t.Fatal(err)
			}
			return r
		}
		r1, r2 := mk(), mk()
		if r1.RootRunID != r1.ID || r1.Status != RunQueued {
			t.Fatalf("defaults: %+v", r1)
		}

		c1, err := s.ClaimRun(ctx, "runner-a")
		if err != nil || c1.ID != r1.ID || c1.Status != RunRunning || c1.ClaimedBy != "runner-a" || c1.StartedAt == nil {
			t.Fatalf("claim1: %v %+v", err, c1)
		}
		c2, err := s.ClaimRun(ctx, "runner-a")
		if err != nil || c2.ID != r2.ID {
			t.Fatalf("claim2: %v %+v", err, c2)
		}
		if _, err := s.ClaimRun(ctx, "runner-a"); err != ErrNotFound {
			t.Fatalf("empty queue: %v", err)
		}

		steps, err := s.GetRunSteps(ctx, r1.ID)
		if err != nil || len(steps) != 2 || steps[1].Name != "two" || steps[0].Status != StepPending {
			t.Fatalf("steps: %v %+v", err, steps)
		}
		now := Now()
		code := 0
		st := steps[0]
		st.Status, st.Attempt, st.ContainerID, st.ExitCode = StepSucceeded, 1, "c123", &code
		st.OutputJSON, st.LogPath, st.LogSize, st.StartedAt, st.FinishedAt = `{"k":"v"}`, "/l/0.log", 42, &now, &now
		if err := s.UpdateRunStep(ctx, st); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRunStep(ctx, r1.ID, 0)
		if err != nil || got.Status != StepSucceeded || *got.ExitCode != 0 || got.OutputJSON != `{"k":"v"}` || got.LogSize != 42 || got.StartedAt == nil {
			t.Fatalf("step update: %v %+v", err, got)
		}

		if err := s.Heartbeat(ctx, r1.ID); err != nil {
			t.Fatal(err)
		}
		mine, err := s.ListRunsClaimedBy(ctx, "runner-a")
		if err != nil || len(mine) != 2 {
			t.Fatalf("claimed by: %v %d", err, len(mine))
		}

		// Cancel: running run gets flagged, queued run is cancelled outright.
		r3 := mk()
		cr, err := s.RequestCancel(ctx, r3.ID)
		if err != nil || cr.Status != RunCancelled || cr.FinishedAt == nil {
			t.Fatalf("cancel queued: %v %+v", err, cr)
		}
		cr, err = s.RequestCancel(ctx, r1.ID)
		if err != nil || cr.Status != RunRunning || cr.CancelRequestedAt == nil {
			t.Fatalf("cancel running: %v %+v", err, cr)
		}
		if _, err := s.RequestCancel(ctx, "nope"); err != ErrNotFound {
			t.Fatalf("cancel missing: %v", err)
		}

		if err := s.FinishRun(ctx, r1.ID, RunFailed, "boom"); err != nil {
			t.Fatal(err)
		}
		fr, _ := s.GetRun(ctx, r1.ID)
		if fr.Status != RunFailed || fr.Error != "boom" || fr.FinishedAt == nil {
			t.Fatalf("finish: %+v", fr)
		}

		list, err := s.ListRuns(ctx, RunFilter{WorkflowID: w.ID})
		if err != nil || len(list) != 3 || list[0].ID != r3.ID {
			t.Fatalf("list: %v %d", err, len(list))
		}
		list, _ = s.ListRuns(ctx, RunFilter{Status: RunRunning})
		if len(list) != 1 || list[0].ID != r2.ID {
			t.Fatalf("filter status: %+v", list)
		}
		counts, _ := s.CountRunsByStatus(ctx)
		if counts[RunRunning] != 1 || counts[RunFailed] != 1 || counts[RunCancelled] != 1 {
			t.Fatalf("counts: %v", counts)
		}

		// Stale release: runner-b's run with an old heartbeat goes back to queued.
		r4 := mk()
		if _, err := s.ClaimRun(ctx, "runner-b"); err != nil {
			t.Fatal(err)
		}
		old := ts(Now().Add(-time.Hour))
		if _, err := s.exec(ctx, `UPDATE runs SET heartbeat_at = ? WHERE id = ?`, old, r4.ID); err != nil {
			t.Fatal(err)
		}
		n, err := s.ReleaseStaleRuns(ctx, "runner-a", 10*time.Minute)
		if err != nil || n != 1 {
			t.Fatalf("release: %v %d", err, n)
		}
		rr, _ := s.GetRun(ctx, r4.ID)
		if rr.Status != RunQueued || rr.ClaimedBy != "" {
			t.Fatalf("released: %+v", rr)
		}
	})
}

func TestTokensConnectionsMeta(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		tok, err := s.CreateToken(ctx, "dev", "hash1", ScopeAdmin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateToken(ctx, "dev", "hash2", ScopeRead); err != ErrConflict {
			t.Fatalf("dup name: %v", err)
		}
		got, err := s.GetTokenByHash(ctx, "hash1")
		if err != nil || got.ID != tok.ID || got.Scope != ScopeAdmin {
			t.Fatalf("by hash: %v %+v", err, got)
		}
		if err := s.TouchToken(ctx, tok.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RevokeToken(ctx, "dev"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetTokenByHash(ctx, "hash1"); err != ErrNotFound {
			t.Fatalf("revoked lookup: %v", err)
		}
		if l, _ := s.ListTokens(ctx); len(l) != 1 || l[0].RevokedAt == nil || l[0].LastUsedAt == nil {
			t.Fatalf("list tokens: %+v", l)
		}

		c := &Connection{Name: "slack", Type: "generic", FieldsCT: []byte("ct"), FieldNames: []string{"token"}}
		if err := s.UpsertConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
		c2 := &Connection{Name: "slack", Type: "bearer", FieldsCT: []byte("ct2"), FieldNames: []string{"token", "x"}}
		if err := s.UpsertConnection(ctx, c2); err != nil {
			t.Fatal(err)
		}
		gc, err := s.GetConnection(ctx, "slack")
		if err != nil || gc.ID != c.ID || gc.Type != "bearer" || string(gc.FieldsCT) != "ct2" || len(gc.FieldNames) != 2 {
			t.Fatalf("conn upsert: %v %+v", err, gc)
		}
		if l, _ := s.ListConnections(ctx); len(l) != 1 {
			t.Fatal("list conns")
		}
		if err := s.DeleteConnection(ctx, "slack"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteConnection(ctx, "slack"); err != ErrNotFound {
			t.Fatal(err)
		}

		if _, err := s.GetMeta(ctx, "runner_id"); err != ErrNotFound {
			t.Fatal(err)
		}
		if err := s.SetMeta(ctx, "runner_id", "r1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMeta(ctx, "runner_id", "r2"); err != nil {
			t.Fatal(err)
		}
		if v, _ := s.GetMeta(ctx, "runner_id"); v != "r2" {
			t.Fatal(v)
		}
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
