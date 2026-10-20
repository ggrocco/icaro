package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"icaro/internal/paths"
	"icaro/internal/store"
)

// Store is the persistence the manager needs (*store.Store implements it).
type Store interface {
	CreatePreview(ctx context.Context, p *store.PreviewInstance) error
	UpdatePreview(ctx context.Context, p *store.PreviewInstance) error
	GetPreview(ctx context.Context, id string) (*store.PreviewInstance, error)
	GetPreviewByPath(ctx context.Context, path string) (*store.PreviewInstance, error)
	ListPreviews(ctx context.Context) ([]*store.PreviewInstance, error)
	DeletePreview(ctx context.Context, id string) error
	AllowPreviewFinding(ctx context.Context, path, findingID, actor string) error
	PreviewAllowances(ctx context.Context, path string) (map[string]bool, error)
	ClearPreviewAllowances(ctx context.Context, path string) error
}

// Options configure the manager.
type Options struct {
	Layout       paths.Layout
	Domain       string // hostnames are <slug>.<domain>; empty means "localhost"
	HTTPPort     int
	TraefikImage string
	// AllowRoots extend the worktree as mountable roots (config mounts.allow).
	AllowRoots []string
	// MacOS adds polling-watch env to routed services.
	MacOS bool
	// Roots are watched for directories with compose files.
	Roots    []string
	Debounce time.Duration
}

// Manager owns every preview. A single goroutine owns the worktree→worker
// map; each worktree's operations run serially on its own worker
// goroutine, so a slow `compose up` never blocks other previews.
type Manager struct {
	opts    Options
	st      Store
	compose Compose
	engine  Engine
	log     *slog.Logger

	cmds    chan func(*actorState)
	proxy   chan proxyReq
	stopped chan struct{} // closed when Run returns
}

// ErrStopped is returned by calls made after the manager stopped.
var ErrStopped = errors.New("preview manager is not running")

type actorState struct {
	workers  map[string]*worker
	retiring map[string]*worker // workers finishing a Remove
}

type worker struct {
	jobs chan func(ctx context.Context)
	done chan struct{}
}

type proxyReq struct {
	kick  bool
	reply chan error
}

