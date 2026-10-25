package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/container/docker"
	"icaro/internal/container/sandbox"
	"icaro/internal/creds"
	"icaro/internal/paths"
	"icaro/internal/store"
)

// Engine is the Docker surface the manager needs.
type Engine interface {
	Ping(ctx context.Context) error
	CreateContainerOn(ctx context.Context, spec docker.Spec, net docker.NetSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout int) error
	RemoveContainer(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (docker.State, error)
}

// ImageResolver pins image references to digests.
type ImageResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// ErrNotFound reports an unknown session.
var ErrNotFound = errors.New("session not found")

// errStartAborted ends a start whose session was stopped or removed while
// it ran; the start has already removed what it created.
var errStartAborted = errors.New("the session was stopped while it was starting")

// allStatuses lets Stop claim a session from whatever state it is in.
var allStatuses = []string{StatusCreating, StatusRunning, StatusQueued, StatusStopped, StatusOrphaned, StatusError}

// ErrInvalid reports a bad request (unknown profile, name in use, ...).
type ErrInvalid struct{ Msg string }

func (e *ErrInvalid) Error() string { return e.Msg }

func invalid(format string, args ...any) error { return &ErrInvalid{Msg: fmt.Sprintf(format, args...)} }

// ErrUnavailable reports that Docker is not reachable, so a session cannot
// start now.
type ErrUnavailable struct{ Msg string }

func (e *ErrUnavailable) Error() string { return e.Msg }

// pingTimeout bounds the Docker check before a session is created.
const pingTimeout = 5 * time.Second

// Options wire a Manager.
type Options struct {
	Store   *store.Store
	Config  *config.Config
	Engine  Engine
	Egress  container.Egress
	Homes   *creds.Homes
	Images  ImageResolver
	Preview PreviewHook    // optional
	Memory  MemoryAttacher // optional
	Log     *slog.Logger
	// UID/GID the containers run as; defaults to the daemon's user, or
	// 1000:1000 when the daemon runs as root (containers are never root).
	UID, GID int
	// MaxActive overrides the configured/derived concurrency cap.
	MaxActive int
	// Tick is the liveness probe interval (default 30s).
	Tick time.Duration
}

// Manager owns the session lifecycle. One goroutine (Run) owns the
// decision to start a session, so the concurrency cap holds without locks;
// API callers send it start requests over a channel.
//
// Status changes are compare-and-set in the store: a start in flight
// writes only while its session is still creating, and Stop claims the
// session before it tears anything down, so a stop is never undone by the
// start it raced (the start notices, removes what it created and exits).
type Manager struct {
	o      Options
	layout paths.Layout
	policy container.MountPolicy

	startCh  chan chan struct{}
	wake     chan struct{}
	gitLock  chan struct{} // serializes repository-cache operations
	mcp      *registry
	starting *owned[chan struct{}] // session id -> closed when its start goroutine returns
}

// New builds a Manager; call Run to process starts.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.UID == 0 && o.GID == 0 {
		o.UID, o.GID = os.Getuid(), os.Getgid()
	}
	if o.UID == 0 {
		o.UID, o.GID = 1000, 1000
	}
	if o.Tick <= 0 {
		o.Tick = 30 * time.Second
	}
	home, _ := os.UserHomeDir()
	l := o.Config.Layout()
	return &Manager{
		o: o, layout: l,
		policy:  container.MountPolicy{Allow: o.Config.Mounts.Allow, Home: home},
		startCh: make(chan chan struct{}), wake: make(chan struct{}, 1), gitLock: make(chan struct{}, 1),
		mcp: newRegistry(), starting: newOwned[chan struct{}](),
	}
}

// SetPreviewHook attaches the preview facet. Call before Run.
func (m *Manager) SetPreviewHook(h PreviewHook) { m.o.Preview = h }

// SetMemoryAttacher attaches the memory facet. Call before Run.
func (m *Manager) SetMemoryAttacher(a MemoryAttacher) { m.o.Memory = a }

// Name implements app.Component.
func (m *Manager) Name() string { return "sessions" }

