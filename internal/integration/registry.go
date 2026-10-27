package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"icaro/internal/workflow"
)

// Loaded is one manifest as served.
type Loaded struct {
	Manifest *Manifest `json:"-"`
	Pack     string    `json:"pack"`
	File     string    `json:"file"`
	Source   string    `json:"-"`
	// Shadows is set when this user manifest replaces a builtin.
	Shadows bool `json:"shadows_builtin,omitempty"`
}

// LoadError is a manifest that failed to load.
type LoadError struct {
	Pack   string `json:"pack"`
	File   string `json:"file"`
	Error  string `json:"error"`
	Issues Issues `json:"issues,omitempty"`
	// ServingLastGood is true when an earlier valid version stays loaded.
	ServingLastGood bool `json:"serving_last_good"`
}

// PackStatus describes one pack.
type PackStatus struct {
	Name         string      `json:"name"`
	Dir          string      `json:"dir,omitempty"`
	ReadOnly     bool        `json:"read_only"`
	Remote       string      `json:"remote,omitempty"`
	Head         string      `json:"head,omitempty"`
	Integrations []string    `json:"integrations"`
	Errors       []LoadError `json:"errors,omitempty"`
}

// Snapshot is an immutable view of every loaded integration. The
// registry's owner goroutine builds a new one per reload; readers never
// lock.
type Snapshot struct {
	byName   map[string]*Loaded
	Packs    []PackStatus `json:"packs"`
	Warnings []string     `json:"warnings,omitempty"`
	LoadedAt time.Time    `json:"loaded_at"`
}

// Get returns an integration by name.
func (s *Snapshot) Get(name string) (*Loaded, bool) {
	l, ok := s.byName[name]
	return l, ok
}

// Names lists loaded integrations.
func (s *Snapshot) Names() []string { return sortedKeys(s.byName) }

// Errors lists every load error across packs.
func (s *Snapshot) Errors() []LoadError {
	var out []LoadError
	for _, p := range s.Packs {
		out = append(out, p.Errors...)
	}
	return out
}

// Registry loads packs from the integrations directory. Load state is
// owned by whichever goroutine runs Run (or by the caller of Load before
// Run starts); readers use Snapshot.
type Registry struct {
	dir     string
	git     GitRunner
	snap    atomic.Pointer[Snapshot]
	reload  chan chan error
	running atomic.Bool

	// Owned by the loading goroutine.
	lastGood map[string]*Loaded // pack/file -> last valid version
	print    string
}

// NewRegistry creates a registry over dir (~/.icaro/integrations) and
// performs the first load.
func NewRegistry(dir string, git GitRunner) *Registry {
	if git == nil {
		git = ExecGit
	}
	r := &Registry{dir: dir, git: git, reload: make(chan chan error), lastGood: map[string]*Loaded{}}
	r.load(context.Background())
	return r
}

// Dir is the integrations directory.
func (r *Registry) Dir() string { return r.dir }

// Snapshot returns the current view.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

// Run polls the packs for changes every interval and serves Reload
// requests until ctx ends.
func (r *Registry) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	r.running.Store(true)
	defer r.running.Store(false)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case done := <-r.reload:
			r.load(ctx)
			done <- nil
		case <-t.C:
			if fp := r.fingerprint(); fp != r.print {
				r.load(ctx)
			}
		}
	}
}

