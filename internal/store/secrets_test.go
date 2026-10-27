package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSecretRows(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		defer func() { Now = time.Now }()
		Now = func() time.Time { return t0 }

		if err := s.PutSecret(ctx, &SecretRow{Name: "gh", ValueCT: []byte{1, 2, 3}, Hosts: []string{"api.github.com"}}); err != nil {
			t.Fatal(err)
		}
		// No binding is stored as an empty list, never as SQL NULL.
		if err := s.PutSecret(ctx, &SecretRow{Name: "env-only", ValueCT: []byte{9}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSecret(ctx, "env-only")
		if err != nil || got.Hosts == nil || len(got.Hosts) != 0 {
			t.Fatalf("unbound secret: %+v %v", got, err)
		}

		// Replacing a value keeps created_at and moves updated_at.
		Now = func() time.Time { return t0.Add(time.Hour) }
		if err := s.PutSecret(ctx, &SecretRow{Name: "gh", ValueCT: []byte{4}, Hosts: []string{"*.github.com"}}); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetSecret(ctx, "gh")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.ValueCT) != "\x04" || len(got.Hosts) != 1 || got.Hosts[0] != "*.github.com" {
			t.Fatalf("replace: %+v", got)
		}
		if !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("timestamps: created %v updated %v", got.CreatedAt, got.UpdatedAt)
		}

		list, err := s.ListSecrets(ctx)
		if err != nil || len(list) != 2 || list[0].Name != "env-only" || list[1].Name != "gh" {
			t.Fatalf("list must be ordered by name: %+v %v", list, err)
		}

		if err := s.DeleteSecret(ctx, "gh"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSecret(ctx, "gh"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted secret: %v", err)
		}
		if err := s.DeleteSecret(ctx, "gh"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}
	})
}

func TestUISessions(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		defer func() { Now = time.Now }()
		Now = func() time.Time { return t0 }

		code := &UISessionRow{ID: "code-hash", Kind: UILoginCode, Name: "cli", Scope: "admin", ExpiresAt: t0.Add(time.Minute)}
		sess := &UISessionRow{ID: "sess-hash", Kind: UISession, Name: "browser", Scope: "admin", ExpiresAt: t0.Add(24 * time.Hour)}
		for _, r := range []*UISessionRow{code, sess} {
			if err := s.CreateUISession(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetUISession(ctx, "sess-hash", UISession)
		if err != nil || got.Name != "browser" || got.Scope != "admin" || !got.ExpiresAt.Equal(sess.ExpiresAt) || !got.CreatedAt.Equal(t0) {
			t.Fatalf("get: %+v %v", got, err)
		}
		// The kind is part of the key: a cookie value is not a login code.
		if _, err := s.GetUISession(ctx, "sess-hash", UILoginCode); !errors.Is(err, ErrNotFound) {
			t.Fatalf("kind mismatch: %v", err)
		}
		if _, err := s.ConsumeUILoginCode(ctx, "sess-hash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session id consumed as a code: %v", err)
		}

		// Login codes are single use.
		c, err := s.ConsumeUILoginCode(ctx, "code-hash")
		if err != nil || c.Name != "cli" {
			t.Fatalf("consume: %+v %v", c, err)
		}
		if _, err := s.ConsumeUILoginCode(ctx, "code-hash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second consume: %v", err)
		}

		// Purge removes only expired rows.
		if err := s.CreateUISession(ctx, &UISessionRow{ID: "old", Kind: UISession, ExpiresAt: t0.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		Now = func() time.Time { return t0.Add(time.Hour) }
		n, err := s.PurgeUISessions(ctx)
		if err != nil || n != 1 {
			t.Fatalf("purge: %d %v", n, err)
		}
		if _, err := s.GetUISession(ctx, "sess-hash", UISession); err != nil {
			t.Fatalf("live session purged: %v", err)
		}

		if err := s.DeleteUISession(ctx, "sess-hash"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetUISession(ctx, "sess-hash", UISession); !errors.Is(err, ErrNotFound) {
			t.Fatalf("sign-out: %v", err)
		}
	})
}

// TestRevokeTokenEndsUISessions is the regression test for browser
// sessions outliving the API token they were signed in with.
func TestRevokeTokenEndsUISessions(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		tok, err := s.CreateToken(ctx, "ci", "h-ci", ScopeAdmin)
		if err != nil {
			t.Fatal(err)
		}
		other, err := s.CreateToken(ctx, "other", "h-other", ScopeAdmin)
		if err != nil {
			t.Fatal(err)
		}
		exp := Now().Add(time.Hour)
		for _, r := range []*UISessionRow{
			{ID: "from-ci", Kind: UISession, Name: "ci", Scope: ScopeAdmin, TokenID: tok.ID, ExpiresAt: exp},
			{ID: "from-other", Kind: UISession, Name: "other", Scope: ScopeAdmin, TokenID: other.ID, ExpiresAt: exp},
			{ID: "from-code", Kind: UISession, Name: "local", Scope: ScopeWrite, ExpiresAt: exp},
		} {
			if err := s.CreateUISession(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.GetUISession(ctx, "from-ci", UISession); err != nil || got.TokenID != tok.ID {
			t.Fatalf("token id not stored: %+v %v", got, err)
		}
		if err := s.RevokeToken(ctx, "ci"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetUISession(ctx, "from-ci", UISession); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a session from a revoked token still resolves: %v", err)
		}
		for _, id := range []string{"from-other", "from-code"} {
			if got, err := s.GetUISession(ctx, id, UISession); err != nil {
				t.Fatalf("%s ended by an unrelated revoke: %v", id, err)
			} else if id == "from-code" && got.TokenID != "" {
				t.Fatalf("login-code session has a token: %+v", got)
			}
		}
		if err := s.RevokeToken(ctx, "ci"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoke twice: %v", err)
		}
	})
}
