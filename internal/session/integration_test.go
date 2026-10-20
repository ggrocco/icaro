//go:build integration

package session

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/store"
)

const itImage = "mirror.gcr.io/library/busybox:1.36"

func staticIcaro(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(t.TempDir(), "icaro")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/icaro")
	cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	return out
}

func dockerOut(args ...string) (string, error) {
	var b bytes.Buffer
	c := exec.Command("docker", args...)
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.String(), err
}

// TestSessionSpineDocker: two sessions on two worktrees run concurrently
// in real containers, exec preserves the session UID and /workspace, and a
// daemon restart re-attaches live sessions and orphans dead ones.
func TestSessionSpineDocker(t *testing.T) {
	eng, err := docker.New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := eng.Ping(ctx); err != nil {
		t.Skipf("docker: %v", err)
	}
	if err := eng.EnsureImage(ctx, itImage); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	profile := "it" + time.Now().Format("150405")
	cfg.Profiles[profile] = config.ProfileConfig{Agent: "claude", Image: itImage, Egress: []string{"api.anthropic.com"}}
	cfg.Sessions.DefaultProfile = profile
	st, err := store.Open(ctx, store.SQLite, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // runs after the session cleanup below
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(repo, 0o755)
	gitInit(t, repo)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	egress := &container.DockerEgress{Engine: eng, HelperBinary: staticIcaro(t), Log: log}
	homes := &creds.Homes{Layout: cfg.Layout(), Volumes: eng}
	// A root daemon hands the worktree to an arbitrary container user; a
	// normal daemon (CI) can only run containers as itself, as in the app.
	uid := 4321
	if os.Getuid() != 0 {
		uid = os.Getuid()
	}
	mk := func() *Manager {
		return New(Options{Store: st, Config: cfg, Engine: eng, Egress: egress, Homes: homes,
			Images: &container.Images{Engine: eng, Pins: st}, Log: log, MaxActive: 4, UID: uid, GID: uid})
	}
	m := mk()
	runCtx, stop := context.WithCancel(ctx)
	go func() { _ = m.Run(runCtx) }()
	var created []*Session
	t.Cleanup(func() {
		for _, s := range created {
			if err := m.Remove(context.Background(), s.ID, false); err != nil {
				t.Errorf("cleanup %s: %v", s.ID, err)
			}
		}
		ids, _ := eng.ListByLabel(context.Background(), map[string]string{container.LabelEgress: profile})
		for _, id := range ids {
			_ = eng.RemoveContainer(context.Background(), id)
		}
		_ = eng.RemoveNetwork(context.Background(), container.NetworkName(profile))
	})
	for _, name := range []string{"one", "two"} {
		s, err := m.Create(ctx, CreateRequest{RepoPath: repo, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, s)
		if s.Status != StatusRunning {
			t.Fatalf("%s: %s %s", name, s.Status, s.Error)
		}
	}
	for i, s := range created {
		info, err := m.Exec(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		script := "id -u; pwd; echo hi > /workspace/from-" + s.ID + " && echo written; ls /icaro >/dev/null && echo icaro-dir; cat /home/icaro/.claude.json | grep -c mcp.sock || true"
		out, err := dockerOut(container.ExecArgs(container.ExecOptions{Container: info.ContainerID, User: info.User, Cmd: []string{"sh", "-c", script}})...)
		if err != nil {
			t.Fatalf("session %d exec: %v %s", i, err, out)
		}
		if !strings.HasPrefix(out, strconv.Itoa(uid)+"\n/workspace\nwritten\nicaro-dir") {
			t.Fatalf("session %d: %q", i, out)
		}
		other := created[1-i]
		if _, err := os.Stat(filepath.Join(other.WorktreePath, "from-"+s.ID)); !os.IsNotExist(err) {
			t.Fatal("sessions share a worktree")
		}
		if _, err := os.Stat(filepath.Join(s.WorktreePath, "from-"+s.ID)); err != nil {
			t.Fatal("write did not reach the host worktree")
		}
	}
	// git works inside the container against the worktree.
	info, _ := m.Exec(ctx, created[0].ID)
	if out, err := dockerOut(container.ExecArgs(container.ExecOptions{Container: info.ContainerID, User: info.User, Cmd: []string{"sh", "-c", "test -f /workspace/.git && echo gitfile"}})...); err != nil || !strings.Contains(out, "gitfile") {
		t.Fatalf("%v %s", err, out)
	}

	// Daemon "restart": stop the manager, kill one container, reconcile.
	stop()
	time.Sleep(100 * time.Millisecond)
	if err := eng.RemoveContainer(ctx, created[1].ContainerID); err != nil {
		t.Fatal(err)
	}
	m = mk()
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Get(ctx, created[0].ID)
	b, _ := m.Get(ctx, created[1].ID)
	if a.Status != StatusRunning || b.Status != StatusOrphaned {
		t.Fatalf("after restart: %s / %s (%s)", a.Status, b.Status, b.Error)
	}
	// The orphan restarts cleanly.
	runCtx2, stop2 := context.WithCancel(ctx)
	defer stop2()
	go func() { _ = m.Run(runCtx2) }()
	b2, err := m.Start(ctx, b.ID)
	if err != nil || b2.Status != StatusRunning {
		t.Fatalf("restart orphan: %+v %v", b2, err)
	}
}