// Reload forces a reload and waits for it.
func (r *Registry) Reload(ctx context.Context) error {
	if !r.running.Load() {
		r.load(ctx)
		return nil
	}
	done := make(chan error, 1)
	select {
	case r.reload <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type candidate struct {
	loaded *Loaded
}

func (r *Registry) load(ctx context.Context) {
	r.print = r.fingerprint()
	snap := &Snapshot{byName: map[string]*Loaded{}, LoadedAt: time.Now()}

	builtin := PackStatus{Name: PackBuiltin, ReadOnly: true}
	files := builtinFiles()
	for _, f := range sortedKeys(files) {
		m, issues, err := Parse(files[f])
		if err != nil || len(issues) > 0 {
			// Builtins are tested; this only happens in development.
			builtin.Errors = append(builtin.Errors, LoadError{Pack: PackBuiltin, File: f, Error: errText(err, issues), Issues: issues})
			continue
		}
		snap.byName[m.Name] = &Loaded{Manifest: m, Pack: PackBuiltin, File: f, Source: string(files[f])}
		builtin.Integrations = append(builtin.Integrations, m.Name)
	}
	snap.Packs = append(snap.Packs, builtin)

	// User packs, in name order.
	user := map[string][]candidate{}
	var statuses []PackStatus
	for _, pack := range r.packDirs() {
		st := PackStatus{Name: pack, Dir: filepath.Join(r.dir, pack)}
		st.Remote, _ = r.git(ctx, st.Dir, "remote", "get-url", "origin")
		st.Head, _ = r.git(ctx, st.Dir, "rev-parse", "--short", "HEAD")
		for _, rel := range manifestFiles(st.Dir) {
			key := pack + "/" + rel
			src, err := os.ReadFile(filepath.Join(st.Dir, rel))
			var m *Manifest
			var issues Issues
			if err == nil {
				m, issues, err = Parse(src)
			}
			wantName := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(rel), ".yaml"), ".yml")
			if err == nil && len(issues) == 0 && m.Name != wantName {
				issues = Issues{{Path: "/name", Message: fmt.Sprintf("manifest name %q must match its file name %q", m.Name, wantName)}}
			}
			if err != nil || len(issues) > 0 {
				le := LoadError{Pack: pack, File: rel, Error: errText(err, issues), Issues: issues}
				if lg, ok := r.lastGood[key]; ok {
					le.ServingLastGood = true
					user[lg.Manifest.Name] = append(user[lg.Manifest.Name], candidate{loaded: lg})
				}
				st.Errors = append(st.Errors, le)
				continue
			}
			l := &Loaded{Manifest: m, Pack: pack, File: rel, Source: string(src)}
			r.lastGood[key] = l
			user[m.Name] = append(user[m.Name], candidate{loaded: l})
		}
		statuses = append(statuses, st)
	}
	for _, name := range sortedKeys(user) {
		cands := user[name]
		if len(cands) > 1 {
			var packs []string
			for _, c := range cands {
				packs = append(packs, c.loaded.Pack)
			}
			msg := fmt.Sprintf("integration %q is defined by more than one user pack (%s); none of them is loaded", name, strings.Join(packs, ", "))
			for i := range statuses {
				for _, c := range cands {
					if statuses[i].Name == c.loaded.Pack {
						statuses[i].Errors = append(statuses[i].Errors, LoadError{Pack: c.loaded.Pack, File: c.loaded.File, Error: msg})
					}
				}
			}
			continue
		}
		l := cands[0].loaded
		if prev, ok := snap.byName[name]; ok && prev.Pack == PackBuiltin {
			cp := *l
			cp.Shadows = true
			l = &cp
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("pack %s shadows the builtin %s integration", l.Pack, name))
		}
		snap.byName[name] = l
		for i := range statuses {
			if statuses[i].Name == l.Pack {
				statuses[i].Integrations = append(statuses[i].Integrations, name)
			}
		}
	}
	snap.Packs = append(snap.Packs, statuses...)
	r.snap.Store(snap)
}

func errText(err error, issues Issues) string {
	if err != nil {
		return err.Error()
	}
	return issues.Error()
}

