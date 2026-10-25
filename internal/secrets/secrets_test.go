package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"icaro/internal/crypto"
	"icaro/internal/store"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	return New(db, key)
}

func TestSecretsLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "github.token", "ghp_abcdef123", []string{"API.github.com", "*.githubusercontent.com"}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Resolve(ctx, "github.token"); err != nil || v != "ghp_abcdef123" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := s.ResolveFor(ctx, "github.token", "api.github.com:443"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveFor(ctx, "github.token", "raw.githubusercontent.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveFor(ctx, "github.token", "evil.example.com"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("binding not enforced: %v", err)
	}
	if _, err := s.ResolveFor(ctx, "github.token", "githubusercontent.com.evil"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatal("suffix trick")
	}
	infos, _ := s.List(ctx)
	if len(infos) != 1 || infos[0].Hosts[0] != "*.githubusercontent.com" {
		t.Fatalf("%+v", infos)
	}
	vals, _ := s.Values(ctx)
	names, _ := s.Names(ctx)
	if len(vals) != 1 || names[0] != "github.token" {
		t.Fatal("values/names")
	}
	if err := s.Delete(ctx, "github.token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, "github.token"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, bad := range [][2]string{{"1bad", "v"}, {"ok", ""}} {
		if err := s.Set(ctx, bad[0], bad[1], nil); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if err := s.Set(ctx, "x", "v", []string{"https://x"}); err == nil {
		t.Fatal("url host accepted")
	}
	if HostAllowed(nil, "a") {
		t.Fatal("empty binding")
	}
	// A wildcard must keep at least two labels: *.com would bind a
	// credential to every .com host.
	for _, bad := range []string{"*.com", "*.com.", "*", "*.*.example.com", "api.*.example.com"} {
		if err := s.Set(ctx, "x", "v", []string{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if err := s.Set(ctx, "x", "v", []string{"*.co.uk", "*.sentry.io"}); err != nil {
		t.Fatal(err)
	}
	if HostAllowed([]string{"*.com"}, "evil.com") || !HostAllowed([]string{"*.sentry.io"}, "us.sentry.io") {
		t.Fatal("a stored top-level wildcard must match nothing")
	}
}
