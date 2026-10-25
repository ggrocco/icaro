package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icaro/internal/creds"
	"icaro/internal/store"
)

// TestCheckLogin: a login is refused while a session of the profile may
// use its seed (running ones only in overlay mode), unless forced.
func TestCheckLogin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	overlay := func(o *Options) { o.Homes = &creds.Homes{Layout: o.Config.Layout(), Mode: creds.ModeOverlay} }
	e := newEnv(t, 1, overlay)
	proj, err := e.st.UpsertProject(ctx, &store.Project{Name: "p", RootPath: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	add := func(profile, status string) *store.SessionRow {
		t.Helper()
		r := &store.SessionRow{Kind: KindAgent, ProjectID: proj.ID, Profile: profile, Branch: "b-" + status + profile, WorktreePath: "/w/" + status + profile, Status: status}
		if err := e.st.CreateSession(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	add("claude", StatusStopped)
	add("codex", StatusRunning)
	if err := e.m.CheckLogin(ctx, "claude", false); err != nil {
		t.Fatalf("stopped sessions and other profiles do not block: %v", err)
	}
	run := add("claude", StatusRunning)
	err = e.m.CheckLogin(ctx, "claude", false)
	var busy *ErrBusy
	if !errors.As(err, &busy) || !strings.Contains(busy.Msg, run.ID+" (running)") || !strings.Contains(busy.Msg, "icaro session stop") {
		t.Fatalf("a running overlay session must block: %v", err)
	}
	if err := e.m.CheckLogin(ctx, "claude", true); err != nil {
		t.Fatalf("force: %v", err)
	}

	// Copy mode: running sessions have their own copy; a queued one would
	// copy the seed mid-login.
	c := newEnv(t, 1)
	proj, err = c.st.UpsertProject(ctx, &store.Project{Name: "p", RootPath: c.repo})
	if err != nil {
		t.Fatal(err)
	}
	e = c
	add("claude", StatusRunning)
	if err := c.m.CheckLogin(ctx, "claude", false); err != nil {
		t.Fatalf("copy mode, running: %v", err)
	}
	add("claude", StatusQueued)
	if err := c.m.CheckLogin(ctx, "claude", false); !errors.As(err, &busy) {
		t.Fatalf("copy mode, queued: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.m.CheckLogin(canceled, "claude", false); err == nil || errors.As(err, &busy) {
		t.Fatalf("a store error surfaces: %v", err)
	}
}
