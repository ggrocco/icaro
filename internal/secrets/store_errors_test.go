package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"icaro/internal/crypto"
	"icaro/internal/store"
)

func TestSetNormalizesHosts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "slack", "xoxb", []string{" ", "Hooks.Slack.com ", ""}); err != nil {
		t.Fatal(err)
	}
	infos, err := s.List(ctx)
	if err != nil || len(infos) != 1 || len(infos[0].Hosts) != 1 || infos[0].Hosts[0] != "hooks.slack.com" {
		t.Fatalf("blank hosts must be dropped: %+v %v", infos, err)
	}
	if err := s.Set(ctx, "env-only", "v", nil); err != nil {
		t.Fatal(err)
	}
	infos, _ = s.List(ctx)
	for _, in := range infos {
		if in.Hosts == nil {
			t.Fatalf("%s: hosts must serialize as [], not null", in.Name)
		}
	}
	if _, err := s.ResolveFor(ctx, "env-only", "example.com"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("an unbound secret is never sent anywhere: %v", err)
	}
	if _, err := s.ResolveFor(ctx, "missing", "example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if orEmpty(nil) == nil {
		t.Fatal("orEmpty(nil)")
	}
}

// TestWrongMasterKey: values sealed under another master key never come
// back as plaintext, and Values (log redaction) skips them silently.
func TestWrongMasterKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "gh", "ghp_x", []string{"api.github.com"}); err != nil {
		t.Fatal(err)
	}
	other, _ := crypto.NewKey()
	rekeyed := New(s.db, other)
	if _, err := rekeyed.Resolve(ctx, "gh"); err == nil {
		t.Fatal("Resolve with the wrong key")
	}
	if _, err := rekeyed.ResolveFor(ctx, "gh", "api.github.com"); err == nil {
		t.Fatal("ResolveFor with the wrong key")
	}
	if vals, err := rekeyed.Values(ctx); err != nil || len(vals) != 0 {
		t.Fatalf("values: %v %v", vals, err)
	}
}

func TestStoreErrors(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	s := New(db, key)
	_ = db.Close()
	for name, err := range map[string]error{
		"Resolve":    func() error { _, err := s.Resolve(ctx, "x"); return err }(),
		"ResolveFor": func() error { _, err := s.ResolveFor(ctx, "x", "h"); return err }(),
		"List":       func() error { _, err := s.List(ctx); return err }(),
		"Names":      func() error { _, err := s.Names(ctx); return err }(),
		"Values":     func() error { _, err := s.Values(ctx); return err }(),
	} {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a closed database: %v", name, err)
		}
	}
}
