package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDeliveries(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		d := &Delivery{Integration: "github", DeliveryKey: "abc", Method: "POST", HeadersJSON: `{"x":"y"}`, Body: []byte(`{"a":1}`), Verified: true, Status: DeliveryProcessed}
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
		rej := &Delivery{Integration: "github", DeliveryKey: "abc", Method: "POST", HeadersJSON: "{}", Verified: false, VerifyError: "bad sig", Status: DeliveryRejected}
		if err := s.CreateDelivery(ctx, rej); err != nil {
			t.Fatal(err)
		}
		got, err := s.FindProcessedDelivery(ctx, "github", "abc", "", "", "")
		if err != nil || got.ID != d.ID || string(got.Body) != `{"a":1}` || !got.Verified {
			t.Fatalf("find: %+v %v", got, err)
		}
		if _, err := s.FindProcessedDelivery(ctx, "github", "abc", "", "", d.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("except self: %v", err)
		}
		if _, err := s.FindProcessedDelivery(ctx, "github", "abc", "", "wf", ""); !errors.Is(err, ErrNotFound) {
			t.Fatal("workflow scoping")
		}
		if err := s.FinishDelivery(ctx, rej.ID, DeliveryRejected, "x", `["e"]`, `[{"r":1}]`); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDelivery(ctx, "nope", "", "", "", ""); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		g, _ := s.GetDelivery(ctx, rej.ID)
		if g.Error != "x" || g.ResultsJSON != `[{"r":1}]` || g.Verified {
			t.Fatalf("%+v", g)
		}
		re := &Delivery{Integration: "github", DeliveryKey: "abc", Method: "POST", HeadersJSON: "{}", Verified: true, Status: DeliveryProcessed, RedeliveryOf: d.ID}
		if err := s.CreateDelivery(ctx, re); err != nil {
			t.Fatal(err)
		}
		all, _ := s.ListDeliveries(ctx, DeliveryFilter{Integration: "github"})
		if len(all) != 3 || all[0].ID != re.ID || all[0].RedeliveryOf != d.ID {
			t.Fatalf("list: %d", len(all))
		}
		only, _ := s.ListDeliveries(ctx, DeliveryFilter{Status: DeliveryRejected, Limit: 5})
		if len(only) != 1 {
			t.Fatal(len(only))
		}
	})
}

// A replay with a fresh delivery id but the same signed material is found
// by its signed hash; an empty hash never matches.
func TestFindProcessedDeliveryBySignedHash(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		d := &Delivery{Integration: "github", DeliveryKey: "id-1", SignedSHA256: "abc", Method: "POST", HeadersJSON: "{}", Body: []byte("x"), Verified: true, Status: DeliveryProcessed}
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
		if d.BodySize != 1 || d.BodySHA256 == "" {
			t.Fatalf("body identity filled in: %+v", d)
		}
		got, err := s.FindProcessedDelivery(ctx, "github", "id-2", "abc", "", "")
		if err != nil || got.ID != d.ID || got.SignedSHA256 != "abc" || got.BodySize != 1 || got.BodySHA256 != d.BodySHA256 {
			t.Fatalf("%+v %v", got, err)
		}
		if _, err := s.FindProcessedDelivery(ctx, "github", "id-2", "", "", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an empty signed hash must not match: %v", err)
		}
	})
}

func TestPruneDeliveries(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		add := func(age time.Duration, verified bool) *Delivery {
			d := &Delivery{Integration: "github", DeliveryKey: NewID(), Method: "POST", HeadersJSON: "{}", Verified: verified, Status: DeliveryRejected, ReceivedAt: now.Add(-age)}
			if verified {
				d.Status = DeliveryProcessed
			}
			if err := s.CreateDelivery(ctx, d); err != nil {
				t.Fatal(err)
			}
			return d
		}
		old := add(40*24*time.Hour, true)
		keep := add(time.Hour, true)
		var rejected []*Delivery
		for i := range 5 {
			rejected = append(rejected, add(time.Duration(i+1)*time.Minute, false))
		}
		n, err := s.PruneDeliveries(ctx, now.Add(-30*24*time.Hour), 2)
		if err != nil || n != 4 {
			t.Fatalf("pruned %d, %v", n, err)
		}
		if _, err := s.GetDelivery(ctx, old.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("an aged-out delivery must go")
		}
		for _, id := range []string{keep.ID, rejected[0].ID, rejected[1].ID} {
			if _, err := s.GetDelivery(ctx, id); err != nil {
				t.Fatalf("%s must stay: %v", id, err)
			}
		}
		if _, err := s.GetDelivery(ctx, rejected[2].ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("rejected deliveries beyond the cap must go")
		}
		// No cap, and a cap above the count, keep the rest.
		if n, err := s.PruneDeliveries(ctx, now.Add(-30*24*time.Hour), 0); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
		if n, err := s.PruneDeliveries(ctx, now.Add(-30*24*time.Hour), 100); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
	})
}

func TestWebhookTokensAndCronState(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		n := 0
		gen := func() string { n++; return fmt.Sprintf("tok%d", n) }
		a, err := s.EnsureWebhookToken(ctx, "wf-a", gen)
		if err != nil || a.Token != "tok1" {
			t.Fatal(a, err)
		}
		again, _ := s.EnsureWebhookToken(ctx, "wf-a", gen)
		if again.Token != "tok1" || n != 1 {
			t.Fatal("token must be stable")
		}
		if got, err := s.WebhookTokenByToken(ctx, "tok1"); err != nil || got.Workflow != "wf-a" {
			t.Fatal(got, err)
		}
		if err := s.RotateWebhookToken(ctx, "wf-a", "fresh"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WebhookTokenByToken(ctx, "tok1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("old token must stop working")
		}
		toks, _ := s.ListWebhookTokens(ctx)
		if len(toks) != 1 || toks[0].Token != "fresh" {
			t.Fatal(toks)
		}
		_ = s.DeleteWebhookToken(ctx, "wf-a")
		if err := s.RotateWebhookToken(ctx, "wf-a", "x"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}

		next := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
		if err := s.PutCronState(ctx, &CronState{Workflow: "w", Idx: 0, Expr: "0 * * * *", NextAt: next}); err != nil {
			t.Fatal(err)
		}
		fired := next
		if err := s.PutCronState(ctx, &CronState{Workflow: "w", Idx: 0, Expr: "0 * * * *", NextAt: next.Add(time.Hour), LastFiredAt: &fired}); err != nil {
			t.Fatal(err)
		}
		cs, _ := s.ListCronStates(ctx)
		if len(cs) != 1 || !cs[0].NextAt.Equal(next.Add(time.Hour)) || !cs[0].LastFiredAt.Equal(fired) {
			t.Fatalf("%+v", cs[0])
		}
		_ = s.DeleteCronState(ctx, "w", 0)
		cs, _ = s.ListCronStates(ctx)
		if len(cs) != 0 {
			t.Fatal("delete")
		}
	})
}