// MaxActive is the effective cap on running agent sessions.
func (m *Manager) MaxActive() int {
	switch {
	case m.o.MaxActive > 0:
		return m.o.MaxActive
	case m.o.Config.Sessions.MaxActive > 0:
		return m.o.Config.Sessions.MaxActive
	}
	return DefaultMaxActive()
}

// User is the uid:gid containers run as.
func (m *Manager) User() string { return fmt.Sprintf("%d:%d", m.o.UID, m.o.GID) }

// Run reconciles, then starts queued sessions as slots free up and
// probes running ones, until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.Reconcile(ctx); err != nil {
		m.o.Log.Error("session reconcile", "err", err)
	}
	m.fill(ctx)
	t := time.NewTicker(m.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case done := <-m.startCh:
			m.fill(ctx)
			close(done)
		case <-m.wake:
			m.fill(ctx)
		case <-t.C:
			m.probe(ctx)
			m.fill(ctx)
		}
	}
}

func (m *Manager) nudge() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// fill claims queued agent sessions while below the cap. Only Run calls
// it, so the count cannot race.
func (m *Manager) fill(ctx context.Context) {
	active, err := m.o.Store.CountSessions(ctx, KindAgent, StatusRunning, StatusCreating)
	if err != nil {
		m.o.Log.Error("count sessions", "err", err)
		return
	}
	queued, err := m.o.Store.ListSessions(ctx, store.SessionFilter{Kind: KindAgent, Statuses: []string{StatusQueued}})
	if err != nil {
		m.o.Log.Error("list queued", "err", err)
		return
	}
	for _, row := range queued {
		if active >= m.MaxActive() {
			return
		}
		// Register the start before claiming the session, so a Stop that
		// wins the session after the claim always finds a start to wait for.
		done, ok := m.beginStart(row.ID)
		if !ok {
			continue // a stopped start of this session is still unwinding
		}
		ok, err := m.o.Store.TransitionSession(ctx, row.ID, StatusCreating, StatusQueued)
		if err != nil || !ok {
			m.endStart(row.ID, done)
			continue
		}
		active++
		row.Status = StatusCreating
		go func(row *store.SessionRow) {
			defer m.endStart(row.ID, done)
			bg := context.WithoutCancel(ctx)
			switch err := m.startAgent(bg, row); {
			case errors.Is(err, errStartAborted):
				m.o.Log.Info("session start abandoned", "session", row.ID, "reason", err)
			case err != nil:
				m.o.Log.Error("session start failed", "session", row.ID, "err", err)
				row.Status, row.Error = StatusError, err.Error()
				_, _ = m.o.Store.UpdateSessionIf(bg, row, StatusCreating)
			}
			m.nudge()
		}(row)
	}
}

// beginStart records that a start goroutine for id is about to run. It
// refuses while an earlier start of id has not returned yet.
func (m *Manager) beginStart(id string) (chan struct{}, bool) {
	done := make(chan struct{})
	ok := false
	m.starting.do(func(s map[string]chan struct{}) {
		if s[id] == nil {
			s[id], ok = done, true
		}
	})
	return done, ok
}

// endStart marks the start of id finished.
func (m *Manager) endStart(id string, done chan struct{}) {
	m.starting.do(func(s map[string]chan struct{}) {
		if s[id] == done {
			delete(s, id)
		}
	})
	close(done)
}

