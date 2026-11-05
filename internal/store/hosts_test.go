package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRemoteHostRows(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		defer func() { Now = time.Now }()
		Now = func() time.Time { return t0 }

		for _, r := range []*RemoteHostRow{
			{Alias: "prod", Label: "Production", URL: "https://icaro.example.com", TokenCT: []byte{1}, Principal: "ci", Scope: "write"},
			{Alias: "lab", Label: "Lab box", URL: "http://10.0.0.2:8787", TokenCT: []byte{2}, Principal: "me", Scope: "admin"},
		} {
			if err := s.PutRemoteHost(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetRemoteHost(ctx, "prod")
		if err != nil || got.Label != "Production" || got.URL != "https://icaro.example.com" || string(got.TokenCT) != "\x01" ||
			got.Principal != "ci" || got.Scope != "write" || !got.CreatedAt.Equal(t0) {
			t.Fatalf("get: %+v %v", got, err)
		}

		// Signing in again under the same alias replaces it and keeps
		// created_at.
		Now = func() time.Time { return t0.Add(time.Hour) }
		if err := s.PutRemoteHost(ctx, &RemoteHostRow{Alias: "prod", Label: "Prod", URL: "https://other.example.com", TokenCT: []byte{3}, Principal: "me", Scope: "read"}); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetRemoteHost(ctx, "prod")
		if err != nil || got.Label != "Prod" || got.URL != "https://other.example.com" || string(got.TokenCT) != "\x03" ||
			got.Scope != "read" || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("replace: %+v %v", got, err)
		}

		list, err := s.ListRemoteHosts(ctx)
		if err != nil || len(list) != 2 || list[0].Alias != "lab" || list[1].Alias != "prod" {
			t.Fatalf("list must be ordered by alias: %+v %v", list, err)
		}

		if err := s.DeleteRemoteHost(ctx, "prod"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetRemoteHost(ctx, "prod"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted host: %v", err)
		}
		if err := s.DeleteRemoteHost(ctx, "prod"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}
	})
}
