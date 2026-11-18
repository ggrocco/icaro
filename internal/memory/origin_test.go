package memory

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPutByOriginRemove(t *testing.T) {
	engines(t, func(t *testing.T, e *Engine) {
		ctx := context.Background()
		now := time.Now().UTC()
		e.SetClock(func() time.Time { return now })
		items := []ImportItem{
			{Key: "lesson:a", Kind: KindFinding, Title: "Validator: unknown field", Body: "fix it", Metadata: map[string]any{"chat": "c1"}},
			{Key: "intent", Kind: KindFinding, Title: "Intent: post bugs", TTL: time.Hour},
		}
		rep, err := e.Put(ctx, scopeA, "distill:c1", items)
		if err != nil || rep.Imported != 2 {
			t.Fatal(rep, err)
		}
		// Idempotent: the same items change nothing; a changed one is replaced.
		if rep, _ := e.Put(ctx, scopeA, "distill:c1", items); rep.Imported != 0 || rep.Unchanged != 2 {
			t.Fatalf("%+v", rep)
		}
		items[0].Body = "fix it better"
		if rep, _ := e.Put(ctx, scopeA, "distill:c1", items); rep.Imported != 1 || rep.Unchanged != 1 {
			t.Fatalf("%+v", rep)
		}
		got, err := e.ByOrigin(ctx, scopeA, "distill:c1", 0)
		if err != nil || len(got) != 2 {
			t.Fatal(got, err)
		}
		var ttl *Memory
		for _, m := range got {
			if m.Title == "Intent: post bugs" {
				ttl = m
			}
		}
		if ttl == nil || ttl.ExpiresAt == nil || ttl.ExpiresAt.Sub(now.Add(time.Hour)).Abs() > time.Second || ttl.Origin != "distill:c1" {
			t.Fatalf("%+v", ttl)
		}
		// Another project's origin is separate.
		if got, _ := e.ByOrigin(ctx, scopeB, "distill:c1", 10); len(got) != 0 {
			t.Fatal("leaked across projects")
		}
		// After the TTL, the model memory is gone from listings.
		e.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
		if got, _ := e.ByOrigin(ctx, scopeA, "distill:c1", 10); len(got) != 1 {
			t.Fatalf("expired memory listed: %d", len(got))
		}
		if err := e.Remove(ctx, scopeB, got[0].ID); err != nil {
			t.Fatal(err)
		}
		if again, _ := e.ByOrigin(ctx, scopeA, "distill:c1", 10); len(again) != 1 {
			t.Fatal("another project removed a memory")
		}
		if err := e.Remove(ctx, scopeA, got[0].ID); err != nil {
			t.Fatal(err)
		}
		if again, _ := e.ByOrigin(ctx, scopeA, "distill:c1", 10); len(again) != 0 {
			t.Fatal("not removed")
		}
		for _, err := range []error{
			func() error { _, err := e.Put(ctx, Scope{}, "x", nil); return err }(),
			func() error { _, err := e.Put(ctx, scopeA, "", nil); return err }(),
			func() error { _, err := e.ByOrigin(ctx, Scope{}, "x", 1); return err }(),
			e.Remove(ctx, Scope{}, "x"),
			e.Remove(ctx, scopeA),
		} {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		}
	})
}