// awaitStart waits until no start goroutine of id is running.
func (m *Manager) awaitStart(ctx context.Context, id string) error {
	var done chan struct{}
	m.starting.do(func(s map[string]chan struct{}) { done = s[id] })
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requestStart asks Run to consider queued sessions and waits for it to
// claim them (or leave them queued at the cap).
func (m *Manager) requestStart(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case m.startCh <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitSettled polls until the session leaves creating.
func (m *Manager) waitSettled(ctx context.Context, id string, limit time.Duration) (*store.SessionRow, error) {
	deadline := time.Now().Add(limit)
	for {
		row, err := m.o.Store.GetSession(ctx, id)
		if err != nil || row.Status != StatusCreating || time.Now().After(deadline) {
			return row, err
		}
		select {
		case <-ctx.Done():
			return row, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// CreateRequest describes a new agent session.
type CreateRequest struct {
	// RepoPath is any path inside the project's git checkout.
	RepoPath string `json:"repo_path"`
	// Name names the session, its branch (icaro/<name>) and worktree.
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Base is the commit-ish a new branch starts from (default HEAD).
	Base string `json:"base,omitempty"`
}

// Create makes a worktree and an agent session for it. The session starts
// at once when below the cap, otherwise it is persisted as queued.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Session, error) {
	profile := req.Profile
	if profile == "" {
		profile = m.o.Config.Sessions.DefaultProfile
	}
	if _, ok := m.o.Config.Profiles[profile]; !ok {
		return nil, invalid("unknown profile %q", profile)
	}
	info, err := InspectRepo(ctx, req.RepoPath)
	if err != nil {
		return nil, invalid("%v", err)
	}
	// Sessions start only while Docker answers (the manager's loop waits
	// for it), so fail now instead of leaving the caller waiting on a
	// worktree and a row that cannot start.
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	err = m.o.Engine.Ping(pctx)
	cancel()
	if err != nil {
		return nil, &ErrUnavailable{Msg: "docker is not reachable, so the session cannot start: " + err.Error()}
	}
	proj, err := m.project(ctx, info)
	if err != nil {
		return nil, err
	}
	row := &store.SessionRow{ID: store.NewID(), Kind: KindAgent, ProjectID: proj.ID, Profile: profile, Status: StatusCreating}
	slug := Slug(req.Name)
	if slug == "" {
		slug = strings.ToLower(row.ID[len(row.ID)-6:])
	}
	row.Branch = "icaro/" + slug
	row.WorktreePath = filepath.Join(m.layout.Worktrees(), proj.Name, slug)
	if _, err := os.Stat(row.WorktreePath); err == nil {
		return nil, invalid("a session named %q already exists for %s", slug, proj.Name)
	}
	if err := m.o.Store.CreateSession(ctx, row); err != nil {
		return nil, err
	}
	if err := addWorktree(ctx, info.Root, row.WorktreePath, row.Branch, req.Base); err != nil {
		row.Status, row.Error = StatusError, err.Error()
		_, _ = m.o.Store.UpdateSessionIf(ctx, row, StatusCreating)
		return nil, err
	}
	m.chownForContainer(row.WorktreePath, filepath.Join(info.GitDir, "worktrees", filepath.Base(row.WorktreePath)))
	if m.o.Preview != nil {
		pid, err := m.o.Preview.WorktreeAdded(ctx, row.ID, row.ProjectID, row.WorktreePath)
		if err != nil {
			m.o.Log.Warn("preview hook", "session", row.ID, "err", err)
		}
		row.PreviewInstanceID = pid
	}
	row.Status = StatusQueued
	if ok, err := m.o.Store.UpdateSessionIf(ctx, row, StatusCreating); err != nil {
		return nil, err
	} else if !ok {
		// Stopped or removed before it was queued: it does not start.
		return m.Get(ctx, row.ID)
	}
	if err := m.requestStart(ctx); err != nil {
		return nil, err
	}
	settled, err := m.waitSettled(ctx, row.ID, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	return toSession(settled), nil
}

// project registers the repository as a project with a unique name.
func (m *Manager) project(ctx context.Context, info RepoInfo) (*store.Project, error) {
	name := Slug(filepath.Base(info.Root))
	if name == "" {
		name = "project"
	}
	id := ProjectIDForRoot(info.Root)
	p, err := m.o.Store.UpsertProject(ctx, &store.Project{ID: id, Name: name, RootPath: info.Root, Remote: info.Remote})
	if errors.Is(err, store.ErrConflict) {
		p, err = m.o.Store.UpsertProject(ctx, &store.Project{ID: id, Name: name + "-" + repoKey(info.Root)[len(repoKey(info.Root))-6:], RootPath: info.Root, Remote: info.Remote})
	}
	return p, err
}

// startAgent creates and starts the agent container of a claimed session.
func (m *Manager) startAgent(ctx context.Context, row *store.SessionRow) error {
	cfg := m.o.Config
	prof := cfg.Profiles[row.Profile]
	agent, err := creds.LookupAgent(prof.Agent)
	if err != nil {
		return err
	}
	proj, err := m.o.Store.GetProject(ctx, row.ProjectID)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	image, err := m.o.Images.Resolve(ctx, prof.Image)
	if err != nil {
		return fmt.Errorf("image %s: %w", prof.Image, err)
	}
	att, err := m.o.Egress.Ensure(ctx, row.Profile, prof.Egress)
	if err != nil {
		return err
	}
	var extra []container.Mount
	for _, spec := range prof.Mounts {
		mt, err := container.ParseMount(spec)
		if err != nil {
			return err
		}
		checked, err := m.policy.Check(mt)
		if err != nil {
			return err
		}
		extra = append(extra, checked)
	}
	icaroDir := m.icaroDir(row.ID)
	if err := os.MkdirAll(icaroDir, 0o700); err != nil {
		return err
	}
	m.chownForContainer(icaroDir)

	build := func() (string, error) {
		home, err := m.o.Homes.Prepare(ctx, row.Profile, row.ID)
		if err != nil {
			return "", err
		}
		if err := agent.Configure(m.o.Homes.SessionFiles(row.Profile, row.ID), m.o.Memory != nil, cfg.Memory.Preamble); err != nil {
			return "", fmt.Errorf("configure %s: %w", agent.Name, err)
		}
		m.chownForContainer(filepath.Join(m.layout.Home(row.Profile), "sessions", row.ID))
		spec, ns := container.AgentSpec{
			Name:  "icaro-session-" + strings.ToLower(row.ID),
			Image: image, User: m.User(),
			Env: []string{"ICARO_SESSION=" + row.ID, "ICARO_PROJECT=" + proj.Name, "ICARO_PROFILE=" + row.Profile, "ICARO_BRANCH=" + row.Branch},
			Labels: map[string]string{
				container.LabelSession: row.ID, container.LabelKind: row.Kind, container.LabelProfile: row.Profile,
			},
			Workspace: row.WorktreePath, GitDir: filepath.Join(proj.RootPath, ".git"), Home: home, IcaroDir: icaroDir,
			Extra:   extra,
			Limits:  sandbox.Limits{MemoryBytes: cfg.Limits.MemoryMB << 20, NanoCPUs: int64(cfg.Limits.CPUs * 1e9), Pids: cfg.Limits.Pids, NoFile: cfg.Limits.NoFile},
			Runtime: prof.Runtime, Egress: att,
		}.Build()
		id, err := m.o.Engine.CreateContainerOn(ctx, spec, ns)
		if err != nil {
			return "", fmt.Errorf("create container: %w", err)
		}
		// Persist before starting so a crash here can be reconciled (ADR 0006),
		// and only while the session is still creating: a stop that won
		// the session meanwhile must not get a container back.
		row.ContainerID, row.ImageDigest = id, image
		if ok, err := m.o.Store.UpdateSessionIf(ctx, row, StatusCreating); err != nil || !ok {
			_ = m.o.Engine.RemoveContainer(ctx, id)
			if err == nil {
				err = errStartAborted
			}
			return "", err
		}
		if err := m.o.Engine.StartContainer(ctx, id); err != nil {
			_ = m.o.Engine.RemoveContainer(ctx, id)
			return "", fmt.Errorf("start container: %w", err)
		}
		return id, nil
	}
	cid, err := build()
	if err != nil && m.o.Homes.OverlayEnabled() && strings.Contains(err.Error(), "overlay") && m.o.Homes.Mode != creds.ModeOverlay {
		// Spike S2 fallback: this engine cannot overlay host paths.
		m.o.Log.Warn("overlay homes unavailable; falling back to per-session copies", "err", err)
		_ = m.o.Homes.Release(ctx, row.Profile, row.ID)
		m.o.Homes.DisableOverlay()
		cid, err = build()
	}
	if err != nil {
		return err
	}
	var mem *memServer
	if m.o.Memory != nil {
		sock := MemorySocketPath(m.layout, row.ID)
		stop, err := m.o.Memory.ServeSession(ctx, m.identity(row, proj, agent.Name), sock)
		if err != nil {
			m.o.Log.Warn("memory MCP unavailable for session", "session", row.ID, "err", err)
		} else {
			// The socket is created after the session dir was handed to the
			// container user; a root daemon must hand it over too, or the
			// non-root agent cannot connect (0600).
			m.chownForContainer(sock)
			mem = m.mcp.put(row.ID, stop)
		}
	}
	row.Status, row.Error = StatusRunning, ""
	now := time.Now()
	row.LastHeartbeat = &now
	ok, err := m.o.Store.UpdateSessionIf(ctx, row, StatusCreating)
	if err == nil && ok {
		return nil
	}
	// Stopped or removed meanwhile (or the row cannot be written): no
	// container may run without a running session that supervises it.
	if mem != nil && m.mcp.takeIf(row.ID, mem) {
		mem.stop()
	}
	if rerr := m.o.Engine.RemoveContainer(ctx, cid); rerr != nil {
		m.o.Log.Warn("remove container of an abandoned start", "session", row.ID, "container", short(cid), "err", rerr)
	}
	if err == nil {
		err = errStartAborted
	}
	return err
}

// MemorySocketPath is the host path of a session's memory MCP socket. It
// sits in the session's run directory, which is bind-mounted at /icaro
// (the socket appears only after the container starts, so the directory,
// not the socket file, is what is mounted; ADR 0019).
func MemorySocketPath(l paths.Layout, sessionID string) string {
	return filepath.Join(l.RunDir(), "sessions", sessionID, "mcp.sock")
}

func (m *Manager) identity(row *store.SessionRow, proj *store.Project, agent string) Identity {
	id := Identity{SessionID: row.ID, Branch: row.Branch, Agent: agent}
	if proj != nil {
		id.ProjectID = proj.ID
	}
	return id
}

func (m *Manager) icaroDir(id string) string { return filepath.Dir(MemorySocketPath(m.layout, id)) }

// chownForContainer hands session-owned host paths to the container user
// when the daemon runs as root (a root daemon must not make the agent
// root to write its own worktree). It is a no-op for a normal user daemon.
func (m *Manager) chownForContainer(paths ...string) {
	if os.Getuid() != 0 || m.o.UID == 0 {
		return
	}
	for _, p := range paths {
		_ = filepath.Walk(p, func(path string, _ os.FileInfo, err error) error {
			if err == nil {
				// Lchown never follows links, and this runs before the
				// session container starts, so no agent can race it.
				_ = os.Lchown(path, m.o.UID, m.o.GID) //nolint:gosec // see above
			}
			return nil
		})
	}
}

// Get returns a session.
func (m *Manager) Get(ctx context.Context, id string) (*Session, error) {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return toSession(row), nil
}

// ListFilter narrows List.
type ListFilter struct {
	Project string // project id or name
	Kind    string
	Status  string
}

// List returns sessions oldest first.
func (m *Manager) List(ctx context.Context, f ListFilter) ([]*Session, error) {
	sf := store.SessionFilter{Kind: f.Kind}
	if f.Status != "" {
		sf.Statuses = []string{f.Status}
	}
	if f.Project != "" {
		p, err := m.o.Store.GetProject(ctx, f.Project)
		if errors.Is(err, store.ErrNotFound) {
			return nil, invalid("unknown project %q", f.Project)
		}
		if err != nil {
			return nil, err
		}
		sf.ProjectID = p.ID
	}
	rows, err := m.o.Store.ListSessions(ctx, sf)
	if err != nil {
		return nil, err
	}
	out := make([]*Session, len(rows))
	for i, r := range rows {
		out[i] = toSession(r)
	}
	return out, nil
}

// Projects lists known projects.
func (m *Manager) Projects(ctx context.Context) ([]*store.Project, error) {
	return m.o.Store.ListProjects(ctx)
}

// Ensure returns the newest running agent session of the repository at
// repoPath with profile, creating one when there is none.
func (m *Manager) Ensure(ctx context.Context, req CreateRequest) (*Session, error) {
	profile := req.Profile
	if profile == "" {
		profile = m.o.Config.Sessions.DefaultProfile
	}
	info, err := InspectRepo(ctx, req.RepoPath)
	if err != nil {
		return nil, invalid("%v", err)
	}
	// A path inside one of our worktrees selects that session directly.
	if abs, err := filepath.Abs(req.RepoPath); err == nil && strings.HasPrefix(abs, m.layout.Worktrees()+"/") {
		rows, _ := m.o.Store.ListSessions(ctx, store.SessionFilter{Kind: KindAgent, Statuses: []string{StatusRunning}})
		for _, r := range rows {
			if abs == r.WorktreePath || strings.HasPrefix(abs, r.WorktreePath+"/") {
				return toSession(r), nil
			}
		}
	}
	if p, err := m.o.Store.GetProjectByPath(ctx, info.Root); err == nil {
		rows, err := m.o.Store.ListSessions(ctx, store.SessionFilter{ProjectID: p.ID, Kind: KindAgent, Statuses: []string{StatusRunning}})
		if err != nil {
			return nil, err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Profile == profile && (req.Name == "" || rows[i].Branch == "icaro/"+Slug(req.Name)) {
				return toSession(rows[i]), nil
			}
		}
	}
	req.Profile = profile
	return m.Create(ctx, req)
}

// Stop stops a session's container; its worktree and home layer stay.
func (m *Manager) Stop(ctx context.Context, id string) (*Session, error) {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Claim the session first. A start still in flight writes only while
	// the session is creating, so from here on it notices the stop,
	// removes what it created and returns; wait for that before reading
	// which container is left to stop.
	if ok, err := m.o.Store.TransitionSession(ctx, id, StatusStopped, allStatuses...); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotFound
	}
	if err := m.awaitStart(ctx, id); err != nil {
		return nil, err
	}
	if stop := m.mcp.take(id); stop != nil {
		stop()
	}
	if row, err = m.o.Store.GetSession(ctx, id); errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if row.ContainerID != "" {
		err := m.o.Engine.StopContainer(ctx, row.ContainerID, 10)
		if err == nil {
			err = m.o.Engine.RemoveContainer(ctx, row.ContainerID)
		}
		if err != nil {
			// The container may still run: say so rather than "stopped".
			row.Status, row.Error = StatusError, "stop: "+err.Error()
			_, _ = m.o.Store.UpdateSessionIf(ctx, row, StatusStopped)
			return nil, err
		}
	}
	row.ContainerID = ""
	if _, err := m.o.Store.UpdateSessionIf(ctx, row, StatusStopped); err != nil {
		return nil, err
	}
	m.nudge()
	return toSession(row), nil
}

// Start re-queues a stopped, orphaned or failed session and starts it.
func (m *Manager) Start(ctx context.Context, id string) (*Session, error) {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Kind != KindAgent {
		return nil, invalid("run sessions are started by their workflow run")
	}
	if row.Status == StatusRunning || row.Status == StatusQueued || row.Status == StatusCreating {
		return toSession(row), nil
	}
	if _, err := os.Stat(row.WorktreePath); err != nil {
		return nil, invalid("worktree %s is gone; remove the session", row.WorktreePath)
	}
	if row.ContainerID != "" {
		_ = m.o.Engine.RemoveContainer(ctx, row.ContainerID)
		row.ContainerID = ""
	}
	from := row.Status
	row.Status, row.Error = StatusQueued, ""
	if ok, err := m.o.Store.UpdateSessionIf(ctx, row, from); err != nil {
		return nil, err
	} else if !ok {
		return m.Get(ctx, id) // moved on (or removed) meanwhile; report where it is
	}
	if err := m.requestStart(ctx); err != nil {
		return nil, err
	}
	settled, err := m.waitSettled(ctx, id, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	return toSession(settled), nil
}

// Remove stops a session, releases its home layer and, unless
// keepWorktree, removes its worktree (the branch is kept).
func (m *Manager) Remove(ctx context.Context, id string, keepWorktree bool) error {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if row.Kind == KindRun {
		return m.FinishRun(ctx, id, keepWorktree)
	}
	if _, err := m.Stop(ctx, id); err != nil {
		return err
	}
	if err := m.o.Homes.Release(ctx, row.Profile, row.ID); err != nil {
		return err
	}
	if m.o.Preview != nil && row.WorktreePath != "" {
		if err := m.o.Preview.WorktreeRemoved(ctx, row.ID, row.WorktreePath); err != nil {
			m.o.Log.Warn("preview cleanup", "session", id, "err", err)
		}
	}
	if !keepWorktree && row.WorktreePath != "" {
		if proj, err := m.o.Store.GetProject(ctx, row.ProjectID); err == nil {
			if err := removeWorktree(ctx, proj.RootPath, row.WorktreePath); err != nil {
				return err
			}
		} else {
			_ = os.RemoveAll(row.WorktreePath)
		}
	}
	_ = os.RemoveAll(m.icaroDir(id))
	return m.o.Store.DeleteSession(ctx, id)
}

// ExecInfo tells a CLI how to exec into a session.
type ExecInfo struct {
	SessionID   string   `json:"session_id"`
	ContainerID string   `json:"container_id"`
	User        string   `json:"user"`
	Workdir     string   `json:"workdir"`
	Agent       string   `json:"agent"`
	AgentCmd    []string `json:"agent_cmd,omitempty"`
}

// Exec returns the exec target of a running session.
func (m *Manager) Exec(ctx context.Context, id string) (*ExecInfo, error) {
	row, err := m.o.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Status != StatusRunning || row.ContainerID == "" {
		return nil, invalid("session %s is %s, not running", id, row.Status)
	}
	agent, _ := creds.LookupAgent(m.o.Config.Profiles[row.Profile].Agent)
	return &ExecInfo{SessionID: row.ID, ContainerID: row.ContainerID, User: m.User(), Workdir: container.WorkspacePath, Agent: agent.Name, AgentCmd: agent.Run}, nil
}

// Reconcile probes every non-terminal session after a (re)start: running
// sessions whose container and worktree survive are re-attached, the
// rest are marked orphaned for `icaro doctor` (ADR 0006).
func (m *Manager) Reconcile(ctx context.Context) error {
	rows, err := m.o.Store.ListSessions(ctx, store.SessionFilter{Statuses: []string{StatusRunning, StatusCreating}})
	if err != nil {
		return err
	}
	for _, row := range rows {
		reason := m.liveness(ctx, row)
		if row.Status == StatusCreating && reason == "" && row.Kind == KindAgent {
			reason = "daemon stopped while the session was being created"
		}
		if reason != "" {
			m.o.Log.Warn("session orphaned", "session", row.ID, "reason", reason)
			row.Status, row.Error = StatusOrphaned, reason
			if err := m.o.Store.UpdateSession(ctx, row); err != nil {
				return err
			}
			continue
		}
		m.o.Log.Info("session re-attached", "session", row.ID, "container", row.ContainerID)
		if row.Kind == KindAgent && m.o.Memory != nil {
			proj, _ := m.o.Store.GetProject(ctx, row.ProjectID)
			agent, _ := creds.LookupAgent(m.o.Config.Profiles[row.Profile].Agent)
			sock := filepath.Join(m.icaroDir(row.ID), "mcp.sock")
			if stop, err := m.o.Memory.ServeSession(ctx, m.identity(row, proj, agent.Name), sock); err == nil {
				m.chownForContainer(sock)
				m.mcp.put(row.ID, stop)
			}
		}
		_ = m.o.Store.HeartbeatSession(ctx, row.ID)
	}
	return nil
}

// liveness returns "" when the session's facets exist, else why not.
func (m *Manager) liveness(ctx context.Context, row *store.SessionRow) string {
	if row.WorktreePath != "" {
		if _, err := os.Stat(row.WorktreePath); err != nil {
			return "worktree " + row.WorktreePath + " is gone"
		}
	}
	if row.Kind == KindRun && row.ContainerID == "" {
		return "" // between steps; the workflow engine reconciles the run
	}
	if row.ContainerID == "" {
		if row.Status == StatusCreating {
			return "daemon stopped while the session was being created"
		}
		return "no container recorded"
	}
	st, err := m.o.Engine.Inspect(ctx, row.ContainerID)
	if err != nil {
		return "inspect container: " + err.Error()
	}
	if !st.Exists {
		return "container " + short(row.ContainerID) + " is gone"
	}
	if !st.Running && row.Kind == KindAgent {
		return fmt.Sprintf("container %s exited (code %d)", short(row.ContainerID), st.ExitCode)
	}
	return ""
}

// probe marks running agent sessions whose container died as orphaned.
func (m *Manager) probe(ctx context.Context) {
	rows, err := m.o.Store.ListSessions(ctx, store.SessionFilter{Kind: KindAgent, Statuses: []string{StatusRunning}})
	if err != nil {
		return
	}
	for _, row := range rows {
		if reason := m.liveness(ctx, row); reason != "" {
			if stop := m.mcp.take(row.ID); stop != nil {
				stop()
			}
			row.Status, row.Error = StatusOrphaned, reason
			_ = m.o.Store.UpdateSession(ctx, row)
			continue
		}
		_ = m.o.Store.HeartbeatSession(ctx, row.ID)
	}
}

// Cleanup removes what is left of an orphaned session's container and
// marks it stopped; its worktree (the user's work) is kept.
func (m *Manager) Cleanup(ctx context.Context, id string) error {
	return CleanupOrphan(ctx, m.o.Store, m.o.Engine, id)
}

// CleanupOrphan is Cleanup without a running Manager (used by doctor).
func CleanupOrphan(ctx context.Context, st *store.Store, eng Engine, id string) error {
	row, err := st.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if row.ContainerID != "" {
		if err := eng.RemoveContainer(ctx, row.ContainerID); err != nil {
			return err
		}
	}
	if proj, err := st.GetProject(ctx, row.ProjectID); err == nil {
		_, _ = git(ctx, proj.RootPath, "worktree", "prune")
	}
	row.ContainerID, row.Status = "", StatusStopped
	row.Error = "cleaned up after: " + row.Error
	return st.UpdateSession(ctx, row)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func toSession(r *store.SessionRow) *Session {
	if r == nil {
		return nil
	}
	return &Session{
		ID: r.ID, Kind: r.Kind, ProjectID: r.ProjectID, Profile: r.Profile, Branch: r.Branch, WorktreePath: r.WorktreePath,
		ContainerID: r.ContainerID, PreviewInstanceID: r.PreviewInstanceID, RunID: r.RunID, Status: r.Status, Error: r.Error,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastHeartbeat: r.LastHeartbeat,
	}
}

// owned is a map owned by one goroutine; callers send it operations
// instead of sharing it under a mutex.
type owned[V any] struct {
	ops chan func(map[string]V)
}

func newOwned[V any]() *owned[V] {
	o := &owned[V]{ops: make(chan func(map[string]V))}
	go func() {
		m := map[string]V{}
		for op := range o.ops {
			op(m)
		}
	}()
	return o
}

// do runs fn on the owning goroutine and waits for it.
func (o *owned[V]) do(fn func(map[string]V)) {
	done := make(chan struct{})
	o.ops <- func(m map[string]V) { fn(m); close(done) }
	<-done
}

// memServer is a registered memory MCP server; its pointer identifies
// one registration.
type memServer struct{ stop func() }

// registry holds the per-session memory MCP servers.
type registry struct{ o *owned[*memServer] }

func newRegistry() *registry { return &registry{o: newOwned[*memServer]()} }

// put registers stop for id, stopping the server it replaces.
func (r *registry) put(id string, stop func()) *memServer {
	s := &memServer{stop: stop}
	var old *memServer
	r.o.do(func(m map[string]*memServer) { old, m[id] = m[id], s })
	if old != nil {
		old.stop()
	}
	return s
}

// take unregisters id and returns its stop function (nil if none).
func (r *registry) take(id string) func() {
	var s *memServer
	r.o.do(func(m map[string]*memServer) { s = m[id]; delete(m, id) })
	if s == nil {
		return nil
	}
	return s.stop
}

// takeIf unregisters id only while s is its registration.
func (r *registry) takeIf(id string, s *memServer) bool {
	ok := false
	r.o.do(func(m map[string]*memServer) {
		if m[id] == s {
			delete(m, id)
			ok = true
		}
	})
	return ok
}
