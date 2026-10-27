package store

import (
	"context"
	"errors"
	"testing"
)

func TestProjectsAndSessions(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		p, err := s.UpsertProject(ctx, &Project{Name: "app", RootPath: "/src/app", Remote: "git@x:app.git"})
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.UpsertProject(ctx, &Project{Name: "app", RootPath: "/src/app", Remote: "git@x:app2.git"})
		if err != nil || again.ID != p.ID || again.Remote != "git@x:app2.git" {
			t.Fatalf("upsert: %+v %v", again, err)
		}
		if _, err := s.UpsertProject(ctx, &Project{Name: "app", RootPath: "/elsewhere"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("name collision: %v", err)
		}
		if got, err := s.GetProject(ctx, "app"); err != nil || got.ID != p.ID {
			t.Fatal(err)
		}
		ps, _ := s.ListProjects(ctx)
		if len(ps) != 1 {
			t.Fatal(ps)
		}

		a := &SessionRow{Kind: "agent", ProjectID: p.ID, Profile: "claude", Branch: "icaro/a", WorktreePath: "/w/a", Status: "creating"}
		if err := s.CreateSession(ctx, a); err != nil {
			t.Fatal(err)
		}
		b := &SessionRow{Kind: "run", Profile: "workflow", RunID: "R1", Status: "queued"}
		if err := s.CreateSession(ctx, b); err != nil {
			t.Fatal(err)
		}
		a.Status, a.ContainerID = "running", "c1"
		if err := s.UpdateSession(ctx, a); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSession(ctx, a.ID)
		if err != nil || got.ContainerID != "c1" || got.Status != "running" {
			t.Fatalf("%+v %v", got, err)
		}
		if r, err := s.GetSessionByRun(ctx, "R1"); err != nil || r.ID != b.ID {
			t.Fatal(err)
		}
		if n, _ := s.CountSessions(ctx, "", "running", "creating"); n != 1 {
			t.Fatalf("count %d", n)
		}
		ok, err := s.TransitionSession(ctx, b.ID, "running", "queued")
		if err != nil || !ok {
			t.Fatal("claim queued")
		}
		ok, _ = s.TransitionSession(ctx, b.ID, "running", "queued")
		if ok {
			t.Fatal("double claim")
		}
		if err := s.HeartbeatSession(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		list, _ := s.ListSessions(ctx, SessionFilter{Kind: "agent", ProjectID: p.ID})
		if len(list) != 1 || list[0].LastHeartbeat == nil {
			t.Fatalf("%+v", list)
		}

		if err := s.AddSessionRepo(ctx, &SessionRepo{SessionID: b.ID, Ref: "r", Revision: "main", Path: "/x"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddSessionRepo(ctx, &SessionRepo{SessionID: b.ID, Ref: "r", Revision: "main", Path: "/y"}); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if r, err := s.GetSessionRepo(ctx, b.ID, "r", "main"); err != nil || r.Path != "/x" {
			t.Fatal(err)
		}
		if rs, _ := s.ListSessionRepos(ctx, b.ID); len(rs) != 1 {
			t.Fatal(rs)
		}
		if err := s.DeleteSession(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSessionRepo(ctx, b.ID, "r", "main"); !errors.Is(err, ErrNotFound) {
			t.Fatal("cascade")
		}

		if err := s.PinImage(ctx, "img:1", "img@sha256:aa"); err != nil {
			t.Fatal(err)
		}
		if err := s.PinImage(ctx, "img:1", "img@sha256:bb"); err != nil {
			t.Fatal(err)
		}
		if pi, err := s.GetPinnedImage(ctx, "img:1"); err != nil || pi.Digest != "img@sha256:bb" {
			t.Fatal(err)
		}
		if l, _ := s.ListPinnedImages(ctx); len(l) != 1 {
			t.Fatal(l)
		}
		if err := s.TouchRepository(ctx, "git@x:app.git", "/r/app.git"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateSessionIf(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		r := &SessionRow{Kind: "agent", Profile: "claude", Status: "creating"}
		if err := s.CreateSession(ctx, r); err != nil {
			t.Fatal(err)
		}
		// A stop lands while the start is still creating the container.
		if ok, err := s.TransitionSession(ctx, r.ID, "stopped", "creating", "running"); err != nil || !ok {
			t.Fatalf("stop: %v %v", ok, err)
		}
		r.Status, r.ContainerID = "running", "c1"
		if ok, err := s.UpdateSessionIf(ctx, r, "creating"); err != nil || ok {
			t.Fatalf("the start must not overwrite the stop: %v %v", ok, err)
		}
		if got, _ := s.GetSession(ctx, r.ID); got.Status != "stopped" || got.ContainerID != "" {
			t.Fatalf("%+v", got)
		}
		r.Status = "queued"
		if ok, err := s.UpdateSessionIf(ctx, r, "stopped", "error"); err != nil || !ok {
			t.Fatalf("matching status: %v %v", ok, err)
		}
		if got, _ := s.GetSession(ctx, r.ID); got.Status != "queued" || got.ContainerID != "c1" {
			t.Fatalf("%+v", got)
		}
		if _, err := s.UpdateSessionIf(ctx, r); err == nil {
			t.Fatal("no expected status")
		}
		if ok, err := s.UpdateSessionIf(ctx, &SessionRow{ID: "gone"}, "queued"); err != nil || ok {
			t.Fatalf("missing row: %v %v", ok, err)
		}
		_ = s.Close()
		if _, err := s.UpdateSessionIf(ctx, r, "queued"); err == nil {
			t.Fatal("closed store")
		}
	})
}
