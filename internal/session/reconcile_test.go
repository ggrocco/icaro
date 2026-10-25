package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

func (e *env) state(id string) (st docker.State) {
	st, _ = e.eng.Inspect(context.Background(), id)
	return st
}

// TestOrphanStopsLiveContainer: marking a session orphaned stops (not
// removes) a container that is still up, so no credentialed agent keeps
// running without a session that supervises it.
func TestOrphanStopsLiveContainer(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 8)
	ctx := context.Background()

	// Caught mid-create by a daemon restart, with its container running.
	cid, _ := e.eng.CreateContainerOn(ctx, docker.Spec{Name: "mid"}, docker.NetSpec{})
	_ = e.eng.StartContainer(ctx, cid)
	mid := &store.SessionRow{Kind: KindAgent, Profile: "claude", Status: StatusCreating, WorktreePath: t.TempDir(), ContainerID: cid}
	if err := e.st.CreateSession(ctx, mid); err != nil {
		t.Fatal(err)
	}
	if err := e.idle().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Get(ctx, mid.ID); got.Status != StatusOrphaned || got.ContainerID != cid {
		t.Fatalf("%+v", got)
	}
	if st := e.state(cid); !st.Exists || st.Running {
		t.Fatalf("the orphan's container must be stopped and kept: %+v", st)
	}

	// The probe does the same for a running session whose worktree went.
	s := e.create(t, "s")
	if err := os.RemoveAll(s.WorktreePath); err != nil {
		t.Fatal(err)
	}
	e.m.probe(ctx)
	if got := e.waitStatus(t, s.ID, StatusOrphaned); !strings.Contains(got.Error, "is gone") {
		t.Fatal(got.Error)
	}
	if st := e.state(s.ContainerID); !st.Exists || st.Running {
		t.Fatalf("%+v", st)
	}

	// An engine that cannot stop it still leaves the session orphaned.
	f := e.create(t, "f")
	if err := os.RemoveAll(f.WorktreePath); err != nil {
		t.Fatal(err)
	}
	e.eng.fail("stop", errBoom)
	e.m.probe(ctx)
	e.waitStatus(t, f.ID, StatusOrphaned)
	e.eng.fail("stop", nil)

	// A session another writer moved on is left as it is.
	r := e.create(t, "r")
	stale, _ := e.st.GetSession(ctx, r.ID)
	if _, err := e.m.Stop(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.m.orphan(ctx, stale, "late probe"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Get(ctx, r.ID); got.Status != StatusStopped {
		t.Fatalf("a late probe overwrote a stop: %+v", got)
	}
}

// TestReconcilePreview: reconciliation probes the preview facet too. A
// preview is optional, so a missing one never orphans the session; its
// stale reference is dropped.
func TestReconcilePreview(t *testing.T) {
	t.Parallel()
	e := newQuietEnv(t, 8)
	ctx := context.Background()
	wt := t.TempDir()
	pv := &store.PreviewInstance{WorktreePath: wt, Slug: "s", Hostname: "s.localhost", ComposeProject: "icaro-s", Status: store.PreviewRunning}
	if err := e.st.CreatePreview(ctx, pv); err != nil {
		t.Fatal(err)
	}
	live := func() string {
		id, _ := e.eng.CreateContainerOn(ctx, docker.Spec{Name: "x"}, docker.NetSpec{})
		_ = e.eng.StartContainer(ctx, id)
		return id
	}
	rows := map[string]*store.SessionRow{
		"kept":    {PreviewInstanceID: pv.ID, WorktreePath: wt, ContainerID: live()},
		"dropped": {PreviewInstanceID: "gone-preview", WorktreePath: wt, ContainerID: live()},
		"orphan":  {PreviewInstanceID: "gone-preview", WorktreePath: filepath.Join(wt, "gone"), ContainerID: live()},
	}
	for _, r := range rows {
		r.Kind, r.Profile, r.Status = KindAgent, "claude", StatusRunning
		if err := e.st.CreateSession(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.idle().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct{ status, preview string }{
		"kept":    {StatusRunning, pv.ID},
		"dropped": {StatusRunning, ""},
		"orphan":  {StatusOrphaned, ""},
	} {
		got, _ := e.m.Get(ctx, rows[name].ID)
		if got.Status != want.status || got.PreviewInstanceID != want.preview {
			t.Errorf("%s: %+v, want %s with preview %q", name, got, want.status, want.preview)
		}
	}

	// Store failures surface from Reconcile.
	again := &store.SessionRow{Kind: KindAgent, Profile: "claude", Status: StatusRunning, PreviewInstanceID: "gone-preview", WorktreePath: wt, ContainerID: live()}
	if err := e.st.CreateSession(ctx, again); err != nil {
		t.Fatal(err)
	}
	e.failWhen(t, "dropref", "UPDATE", "sessions", "NEW.preview_instance_id = '' AND OLD.preview_instance_id <> ''")
	if err := e.idle().Reconcile(ctx); err == nil {
		t.Fatal("a reference that cannot be dropped must fail reconciliation")
	}
	e.sql(t, "DROP TRIGGER dropref")
	e.sql(t, "ALTER TABLE preview_instances RENAME TO hidden_previews")
	if err := e.idle().Reconcile(ctx); err == nil {
		t.Fatal("an unreadable preview must fail reconciliation")
	}
}
