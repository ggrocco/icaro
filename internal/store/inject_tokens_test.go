package store

import (
	"context"
	"errors"
	"testing"
)

func TestInjectTokens(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		row := &SessionRow{Kind: "agent", Profile: "claude", Status: "running"}
		if err := s.CreateSession(ctx, row); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InjectTokenHash(ctx, row.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no token yet: %v", err)
		}
		if err := s.PutInjectToken(ctx, row.ID, "h1"); err != nil {
			t.Fatal(err)
		}
		// A restart replaces the token.
		if err := s.PutInjectToken(ctx, row.ID, "h2"); err != nil {
			t.Fatal(err)
		}
		if h, err := s.InjectTokenHash(ctx, row.ID); err != nil || h != "h2" {
			t.Fatalf("%q %v", h, err)
		}
		if err := s.DeleteInjectToken(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteInjectToken(ctx, row.ID); err != nil {
			t.Fatal("deleting a missing token is not an error")
		}
		if _, err := s.InjectTokenHash(ctx, row.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		// Removing the session removes its token.
		if err := s.PutInjectToken(ctx, row.ID, "h3"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSession(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InjectTokenHash(ctx, row.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("token outlived its session: %v", err)
		}
		// A token cannot name a session that does not exist.
		if err := s.PutInjectToken(ctx, "nope", "h"); err == nil {
			t.Fatal("token for an unknown session accepted")
		}
	})
}
