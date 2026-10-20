package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/apiclient"
	"icaro/internal/config"
	"icaro/internal/crypto"
	"icaro/internal/secrets"
	"icaro/internal/service"
	"icaro/internal/session"
	"icaro/internal/store"
)

func TestSecretAndProfileRoutes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.NewKey()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("ICARO_HOME", t.TempDir())
	t.Setenv("ICARO_CONFIG", "")
	cfg, _ := config.Load("")
	srv := New(service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet}), quiet)
	sec := secrets.New(st, key)
	srv.RegisterSessions(SessionDeps{Secrets: sec, Config: cfg})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	mk := func(name, scope string) *apiclient.Client {
		plain, hash, _ := crypto.NewToken()
		if _, err := st.CreateToken(ctx, name, hash, scope); err != nil {
			t.Fatal(err)
		}
		return apiclient.New(hs.URL, plain)
	}
	admin, reader := mk("admin", store.ScopeAdmin), mk("reader", store.ScopeRead)

	if err := reader.SetSecret(ctx, "gh", "ghp_123456", nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("reader must not set secrets: %v", err)
	}
	if err := admin.SetSecret(ctx, "gh", "ghp_123456", []string{"api.github.com"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.SetSecret(ctx, "bad name", "x", nil); err == nil {
		t.Fatal("invalid name accepted")
	}
	infos, err := reader.ListSecrets(ctx)
	if err != nil || len(infos) != 1 || infos[0].Hosts[0] != "api.github.com" {
		t.Fatalf("%+v %v", infos, err)
	}
	var raw []byte
	if err := reader.Do(ctx, "GET", "/api/v1/secrets", nil, &raw); err != nil || strings.Contains(string(raw), "ghp_123456") {
		t.Fatalf("a secret value left the daemon: %s", raw)
	}
	if err := admin.DeleteSecret(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if err := admin.DeleteSecret(ctx, "gh"); !apiclient.IsNotFound(err) {
		t.Fatalf("%v", err)
	}
	ps, err := reader.ListProfiles(ctx)
	if err != nil || len(ps) < 4 || ps[0].Name != "claude" || len(ps[0].Egress) == 0 {
		t.Fatalf("%+v %v", ps, err)
	}
}

func TestSessionErrMapping(t *testing.T) {
	if !errors.Is(sessionErr(session.ErrNotFound), service.ErrNotFound) {
		t.Fatal("not found")
	}
	var br *service.BadRequest
	if !errors.As(sessionErr(&session.ErrInvalid{Msg: "x"}), &br) || br.Msg != "x" {
		t.Fatal("invalid")
	}
	other := errors.New("boom")
	if sessionErr(other) != other {
		t.Fatal("passthrough")
	}
}
