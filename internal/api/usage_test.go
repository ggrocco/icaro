package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"icaro/internal/crypto"
	"icaro/internal/service"
	"icaro/internal/store"
	"icaro/internal/usage"
)

func TestUsageREST(t *testing.T) {
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
	srvAPI := New(service.New(service.Options{Store: st, Key: key, LogsDir: t.TempDir(), Log: quiet}), quiet)
	var gotSince string
	RegisterUsage(srvAPI, func(ctx context.Context, since string) (*usage.Report, error) {
		gotSince = since
		return usage.BuildReport(ctx, usage.ReportOptions{
			Roots: usage.ProfileRoots("../usage/testdata/home"), Timezone: "UTC", Since: since, LimitUSD: 0.000001,
			Now: time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC),
		})
	})
	srv := httptest.NewServer(srvAPI.Handler())
	t.Cleanup(srv.Close)
	plain, hash, _ := crypto.NewToken()
	if _, err := st.CreateToken(ctx, "reader", hash, store.ScopeRead); err != nil {
		t.Fatal(err)
	}
	get := func(path, tok string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, b
	}

	if res, _ := get("/api/v1/usage", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", res.StatusCode)
	}
	res, body := get("/api/v1/usage?since=30d", plain)
	if res.StatusCode != http.StatusOK || gotSince != "30d" {
		t.Fatalf("read token: %d %s (since %q)", res.StatusCode, body, gotSince)
	}
	var rep usage.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Timezone != "UTC" || !rep.Since.Equal(time.Date(2025, 2, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window: %+v", rep)
	}
	var profile bool
	for _, r := range rep.Records {
		if r.Source == "profile:claude" && r.Confidence == usage.ConfidenceHigh {
			profile = true
		}
	}
	if !profile || len(rep.Alerts) == 0 || rep.LimitUSD == 0 {
		t.Fatalf("records/alerts: %s", body)
	}

	res, body = get("/api/v1/usage?since=yesterday", plain)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since: %d %s", res.StatusCode, body)
	}
}