// packDirs lists user pack directories.
func (r *Registry) packDirs() []string {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != PackBuiltin {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// manifestFiles lists manifest paths (relative) at the pack root and
// under manifests/. Only regular files count, and manifests/ must be a
// real directory: a symlink there could pull in files from outside the
// pack, which the permission summary never showed.
func manifestFiles(dir string) []string {
	var out []string
	for _, sub := range []string{"", "manifests"} {
		if sub != "" {
			if fi, err := os.Lstat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
				continue
			}
		}
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() && isManifestFile(e.Name()) {
				out = append(out, filepath.Join(sub, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// fingerprint hashes the manifest files' names, sizes and mtimes.
func (r *Registry) fingerprint() string {
	h := sha256.New()
	for _, pack := range r.packDirs() {
		dir := filepath.Join(r.dir, pack)
		for _, rel := range manifestFiles(dir) {
			fi, err := os.Stat(filepath.Join(dir, rel))
			if err != nil {
				continue
			}
			fmt.Fprintf(h, "%s/%s:%d:%d\n", pack, rel, fi.Size(), fi.ModTime().UnixNano())
		}
		if b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- workflow.Catalog ---

// Action resolves "<integration>.<action>".
func (s *Snapshot) Action(ref string) (workflow.ActionInfo, bool) {
	integ, act, ok := strings.Cut(ref, ".")
	if !ok {
		return workflow.ActionInfo{}, false
	}
	l, ok := s.byName[integ]
	if !ok {
		return workflow.ActionInfo{}, false
	}
	a, ok := l.Manifest.Actions[act]
	if !ok {
		return workflow.ActionInfo{}, false
	}
	return actionInfo(l.Manifest, act, a), true
}

func actionInfo(m *Manifest, name string, a Action) workflow.ActionInfo {
	info := workflow.ActionInfo{Ref: m.Name + "." + name, Description: a.Description, Inputs: map[string]workflow.InputInfo{}, Outputs: sortedKeys(a.Outputs), Hosts: append([]string{}, m.Hosts...)}
	for k, in := range a.Inputs {
		info.Inputs[k] = workflow.InputInfo{Type: in.Type, Required: in.Required, Description: in.Description, Default: in.Default}
	}
	return info
}

// Event resolves an integration trigger event.
func (s *Snapshot) Event(integration, event string) (workflow.EventInfo, bool) {
	l, ok := s.byName[integration]
	if !ok || l.Manifest.Triggers == nil {
		return workflow.EventInfo{}, false
	}
	ev, ok := l.Manifest.Triggers.Events[event]
	if !ok {
		return workflow.EventInfo{}, false
	}
	return workflow.EventInfo{Integration: integration, Event: event, Description: ev.Description, Fields: sortedKeys(ev.Payload)}, true
}

// ActionRefs lists every action reference.
func (s *Snapshot) ActionRefs() []string {
	var out []string
	for _, name := range s.Names() {
		for _, a := range sortedKeys(s.byName[name].Manifest.Actions) {
			out = append(out, name+"."+a)
		}
	}
	return out
}

// EventNames lists an integration's events.
func (s *Snapshot) EventNames(integration string) []string {
	l, ok := s.byName[integration]
	if !ok || l.Manifest.Triggers == nil {
		return nil
	}
	return sortedKeys(l.Manifest.Triggers.Events)
}

// Integrations lists loaded integrations.
func (s *Snapshot) Integrations() []string { return s.Names() }

// Catalog adapts a registry to workflow.Catalog, always reading the
// latest snapshot.
type Catalog struct{ R *Registry }

func (c Catalog) Action(ref string) (workflow.ActionInfo, bool) { return c.R.Snapshot().Action(ref) }
func (c Catalog) Event(i, e string) (workflow.EventInfo, bool) {
	return c.R.Snapshot().Event(i, e)
}
func (c Catalog) ActionRefs() []string         { return c.R.Snapshot().ActionRefs() }
func (c Catalog) EventNames(i string) []string { return c.R.Snapshot().EventNames(i) }
func (c Catalog) Integrations() []string       { return c.R.Snapshot().Integrations() }

var _ workflow.Catalog = Catalog{}
var _ workflow.Catalog = (*Snapshot)(nil)

// ActionInputSchema compiles an action's typed inputs into a JSON Schema
// (an MCP resource, so agents know required inputs before writing YAML).
func (s *Snapshot) ActionInputSchema(ref string) (map[string]any, bool) {
	info, ok := s.Action(ref)
	if !ok {
		return nil, false
	}
	props := map[string]any{}
	var required []string
	for _, k := range sortedKeys(info.Inputs) {
		in := info.Inputs[k]
		p := map[string]any{"type": in.Type}
		if in.Description != "" {
			p["description"] = in.Description
		}
		if in.Default != nil {
			p["default"] = in.Default
		}
		props[k] = map[string]any{"anyOf": []any{p, map[string]any{"type": "string", "pattern": `^\$\{\{.*\}\}$`, "description": "an interpolated value"}}}
		if in.Required {
			required = append(required, k)
		}
	}
	schema := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://icaro.dev/schemas/actions/" + ref + ".json",
		"title":                ref + " input",
		"description":          info.Description,
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"x-icaro-outputs":      info.Outputs,
		"x-icaro-hosts":        info.Hosts,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, true
}
