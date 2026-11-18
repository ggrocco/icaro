package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDistillJobs(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now()
		if _, err := s.ClaimDistill(ctx, now); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if ok, err := s.EnqueueDistill(ctx, "c1", now.Add(-time.Second)); !ok || err != nil {
			t.Fatal(ok, err)
		}
		// A queued chat keeps its place.
		if ok, _ := s.EnqueueDistill(ctx, "c1", now.Add(time.Hour)); ok {
			t.Fatal("queued twice")
		}
		if ok, _ := s.EnqueueDistill(ctx, "later", now.Add(time.Hour)); !ok {
			t.Fatal("second chat")
		}
		j, err := s.ClaimDistill(ctx, now)
		if err != nil || j.ChatID != "c1" || j.Status != DistillRunning || j.Attempts != 1 {
			t.Fatalf("%+v %v", j, err)
		}
		// "later" is not due yet.
		if _, err := s.ClaimDistill(ctx, now); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		// A retry: back to pending, due later.
		j.Status, j.Error, j.RunAt = DistillPending, "model down", now.Add(-time.Millisecond)
		if err := s.FinishDistill(ctx, j); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDistill(ctx, j); !errors.Is(err, ErrNotFound) {
			t.Fatal("finished a job that was not running")
		}
		j, _ = s.ClaimDistill(ctx, now)
		if j.Attempts != 2 || j.Error != "model down" {
			t.Fatalf("%+v", j)
		}
		j.Status, j.Error, j.Stage2, j.Memories = DistillDone, "", "done", 7
		if err := s.FinishDistill(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetDistill(ctx, "c1")
		if err != nil || got.Status != DistillDone || got.Memories != 7 || got.Stage2 != "done" {
			t.Fatalf("%+v %v", got, err)
		}
		if last, err := s.LastDistill(ctx); err != nil || last.ChatID != "c1" {
			t.Fatal(last, err)
		}
		// A finished chat is queued again with a fresh count.
		if ok, _ := s.EnqueueDistill(ctx, "c1", now.Add(-time.Second)); !ok {
			t.Fatal("re-queue")
		}
		j, _ = s.ClaimDistill(ctx, now)
		if j.ChatID != "c1" || j.Attempts != 1 {
			t.Fatalf("%+v", j)
		}
		// A restart puts running jobs back.
		if n, err := s.ResetDistills(ctx); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		if got, _ := s.GetDistill(ctx, "c1"); got.Status != DistillPending {
			t.Fatalf("%+v", got)
		}
		if _, err := s.GetDistill(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}

		// Stats.
		st := &ChatStats{ChatID: "c1", UsedMemories: 3, Turns: 4, Validations: 2, Proposals: 2, Saves: 1, TurnsToSave: 3, SecondsToSave: 90}
		if err := s.PutChatStats(ctx, st); err != nil {
			t.Fatal(err)
		}
		st.Turns = 5
		if err := s.PutChatStats(ctx, st); err != nil {
			t.Fatal(err)
		}
		all, err := s.ListChatStats(ctx)
		if err != nil || len(all) != 1 || all[0].Turns != 5 || all[0].SecondsToSave != 90 {
			t.Fatalf("%+v %v", all, err)
		}
		if err := s.MarkDistillRemoved(ctx, "c1", "m1"); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDistillRemoved(ctx, "c1", "m1"); err != nil {
			t.Fatal("twice", err)
		}
		if got, err := s.DistillRemoved(ctx, "c1"); err != nil || !got["m1"] || len(got) != 1 {
			t.Fatal(got, err)
		}
		if err := s.DeleteDistill(ctx, "c1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.DistillRemoved(ctx, "c1"); len(got) != 0 {
			t.Fatal("removed marks kept")
		}
		if all, _ := s.ListChatStats(ctx); len(all) != 0 {
			t.Fatal(all)
		}
		if _, err := s.GetDistill(ctx, "c1"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func TestDistillClosedStore(t *testing.T) {
	s := sqliteStore(t)
	_ = s.Close()
	ctx := context.Background()
	if _, err := s.EnqueueDistill(ctx, "c", time.Now()); err == nil {
		t.Error("enqueue")
	}
	if _, err := s.ClaimDistill(ctx, time.Now()); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("claim")
	}
	if _, err := s.ResetDistills(ctx); err == nil {
		t.Error("reset")
	}
	if err := s.DeleteDistill(ctx, "c"); err == nil {
		t.Error("delete")
	}
	if err := s.PutChatStats(ctx, &ChatStats{ChatID: "c"}); err == nil {
		t.Error("stats")
	}
	if _, err := s.ListChatStats(ctx); err == nil {
		t.Error("list stats")
	}
	if _, err := s.DistillRemoved(ctx, "c"); err == nil {
		t.Error("removed")
	}
}

// Errors inside the transactions: each statement's table is gone.
func TestDistillMissingTables(t *testing.T) {
	s := sqliteStore(t)
	ctx := context.Background()
	for _, tbl := range []string{"distill_jobs", "chat_stats", "distill_removed"} {
		if _, err := s.exec(ctx, "DROP TABLE "+tbl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnqueueDistill(ctx, "c", time.Now()); err == nil {
		t.Error("enqueue")
	}
	if err := s.DeleteDistill(ctx, "c"); err == nil {
		t.Error("delete")
	}
	if err := s.PutChatStats(ctx, &ChatStats{ChatID: "c"}); err == nil {
		t.Error("stats")
	}
}
