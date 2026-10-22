package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// forEachDriver runs fn against SQLite and, when ICARO_TEST_POSTGRES_DSN is
// set, against Postgres with a freshly reset schema.
func forEachDriver(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	ctx := context.Background()

	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteStore(t)) })

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
		steps := []StepInit{{StepID: "one", Idx: 0, Kind: "image"}, {StepID: "two", Idx: 1, Kind: "uses"}}
		mk := func(key, policy string) *Run {
			r := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual", TriggerJSON: `{"a":1}`,
				IdempotencyKey: key, Concurrency: policy, Actor: "alice", DeliveryID: "d1"}
			if err := s.CreateRun(ctx, r, steps); err != nil {
				t.Fatal(err)
			}
			return r
		}
		r1, r2 := mk("k1", "allow"), mk("", "allow")
		if r1.RootRunID != r1.ID || r1.Status != RunQueued {
			t.Fatalf("defaults: %+v", r1)
		}
		dup := &Run{WorkflowID: w.ID, WorkflowName: w.Name, WorkflowVersion: 1, TriggerKind: "manual", IdempotencyKey: "k1"}
		if err := s.CreateRun(ctx, dup, steps); err != ErrConflict {
			t.Fatalf("duplicate idempotency key: %v", err)
		}
		byKey, err := s.GetRunByIdempotencyKey(ctx, "k1")
		if err != nil || byKey.ID != r1.ID || byKey.Actor != "alice" || byKey.DeliveryID != "d1" || byKey.TriggerJSON != `{"a":1}` {
			t.Fatalf("by key: %v %+v", err, byKey)
		}
		if _, err := s.GetStepRuns(ctx, dup.ID); err != nil {
			t.Fatal(err)
		}

		c1, err := s.ClaimRun(ctx, "daemon")
		if err != nil || c1.ID != r1.ID || c1.Status != RunRunning || c1.ClaimedBy != "daemon" || c1.StartedAt == nil {
			t.Fatalf("claim1: %v %+v", err, c1)
		}
		// A queue-policy run waits while another run of the workflow runs.
		r3 := mk("", "queue")
		c2, err := s.ClaimRun(ctx, "daemon")
		if err != nil || c2.ID != r2.ID {
			t.Fatalf("claim2: %v %+v", err, c2)
		}
		if _, err := s.ClaimRun(ctx, "daemon"); err != ErrNotFound {
			t.Fatalf("queue policy must wait: %v", err)
		}
		_ = s.FinishRun(ctx, r1.ID, RunSucceeded, "")
		_ = s.FinishRun(ctx, r2.ID, RunSucceeded, "")
		c3, err := s.ClaimRun(ctx, "daemon")
		if err != nil || c3.ID != r3.ID {
			t.Fatalf("claim queued after finish: %v %+v", err, c3)
		}
		if err := s.SetRunSession(ctx, r3.ID, "sess1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetRun(ctx, r3.ID); got.SessionID != "sess1" || got.Concurrency != "queue" {
			t.Fatalf("session: %+v", got)
		}

		srs, err := s.GetStepRuns(ctx, r1.ID)
		if err != nil || len(srs) != 2 || srs[1].StepID != "two" || srs[1].Kind != "uses" || srs[0].Status != StepPending {
			t.Fatalf("steps: %v %+v", err, srs)
		}
		now := Now()
		code := 0
		st := srs[0]
		st.Status, st.Attempt, st.ContainerID, st.ExitCode, st.SessionID = StepSucceeded, 1, "c123", &code, "sess"
		st.OutputsJSON, st.LogPath, st.LogSize, st.LogLines, st.StartedAt, st.FinishedAt = `{"k":"v"}`, "/l/one.1.log", 42, 3, &now, &now
		if err := s.UpdateStepRun(ctx, st); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetStepRun(ctx, r1.ID, "one")
		if err != nil || got.Status != StepSucceeded || *got.ExitCode != 0 || got.OutputsJSON != `{"k":"v"}` || got.LogSize != 42 || got.LogLines != 3 || got.StartedAt == nil {
			t.Fatalf("step update: %v %+v", err, got)
		}
		if _, err := s.GetStepRun(ctx, r1.ID, "nope"); err != ErrNotFound {
			t.Fatal(err)
		}
		if err := s.UpdateStepRun(ctx, &StepRun{RunID: r1.ID, StepID: "nope"}); err != ErrNotFound {
			t.Fatal(err)
		}

		// Log index.
		if _, err := s.LastLogChunk(ctx, r1.ID, "one", 1); err != ErrNotFound {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			if err := s.AddLogChunk(ctx, &LogChunk{RunID: r1.ID, StepID: "one", Attempt: 1, Seq: i, FileOffset: int64(i * 10), LineCount: int64(i)}); err != nil {
				t.Fatal(err)
			}
		}
		last, err := s.LastLogChunk(ctx, r1.ID, "one", 1)
		if err != nil || last.Seq != 3 || last.FileOffset != 30 {
			t.Fatalf("last chunk: %v %+v", err, last)
		}
		if cs, _ := s.ListLogChunks(ctx, r1.ID, "one", 1); len(cs) != 3 || cs[0].LineCount != 1 {
			t.Fatalf("chunks %+v", cs)
		}

		if err := s.Heartbeat(ctx, r3.ID); err != nil {
			t.Fatal(err)
		}
		running, err := s.ListRunsByStatus(ctx, RunRunning)
		if err != nil || len(running) != 1 || running[0].ID != r3.ID {
			t.Fatalf("by status: %v %+v", err, running)
		}

		// Cancel: running run gets flagged, queued run is cancelled outright.
		r4 := mk("", "allow")
		if act, _ := s.ActiveRuns(ctx, w.ID); len(act) != 2 {
			t.Fatalf("active: %d", len(act))
		}
		cr, err := s.RequestCancel(ctx, r4.ID)
		if err != nil || cr.Status != RunCancelled || cr.FinishedAt == nil {
			t.Fatalf("cancel queued: %v %+v", err, cr)
		}
		if st4, _ := s.GetStepRuns(ctx, r4.ID); st4[0].Status != StepCancelled {
			t.Fatalf("queued run steps cancelled: %+v", st4[0])
		}
		cr, err = s.RequestCancel(ctx, r3.ID)
		if err != nil || cr.Status != RunRunning || cr.CancelRequestedAt == nil {
			t.Fatalf("cancel running: %v %+v", err, cr)
		}
		if _, err := s.RequestCancel(ctx, "nope"); err != ErrNotFound {
			t.Fatalf("cancel missing: %v", err)
		}
		if err := s.FinishRun(ctx, r3.ID, RunFailed, "boom"); err != nil {
			t.Fatal(err)
		}
		fr, _ := s.GetRun(ctx, r3.ID)
		if fr.Status != RunFailed || fr.Error != "boom" || fr.FinishedAt == nil {
			t.Fatalf("finish: %+v", fr)
		}
		if err := s.FinishRun(ctx, "nope", RunFailed, ""); err != ErrNotFound {
			t.Fatal(err)
		}

		list, err := s.ListRuns(ctx, RunFilter{WorkflowID: w.ID})
		if err != nil || len(list) != 4 || list[0].ID != r4.ID {
			t.Fatalf("list: %v %d", err, len(list))
		}
		list, _ = s.ListRuns(ctx, RunFilter{Status: RunFailed, Limit: 5})
		if len(list) != 1 || list[0].ID != r3.ID {
			t.Fatalf("filter status: %+v", list)
		}
		counts, _ := s.CountRunsByStatus(ctx)
		if counts[RunSucceeded] != 2 || counts[RunFailed] != 1 || counts[RunCancelled] != 1 {
			t.Fatalf("counts: %v", counts)
		}
		if act, _ := s.ActiveWorkflows(ctx); len(act) != 1 {
			t.Fatal("active workflows")
		}
	})
}

func TestTokensMeta(t *testing.T) {
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