// New builds a manager; call Run to start it.
func New(opts Options, st Store, compose Compose, engine Engine, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if opts.HTTPPort == 0 {
		opts.HTTPPort = 8780
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.TraefikImage == "" {
		opts.TraefikImage = "traefik:v3.1"
	}
	return &Manager{
		opts: opts, st: st, compose: compose, engine: engine, log: log.With("component", "preview"),
		cmds: make(chan func(*actorState)), proxy: make(chan proxyReq), stopped: make(chan struct{}),
	}
}

// Name implements app.Component.
func (m *Manager) Name() string { return "previews" }

// Run serves commands until ctx ends. On start it re-syncs every recorded
// preview: running ones whose configuration is unchanged are re-attached,
// not restarted, and ones whose worktree vanished are cleaned up.
func (m *Manager) Run(ctx context.Context) error {
	defer close(m.stopped)
	go m.proxyLoop(ctx)
	st := &actorState{workers: map[string]*worker{}, retiring: map[string]*worker{}}
	go m.resume(ctx)
	if len(m.opts.Roots) > 0 {
		add := func(ctx context.Context, path string) error {
			_, err := m.Add(ctx, AddRequest{Path: path, Automatic: true})
			return err
		}
		go newWatcher(m.opts.Roots, m.opts.Debounce, add, m.Remove, m.log).run(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			for _, w := range st.workers {
				close(w.jobs)
			}
			return nil
		case fn := <-m.cmds:
			fn(st)
		}
	}
}

func (m *Manager) resume(ctx context.Context) {
	list, err := m.st.ListPreviews(ctx)
	if err != nil {
		m.log.Error("load previews", "err", err)
		return
	}
	for _, p := range list {
		if _, err := os.Stat(p.WorktreePath); errors.Is(err, os.ErrNotExist) {
			if err := m.Remove(ctx, p.WorktreePath); err != nil {
				m.log.Warn("cleanup vanished worktree", "path", p.WorktreePath, "err", err)
			}
			continue
		}
		if p.Status == store.PreviewStopped {
			continue
		}
		if _, err := m.enqueue(ctx, p.WorktreePath, false, func(ctx context.Context) error {
			return m.sync(ctx, p.WorktreePath, false)
		}); err != nil && ctx.Err() == nil {
			m.log.Warn("resume preview", "path", p.WorktreePath, "err", err)
		}
	}
}

// do runs fn on the actor goroutine.
func (m *Manager) do(ctx context.Context, fn func(*actorState)) error {
	done := make(chan struct{})
	select {
	case m.cmds <- func(s *actorState) { fn(s); close(done) }:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.stopped:
		return ErrStopped
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue schedules job on path's worker. With wait it returns the job's
// error; otherwise it returns once queued. retire closes the worker after
// this job (used by Remove).
func (m *Manager) enqueue(ctx context.Context, path string, wait bool, job func(ctx context.Context) error) (<-chan error, error) {
	return m.enqueueRetire(ctx, path, wait, false, job)
}

func (m *Manager) enqueueRetire(ctx context.Context, path string, wait, retire bool, job func(ctx context.Context) error) (<-chan error, error) {
	res := make(chan error, 1)
	wrapped := func(wctx context.Context) { res <- job(wctx) }
	err := m.do(ctx, func(s *actorState) {
		w, ok := s.workers[path]
		if !ok {
			// A worker still finishing a Remove for this path must finish
			// first, so two operations on one worktree never overlap.
			var after <-chan struct{}
			if prev := s.retiring[path]; prev != nil {
				after = prev.done
			}
			w = m.startWorker(ctx, after)
			s.workers[path] = w
		}
		w.jobs <- wrapped
		if retire {
			close(w.jobs)
			delete(s.workers, path)
			s.retiring[path] = w
			go func() {
				<-w.done
				_ = m.do(context.WithoutCancel(ctx), func(s *actorState) {
					if s.retiring[path] == w {
						delete(s.retiring, path)
					}
				})
			}()
		}
	})
	if err != nil {
		return nil, err
	}
	if !wait {
		return res, nil
	}
	select {
	case err := <-res:
		return res, err
	case <-ctx.Done():
		return res, ctx.Err()
	case <-m.stopped:
		return res, ErrStopped
	}
}

func (m *Manager) startWorker(ctx context.Context, after <-chan struct{}) *worker {
	w := &worker{jobs: make(chan func(context.Context), 64), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		if after != nil {
			<-after
		}
		for job := range w.jobs {
			job(context.WithoutCancel(ctx))
		}
	}()
	return w
}

// NormalizePath makes worktree paths comparable: absolute, with symlinks
// resolved in the deepest existing ancestor, so a path normalizes the same
// before and after its directory is deleted.
func NormalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	rest := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// AddRequest registers a worktree.
type AddRequest struct {
	Path      string
	SessionID string
	ProjectID string
	// Wait blocks until the preview is up (or failed).
	Wait bool
	// Force restarts even when the configuration is unchanged.
	Force bool
	// Automatic marks syncs from the watcher: they leave a preview the
	// user stopped with Down alone.
	Automatic bool
}

// Add registers a worktree's preview and brings it up.
func (m *Manager) Add(ctx context.Context, req AddRequest) (*store.PreviewInstance, error) {
	path, err := NormalizePath(req.Path)
	if err != nil {
		return nil, err
	}
	if FindComposeFile(path) == "" {
		return nil, fmt.Errorf("%s has no compose file (%s)", path, strings.Join(ComposeFileNames, ", "))
	}
	inst, err := m.ensureInstance(ctx, path, req.SessionID, req.ProjectID)
	if err != nil {
		return nil, err
	}
	if req.Automatic && inst.Status == store.PreviewStopped {
		return inst, nil
	}
	_, err = m.enqueue(ctx, path, req.Wait, func(ctx context.Context) error { return m.sync(ctx, path, req.Force) })
	if req.Wait {
		if got, gerr := m.st.GetPreviewByPath(ctx, path); gerr == nil {
			inst = got
		}
		if err != nil && inst.Status != store.PreviewBlocked {
			return inst, err
		}
	}
	return inst, nil
}

// ensureInstance returns the row for path, creating it with a
// collision-free hostname. Creation runs on the actor goroutine so two
// worktrees never race for the same hostname.
func (m *Manager) ensureInstance(ctx context.Context, path, sessionID, projectID string) (*store.PreviewInstance, error) {
	var inst *store.PreviewInstance
	var ierr error
	err := m.do(ctx, func(*actorState) {
		p, err := m.st.GetPreviewByPath(ctx, path)
		if err == nil {
			if (sessionID != "" && p.SessionID != sessionID) || (projectID != "" && p.ProjectID != projectID) {
				p.SessionID, p.ProjectID = sessionID, projectID
				ierr = m.st.UpdatePreview(ctx, p)
			}
			inst = p
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			ierr = err
			return
		}
		all, err := m.st.ListPreviews(ctx)
		if err != nil {
			ierr = err
			return
		}
		taken := map[string]bool{}
		for _, o := range all {
			taken[o.Hostname] = true
		}
		n := NameFor(path)
		host := Hostname(n, m.opts.Domain, func(h string) bool { return taken[h] })
		p = &store.PreviewInstance{
			SessionID: sessionID, ProjectID: projectID, WorktreePath: path, ComposeFile: FindComposeFile(path),
			Slug: n.Slug, Hostname: host, ComposeProject: n.Project, Status: store.PreviewPending,
			URL: m.url(host),
		}
		ierr = m.st.CreatePreview(ctx, p)
		inst = p
	})
	if err != nil {
		return nil, err
	}
	return inst, ierr
}

func (m *Manager) url(host string) string { return fmt.Sprintf("http://%s:%d", host, m.opts.HTTPPort) }

// CurlHint is a command that reaches a preview without DNS.
func (m *Manager) CurlHint(p *store.PreviewInstance) string {
	return fmt.Sprintf("curl -H 'Host: %s' http://127.0.0.1:%d/", p.Hostname, m.opts.HTTPPort)
}

func (m *Manager) routeDir() string { return filepath.Join(m.opts.Layout.Previews(), "traefik") }

func (m *Manager) overridePath(project string) string {
	return filepath.Join(m.opts.Layout.Previews(), project, "override.yaml")
}

// loadProject returns the normalized compose model, preferring
// `docker compose config` (which applies .env interpolation) and falling
// back to the raw file only when the docker CLI is missing.
func (m *Manager) loadProject(ctx context.Context, dir, file string) (*Project, error) {
	data, err := m.compose.Config(ctx, dir, []string{file})
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		if data, err = os.ReadFile(file); err != nil {
			return nil, err
		}
	}
	return ParseCompose(data, dir)
}

// LintResult is the outcome of linting one worktree.
type LintResult struct {
	Path        string    `json:"path"`
	ComposeFile string    `json:"compose_file"`
	Findings    []Finding `json:"findings"`
	Blocked     []Finding `json:"blocked"`
}

// Lint lints a worktree's compose file against the mount policy and the
// worktree's recorded allowances.
func (m *Manager) Lint(ctx context.Context, path string) (*LintResult, error) {
	path, err := NormalizePath(path)
	if err != nil {
		return nil, err
	}
	file := FindComposeFile(path)
	if file == "" {
		return nil, fmt.Errorf("%s has no compose file", path)
	}
	p, err := m.loadProject(ctx, path, file)
	if err != nil {
		return nil, err
	}
	allowed, err := m.st.PreviewAllowances(ctx, path)
	if err != nil {
		return nil, err
	}
	findings := Lint(p, LintOptions{AllowedRoots: append([]string{path}, m.opts.AllowRoots...)})
	return &LintResult{Path: path, ComposeFile: file, Findings: orEmpty(findings), Blocked: orEmpty(Blocked(findings, allowed))}, nil
}

func orEmpty(f []Finding) []Finding {
	if f == nil {
		return []Finding{}
	}
	return f
}

// Allow records the user's explicit override of findings for a worktree
// and re-syncs its preview.
func (m *Manager) Allow(ctx context.Context, path string, findingIDs []string, actor string) error {
	path, err := NormalizePath(path)
	if err != nil {
		return err
	}
	res, err := m.Lint(ctx, path)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, f := range res.Findings {
		known[f.ID] = true
	}
	for _, id := range findingIDs {
		if !known[id] {
			return fmt.Errorf("finding %q is not reported for %s", id, path)
		}
	}
	for _, id := range findingIDs {
		if err := m.st.AllowPreviewFinding(ctx, path, id, actor); err != nil {
			return err
		}
	}
	m.log.Warn("compose lint finding allowed by user", "path", path, "findings", findingIDs, "actor", actor)
	if _, err := m.st.GetPreviewByPath(ctx, path); err == nil {
		_, err = m.enqueue(ctx, path, false, func(ctx context.Context) error { return m.sync(ctx, path, true) })
		return err
	}
	return nil
}

// Down stops a preview but keeps its record.
func (m *Manager) Down(ctx context.Context, path string) (*store.PreviewInstance, error) {
	path, err := NormalizePath(path)
	if err != nil {
		return nil, err
	}
	_, err = m.enqueue(ctx, path, true, func(ctx context.Context) error {
		p, err := m.st.GetPreviewByPath(ctx, path)
		if err != nil {
			return err
		}
		if err := m.stop(ctx, p); err != nil {
			return err
		}
		p.Status, p.Error = store.PreviewStopped, ""
		return m.st.UpdatePreview(ctx, p)
	})
	if err != nil {
		return nil, err
	}
	return m.st.GetPreviewByPath(ctx, path)
}

// Remove tears a preview down and forgets it. It is idempotent and works
// after the worktree is gone: the compose project name derives from the
// path alone.
func (m *Manager) Remove(ctx context.Context, path string) error {
	norm, err := NormalizePath(path)
	if err != nil {
		return err
	}
	_, err = m.enqueueRetire(ctx, norm, true, true, func(ctx context.Context) error {
		p, err := m.st.GetPreviewByPath(ctx, norm)
		if errors.Is(err, store.ErrNotFound) {
			p = &store.PreviewInstance{WorktreePath: norm, ComposeProject: NameFor(norm).Project}
		} else if err != nil {
			return err
		}
		if err := m.stop(ctx, p); err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Dir(m.overridePath(p.ComposeProject)))
		if err := m.st.ClearPreviewAllowances(ctx, norm); err != nil {
			return err
		}
		if p.ID != "" {
			return m.st.DeletePreview(ctx, p.ID)
		}
		return nil
	})
	return err
}

// stop runs compose down and drops the route.
func (m *Manager) stop(ctx context.Context, p *store.PreviewInstance) error {
	if err := removeRoute(m.routeDir(), p.ComposeProject); err != nil {
		return err
	}
	return m.compose.Down(ctx, p.ComposeProject)
}

// sync converges one worktree's preview. It restarts the stack only when
// the configuration (compose files, .env, lockfiles) changed or force.
func (m *Manager) sync(ctx context.Context, path string, force bool) error {
	p, err := m.st.GetPreviewByPath(ctx, path)
	if err != nil {
		return err
	}
	fail := func(status string, err error) error {
		p.Status, p.Error = status, err.Error()
		if uerr := m.st.UpdatePreview(ctx, p); uerr != nil {
			m.log.Error("persist preview", "err", uerr)
		}
		m.log.Warn("preview not running", "path", path, "status", status, "err", err)
		return err
	}
	file := FindComposeFile(path)
	if file == "" {
		_ = m.stop(ctx, p)
		return fail(store.PreviewStopped, errors.New("no compose file in worktree"))
	}
	hash, err := ConfigHash(path)
	if err != nil {
		return fail(store.PreviewError, err)
	}
	if !force && p.Status == store.PreviewRunning && p.ConfigHash == hash {
		if n, err := m.compose.Running(ctx, p.ComposeProject); err == nil && n > 0 {
			// Unchanged and alive: re-attached, not restarted. The proxy may
			// still be gone (removed, or renamed by an upgrade), and routing
			// needs it, so ensure it (idempotent).
			if err := m.proxyDo(ctx, false); err != nil {
				return fail(store.PreviewError, fmt.Errorf("preview proxy: %w", err))
			}
			return nil
		}
	}
	proj, err := m.loadProject(ctx, path, file)
	if err != nil {
		return fail(store.PreviewError, err)
	}
	allowed, err := m.st.PreviewAllowances(ctx, path)
	if err != nil {
		return fail(store.PreviewError, err)
	}
	if blocked := Blocked(Lint(proj, LintOptions{AllowedRoots: append([]string{path}, m.opts.AllowRoots...)}), allowed); len(blocked) > 0 {
		_ = m.stop(ctx, p)
		msgs := make([]string, len(blocked))
		for i, f := range blocked {
			msgs[i] = f.String()
		}
		p.ConfigHash = hash
		return fail(store.PreviewBlocked, fmt.Errorf("compose file blocked by lint: %s", strings.Join(msgs, "; ")))
	}
	route, err := SelectRoute(proj)
	if err != nil {
		return fail(store.PreviewError, err)
	}
	n := NameFor(path)
	n.Project = p.ComposeProject
	override, err := Override(proj, route, n, OverrideOptions{InstanceID: p.ID, Hostname: p.Hostname, MacOS: m.opts.MacOS})
	if err != nil {
		return fail(store.PreviewError, err)
	}
	op := m.overridePath(p.ComposeProject)
	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		return fail(store.PreviewError, err)
	}
	if err := os.WriteFile(op, override, 0o600); err != nil {
		return fail(store.PreviewError, err)
	}
	if err := m.proxyDo(ctx, false); err != nil {
		return fail(store.PreviewError, fmt.Errorf("preview proxy: %w", err))
	}
	if err := m.compose.Up(ctx, p.ComposeProject, path, []string{file, op}); err != nil {
		return fail(store.PreviewError, err)
	}
	rc, err := TraefikConfig(n, p.Hostname, route)
	if err != nil {
		return fail(store.PreviewError, err)
	}
	if err := writeRoute(m.routeDir(), p.ComposeProject, rc); err != nil {
		return fail(store.PreviewError, err)
	}
	if m.opts.MacOS {
		// Bind-mount file events are unreliable on Docker Desktop; make
		// the proxy re-read its routes.
		_ = m.proxyDo(ctx, true)
	}
	p.ComposeFile, p.Service, p.Port, p.ConfigHash = file, route.Service, route.Port, hash
	p.Status, p.Error, p.URL = store.PreviewRunning, "", m.url(p.Hostname)
	if err := m.st.UpdatePreview(ctx, p); err != nil {
		return err
	}
	m.log.Info("preview up", "path", path, "url", p.URL, "service", route.Service, "port", route.Port)
	return nil
}

