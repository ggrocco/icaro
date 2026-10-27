package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GitRunner runs git in dir and returns trimmed stdout. It is the edge
// that tests replace (or point at local file:// repositories).
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// ExecGit runs the git CLI.
func ExecGit(ctx context.Context, dir string, args ...string) (string, error) {
	// A pack is a third-party repository: its config never runs a program.
	args = append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary, args from callers
	if dir != "" {
		cmd.Dir = dir
	}
	// Never prompt for credentials from the daemon.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

var packNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Packs manages pack directories under the integrations directory.
type Packs struct {
	Dir string
	Git GitRunner
}

// NewPacks returns a pack manager.
func NewPacks(dir string, git GitRunner) *Packs {
	if git == nil {
		git = ExecGit
	}
	return &Packs{Dir: dir, Git: git}
}

// Summary is the complete permission summary of a pack: complete because
// manifests execute no code.
type Summary struct {
	Pack         string               `json:"pack"`
	Integrations []IntegrationSummary `json:"integrations"`
	Errors       []LoadError          `json:"errors,omitempty"`
}

// IntegrationSummary lists what one manifest can do.
type IntegrationSummary struct {
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Hosts          []string `json:"hosts"`
	Auth           []string `json:"auth"` // "<ref>: <kind> (credential <integration>.<cred>)"
	Verify         string   `json:"verify,omitempty"`
	Events         []string `json:"events"`
	Actions        []string `json:"actions"`
	ShadowsBuiltin bool     `json:"shadows_builtin,omitempty"`
}

// Summarize describes a manifest.
func Summarize(m *Manifest) IntegrationSummary {
	s := IntegrationSummary{Name: m.Name, Description: m.Description, Hosts: append([]string{}, m.Hosts...), Actions: sortedKeys(m.Actions), Events: []string{}, Auth: []string{}}
	for _, k := range sortedKeys(m.Auth) {
		a := m.Auth[k]
		desc := fmt.Sprintf("%s: %s", k, a.Kind)
		if a.Credential != "" {
			desc += fmt.Sprintf(" (credential %s.%s)", m.Name, a.Credential)
		}
		s.Auth = append(s.Auth, desc)
	}
	if m.Triggers != nil {
		s.Verify = m.Triggers.Verify.Kind
		s.Events = sortedKeys(m.Triggers.Events)
	}
	if s.Actions == nil {
		s.Actions = []string{}
	}
	if base, ok := m.Config["base_url"]; ok && base.Default == "" {
		s.Hosts = append(s.Hosts, "(user-configured base_url)")
	}
	return s
}

// SummarizeDir builds the permission summary of a pack directory.
func SummarizeDir(pack, dir string) Summary {
	builtins := map[string]bool{}
	for _, n := range BuiltinNames() {
		builtins[n] = true
	}
	sum := Summary{Pack: pack}
	for _, rel := range manifestFiles(dir) {
		src, err := os.ReadFile(filepath.Join(dir, rel))
		var m *Manifest
		var issues Issues
		if err == nil {
			m, issues, err = Parse(src)
		}
		if err != nil || len(issues) > 0 {
			sum.Errors = append(sum.Errors, LoadError{Pack: pack, File: rel, Error: errText(err, issues), Issues: issues})
			continue
		}
		is := Summarize(m)
		is.ShadowsBuiltin = builtins[m.Name]
		sum.Integrations = append(sum.Integrations, is)
	}
	return sum
}

// Format renders a summary for a confirmation prompt.
func (s Summary) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pack %s\n", s.Pack)
	if len(s.Integrations) == 0 {
		b.WriteString("  (no valid manifests)\n")
	}
	for _, in := range s.Integrations {
		fmt.Fprintf(&b, "  integration %s", in.Name)
		if in.ShadowsBuiltin {
			b.WriteString("  [shadows the builtin]")
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "    hosts:   %s\n", orNone(in.Hosts))
		fmt.Fprintf(&b, "    auth:    %s\n", orNone(in.Auth))
		if in.Verify != "" {
			fmt.Fprintf(&b, "    verify:  %s\n", in.Verify)
		}
		fmt.Fprintf(&b, "    events:  %s\n", orNone(in.Events))
		fmt.Fprintf(&b, "    actions: %s\n", orNone(in.Actions))
	}
	for _, e := range s.Errors {
		fmt.Fprintf(&b, "  INVALID %s: %s\n", e.File, e.Error)
	}
	return b.String()
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

// Staged is a cloned pack waiting for confirmation.
type Staged struct {
	Name    string
	URL     string
	dir     string
	Summary Summary
}

// PackNameFromURL derives a pack name from a git URL.
func PackNameFromURL(url string) string {
	base := path.Base(strings.TrimSuffix(strings.TrimRight(url, "/"), ".git"))
	if i := strings.LastIndex(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	return strings.ToLower(base)
}

// Stage clones url into a staging directory and summarizes it. Nothing is
// active until Activate.
func (p *Packs) Stage(ctx context.Context, url, name string) (*Staged, error) {
	if name == "" {
		name = PackNameFromURL(url)
	}
	if !packNameRe.MatchString(name) || name == PackBuiltin || name == PackLocal {
		return nil, fmt.Errorf("invalid or reserved pack name %q (use --name)", name)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, name)); err == nil {
		return nil, fmt.Errorf("pack %q already exists; use `icaro integration update %s`", name, name)
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(p.Dir, ".staging-"+name+"-")
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(stage, "pack")
	if _, err := p.Git(ctx, "", "clone", "--quiet", "--", url, dest); err != nil {
		_ = os.RemoveAll(stage)
		return nil, err
	}
	return &Staged{Name: name, URL: url, dir: dest, Summary: SummarizeDir(name, dest)}, nil
}

// Activate moves a staged pack into place.
func (p *Packs) Activate(s *Staged) error {
	defer func() { _ = os.RemoveAll(filepath.Dir(s.dir)) }()
	return os.Rename(s.dir, filepath.Join(p.Dir, s.Name))
}

// Discard drops a staged pack.
func (p *Packs) Discard(s *Staged) error { return os.RemoveAll(filepath.Dir(s.dir)) }

// Update pulls a pack (fast-forward only) and returns its new summary.
func (p *Packs) Update(ctx context.Context, name string) (Summary, error) {
	dir, err := p.userPack(name)
	if err != nil {
		return Summary{}, err
	}
	if _, err := p.Git(ctx, dir, "pull", "--ff-only", "--quiet"); err != nil {
		return Summary{}, err
	}
	return SummarizeDir(name, dir), nil
}

// Push pushes a pack's commits to its remote.
func (p *Packs) Push(ctx context.Context, name string) error {
	dir, err := p.userPack(name)
	if err != nil {
		return err
	}
	_, err = p.Git(ctx, dir, "push", "--quiet")
	return err
}

// Remove deletes a user pack directory.
func (p *Packs) Remove(name string) error {
	dir, err := p.userPack(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (p *Packs) userPack(name string) (string, error) {
	if name == PackBuiltin {
		return "", errors.New("the builtin pack is read-only and embedded in the binary")
	}
	if !packNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid pack name %q", name)
	}
	dir := filepath.Join(p.Dir, name)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("pack %q is not installed", name)
	}
	return dir, nil
}

// List returns installed user pack names.
func (p *Packs) List() []string {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// EnsureLocal initializes the local pack as a git repository.
func (p *Packs) EnsureLocal(ctx context.Context) (string, error) {
	dir := filepath.Join(p.Dir, PackLocal)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if _, err := p.Git(ctx, dir, "init", "--quiet"); err != nil {
		return "", err
	}
	readme := "# local integration pack\n\nManifests edited through Ícaro are committed here. Push is manual: `icaro integration push local`.\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600); err != nil {
		return "", err
	}
	if _, err := p.Git(ctx, dir, "add", "README.md"); err != nil {
		return "", err
	}
	if err := p.commit(ctx, dir, "icaro: initialize local pack"); err != nil {
		return "", err
	}
	return dir, nil
}

// ValidationError is returned when an edit is rejected before any write.
type ValidationError struct{ Issues Issues }

func (e *ValidationError) Error() string { return "manifest is invalid: " + e.Issues.Error() }

// EditManifest validates src and, only if it is valid, writes it to the
// local pack as <name>.yaml and commits it with a structured message. It
// returns the commit hash ("" when the content did not change).
func (p *Packs) EditManifest(ctx context.Context, name string, src []byte, actor string) (string, error) {
	m, issues, err := Parse(src)
	if err != nil {
		return "", &ValidationError{Issues: Issues{{Path: "/", Message: err.Error()}}}
	}
	if len(issues) > 0 {
		return "", &ValidationError{Issues: issues}
	}
	if m.Name != name {
		return "", &ValidationError{Issues: Issues{{Path: "/name", Message: fmt.Sprintf("manifest name %q does not match %q", m.Name, name)}}}
	}
	dir, err := p.EnsureLocal(ctx)
	if err != nil {
		return "", err
	}
	file := name + ".yaml"
	target := filepath.Join(dir, file)
	verb := "add"
	if old, err := os.ReadFile(target); err == nil {
		if bytes.Equal(old, src) {
			return "", nil
		}
		verb = "update"
	}
	if err := os.WriteFile(target, src, 0o600); err != nil {
		return "", err
	}
	if _, err := p.Git(ctx, dir, "add", "--", file); err != nil {
		return "", err
	}
	if actor == "" {
		actor = "icaro"
	}
	sum := Summarize(m)
	msg := fmt.Sprintf("icaro: %s %s manifest\n\nEdited-By: %s\nHosts: %s\nEvents: %s\nActions: %s\n",
		verb, name, actor, orNone(sum.Hosts), orNone(sum.Events), orNone(sum.Actions))
	if err := p.commit(ctx, dir, msg); err != nil {
		return "", err
	}
	return p.Git(ctx, dir, "rev-parse", "HEAD")
}

// commit commits staged changes, supplying an identity only when git has
// none configured.
func (p *Packs) commit(ctx context.Context, dir, msg string) error {
	args := []string{"commit", "--quiet", "-m", msg}
	if email, _ := p.Git(ctx, dir, "config", "user.email"); email == "" {
		args = append([]string{"-c", "user.name=icaro", "-c", "user.email=icaro@localhost"}, args...)
	}
	_, err := p.Git(ctx, dir, args...)
	return err
}
