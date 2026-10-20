package store

import (
	"context"
	"errors"
	"testing"
)

func TestPreviewInstances(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		p := &PreviewInstance{WorktreePath: "/w/a", Slug: "a", Hostname: "a.localhost", ComposeProject: "icaro-a-1", SessionID: "S1"}
		if err := s.CreatePreview(ctx, p); err != nil {
			t.Fatal(err)
		}
		dup := &PreviewInstance{WorktreePath: "/w/b", Slug: "a", Hostname: "a.localhost", ComposeProject: "icaro-a-2"}
		if err := s.CreatePreview(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate hostname: %v", err)
		}
		p.Status, p.Port, p.Service, p.URL, p.SessionID = PreviewRunning, 80, "web", "http://a.localhost:8780", ""
		if err := s.UpdatePreview(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetPreviewByPath(ctx, "/w/a")
		if err != nil || got.Status != PreviewRunning || got.Port != 80 || got.SessionID != "" || got.ID != p.ID {
			t.Fatalf("%+v %v", got, err)
		}
		if _, err := s.GetPreview(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListPreviews(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("%v %v", list, err)
		}
		if err := s.AllowPreviewFinding(ctx, "/w/a", "web/privileged", "local"); err != nil {
			t.Fatal(err)
		}
		if err := s.AllowPreviewFinding(ctx, "/w/a", "web/privileged", "local"); err != nil {
			t.Fatal("idempotent allow:", err)
		}
		al, err := s.PreviewAllowances(ctx, "/w/a")
		if err != nil || !al["web/privileged"] || len(al) != 1 {
			t.Fatalf("%v %v", al, err)
		}
		if err := s.ClearPreviewAllowances(ctx, "/w/a"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePreview(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetPreview(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if err := s.UpdatePreview(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}