// proxyLoop serializes proxy operations on one goroutine.
func (m *Manager) proxyLoop(ctx context.Context) {
	opts := ProxyOptions{Name: ProxyName(m.opts.Layout.Root), Image: m.opts.TraefikImage, HTTPPort: m.opts.HTTPPort, DynamicDir: m.routeDir(), MacOS: m.opts.MacOS}
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-m.proxy:
			var err error
			if req.kick {
				err = m.engine.Restart(context.WithoutCancel(ctx), opts.Name)
			} else {
				err = EnsureProxy(context.WithoutCancel(ctx), m.engine, opts)
			}
			req.reply <- err
		}
	}
}

func (m *Manager) proxyDo(ctx context.Context, kick bool) error {
	reply := make(chan error, 1)
	select {
	case m.proxy <- proxyReq{kick: kick, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.stopped:
		return ErrStopped
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// List returns every preview.
func (m *Manager) List(ctx context.Context) ([]*store.PreviewInstance, error) {
	return m.st.ListPreviews(ctx)
}

// Get returns one preview by id or worktree path.
func (m *Manager) Get(ctx context.Context, idOrPath string) (*store.PreviewInstance, error) {
	if strings.Contains(idOrPath, string(filepath.Separator)) {
		path, err := NormalizePath(idOrPath)
		if err != nil {
			return nil, err
		}
		return m.st.GetPreviewByPath(ctx, path)
	}
	return m.st.GetPreview(ctx, idOrPath)
}

// Logs writes the tail of a preview's compose logs.
func (m *Manager) Logs(ctx context.Context, idOrPath string, tail int, w io.Writer) error {
	p, err := m.Get(ctx, idOrPath)
	if err != nil {
		return err
	}
	return m.compose.Logs(ctx, p.ComposeProject, tail, w)
}

// WorktreeAdded implements session.PreviewHook: a session's worktree with
// a compose file gets a preview whose build context is that same
// worktree, bind-mounted rather than copied, so the agent's edits reach
// the dev server. It returns "" when the worktree has no compose file.
func (m *Manager) WorktreeAdded(ctx context.Context, sessionID, projectID, worktreePath string) (string, error) {
	path, err := NormalizePath(worktreePath)
	if err != nil {
		return "", err
	}
	if FindComposeFile(path) == "" {
		return "", nil
	}
	inst, err := m.Add(ctx, AddRequest{Path: path, SessionID: sessionID, ProjectID: projectID})
	if err != nil {
		return "", err
	}
	return inst.ID, nil
}

// WorktreeRemoved implements session.PreviewHook.
func (m *Manager) WorktreeRemoved(ctx context.Context, _ string, worktreePath string) error {
	return m.Remove(ctx, worktreePath)
}
