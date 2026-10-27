package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"icaro/internal/creds"
	"icaro/internal/store"
)

// A login holds its profile: a session created meanwhile waits queued, so
// nothing reads the seed while the login rewrites it, and starts when the
// login ends.
func TestLoginLeaseHoldsSessionStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, 4)
	profile := e.cfg.Sessions.DefaultProfile
	if err := e.m.BeginLogin(ctx, profile, false); err != nil {
		t.Fatal(err)
	}
	s, err := e.m.Create(ctx, CreateRequest{RepoPath: e.repo, Name: "during-login"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusQueued {
		t.Fatalf("a session of a profile under login must wait: %s", s.Status)
	}
	e.m.nudge()
	time.Sleep(50 * time.Millisecond)
	if got, _ := e.m.Get(ctx, s.ID); got.Status != StatusQueued {
		t.Fatalf("still held: %s", got.Status)
	}
	e.m.EndLogin(profile)
	e.waitStatus(t, s.ID, StatusRunning)
}

func TestLoginLeaseRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, 4)
	if err := e.m.BeginLogin(ctx, "claude", false); err != nil {
		t.Fatal(err)
	}
	var busy *ErrBusy
	if err := e.m.BeginLogin(ctx, "claude", false); !errors.As(err, &busy) || !strings.Contains(busy.Msg, "already in progress") {
		t.Fatalf("one login per profile at a time: %v", err)
	}
	if !e.m.loginHeld("claude") || e.m.loginHeld("codex") {
		t.Fatal("the lease is per profile")
	}
	if err := e.m.BeginLogin(ctx, "claude", true); err != nil {
		t.Fatalf("force replaces the lease: %v", err)
	}
	e.m.EndLogin("claude")
	if e.m.loginHeld("claude") {
		t.Fatal("released")
	}

	// A lease the CLI never released expires.
	e.m.logins.do(func(l map[string]time.Time) { l["claude"] = time.Now().Add(-time.Second) })
	if e.m.loginHeld("claude") {
		t.Fatal("an expired lease does not hold the profile")
	}
	if err := e.m.BeginLogin(ctx, "claude", false); err != nil {
		t.Fatalf("an expired lease does not block a login: %v", err)
	}
	e.m.EndLogin("claude")

	// A refused login (sessions use the seed) leaves no lease behind.
	o := newEnv(t, 1, func(o *Options) { o.Homes = &creds.Homes{Layout: o.Config.Layout(), Mode: creds.ModeOverlay} })
	proj, err := o.st.UpsertProject(ctx, &store.Project{Name: "p", RootPath: o.repo})
	if err != nil {
		t.Fatal(err)
	}
	row := &store.SessionRow{Kind: KindAgent, ProjectID: proj.ID, Profile: "claude", Branch: "b", WorktreePath: "/w/b", Status: StatusRunning}
	if err := o.st.CreateSession(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := o.m.BeginLogin(ctx, "claude", false); !errors.As(err, &busy) {
		t.Fatalf("busy: %v", err)
	}
	if o.m.loginHeld("claude") {
		t.Fatal("a refused login must not hold the profile")
	}
}
