package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Network is the dedicated external network every routed service joins.
const Network = "icaro-preview"

// PortLabel declares which service and port a preview routes to.
const PortLabel = "icaro.preview.port"

// Naming is the collision-free identity of one worktree's preview.
type Naming struct {
	Slug    string // human part, from the path
	Hash    string // short hash of the absolute path
	Project string // compose project name: icaro-<slug>-<hash>
	Alias   string // network alias of the routed service
}

// NameFor derives the naming for an absolute worktree path. It depends
// only on the path, so cleanup can recompute it without any state.
func NameFor(path string) Naming {
	abs := filepath.Clean(path)
	sum := sha256.Sum256([]byte(abs))
	h := hex.EncodeToString(sum[:])[:8]
	slug := Slugify(filepath.Base(abs))
	return Naming{Slug: slug, Hash: h, Project: "icaro-" + slug + "-" + h, Alias: "icaro-" + slug + "-" + h + "-web"}
}

// Slugify keeps [a-z0-9-], collapses the rest to '-', and caps the length
// so hostnames stay under the DNS label limit.
func Slugify(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			dash = false
		} else if !dash && sb.Len() > 0 {
			sb.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(sb.String(), "-")
	if len(out) > 30 {
		out = strings.Trim(out[:30], "-")
	}
	if out == "" {
		out = "preview"
	}
	return out
}

// Hostname picks slug.<domain> unless another worktree already holds it,
// in which case the path hash disambiguates. taken reports hostnames in use
// by other worktrees.
func Hostname(n Naming, domain string, taken func(string) bool) string {
	if domain == "" {
		domain = "localhost"
	}
	h := n.Slug + "." + domain
	if taken != nil && taken(h) {
		h = n.Slug + "-" + n.Hash + "." + domain
	}
	return h
}

// Route is the service and port a preview sends traffic to.
type Route struct {
	Service string
	Port    int
}

// ErrNoRoute means no service could be chosen without guessing.
var ErrNoRoute = errors.New("no routable service")

// SelectRoute applies the port-discovery rule: the service labelled
// icaro.preview.port wins; otherwise exactly one service publishing or
// exposing 80. Ícaro never guesses beyond that.
func SelectRoute(p *Project) (Route, error) {
	var labelled []Route
	var on80 []string
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		if v, ok := s.Labels[PortLabel]; ok {
			port, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || port < 1 || port > 65535 {
				return Route{}, fmt.Errorf("service %q: label %s=%q is not a port", name, PortLabel, v)
			}
			labelled = append(labelled, Route{Service: name, Port: port})
		}
		for _, port := range s.Ports {
			if port == 80 {
				on80 = append(on80, name)
				break
			}
		}
	}
	switch {
	case len(labelled) == 1:
		return labelled[0], nil
	case len(labelled) > 1:
		names := make([]string, len(labelled))
		for i, r := range labelled {
			names[i] = r.Service
		}
		return Route{}, fmt.Errorf("%w: several services carry %s (%s); keep it on one", ErrNoRoute, PortLabel, strings.Join(names, ", "))
	case len(on80) == 1:
		return Route{Service: on80[0], Port: 80}, nil
	case len(on80) > 1:
		return Route{}, fmt.Errorf("%w: services %s all use port 80; add the label %s to the one to preview", ErrNoRoute, strings.Join(on80, ", "), PortLabel)
	}
	// Name what there is to choose from, but do not choose.
	var cands []string
	for _, name := range p.ServiceNames() {
		if ports := p.Services[name].Ports; len(ports) > 0 {
			ps := make([]string, len(ports))
			for i, port := range ports {
				ps[i] = strconv.Itoa(port)
			}
			cands = append(cands, name+" ("+strings.Join(ps, ", ")+")")
		}
	}
	if len(cands) == 0 {
		return Route{}, fmt.Errorf("%w: no service publishes or exposes a TCP port; add `labels: {%s: \"<port>\"}` to the service to preview", ErrNoRoute, PortLabel)
	}
	return Route{}, fmt.Errorf("%w: no service uses port 80 or carries %s; services with ports: %s; add `labels: {%s: \"<port>\"}` to the one to preview", ErrNoRoute, PortLabel, strings.Join(cands, ", "), PortLabel)
}

// PollingEnv is added to the routed service on macOS, where bind-mounted
// worktrees do not deliver inotify events to the dev server reliably
// (architecture §7, spike S1). Hot reload stays the app's job.
var PollingEnv = map[string]string{
	"CHOKIDAR_USEPOLLING": "true",
	"WATCHPACK_POLLING":   "true",
	"ICARO_POLLING_WATCH": "true",
}

// OverrideOptions parameterize the generated compose override.
type OverrideOptions struct {
	InstanceID string
	Hostname   string
	MacOS      bool
}

// Override renders the compose override that joins the routed service to
// the preview network under a stable alias. The user's file is untouched.
func Override(p *Project, r Route, n Naming, o OverrideOptions) ([]byte, error) {
	s, ok := p.Services[r.Service]
	if !ok {
		return nil, fmt.Errorf("service %q not in compose file", r.Service)
	}
	if s.NetworkMode != "" {
		return nil, fmt.Errorf("service %q sets network_mode %q, so it cannot join the %s network; remove network_mode from the previewed service", r.Service, s.NetworkMode, Network)
	}
	nets := map[string]any{Network: map[string]any{"aliases": []string{n.Alias}}}
	if !s.HasNetworks {
		// Declaring networks replaces the implicit default; keep it.
		nets["default"] = map[string]any{}
	}
	svc := map[string]any{
		"networks": nets,
		"labels": map[string]string{
			"icaro.preview.instance": o.InstanceID,
			"icaro.preview.host":     o.Hostname,
		},
	}
	if o.MacOS {
		svc["environment"] = PollingEnv
	}
	doc := map[string]any{
		"services": map[string]any{r.Service: svc},
		"networks": map[string]any{Network: map[string]any{"external": true, "name": Network}},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Generated by icaro for preview "+o.InstanceID+"; do not edit.\n"), body...), nil
}

// TraefikConfig renders the dynamic file-provider config routing hostname
// to the service alias on the preview network.
func TraefikConfig(n Naming, hostname string, r Route) ([]byte, error) {
	name := n.Project
	doc := map[string]any{
		"http": map[string]any{
			"routers": map[string]any{
				name: map[string]any{
					"rule":        "Host(`" + hostname + "`)",
					"entryPoints": []string{"web"},
					"service":     name,
				},
			},
			"services": map[string]any{
				name: map[string]any{
					"loadBalancer": map[string]any{
						"servers": []map[string]string{{"url": fmt.Sprintf("http://%s:%d", n.Alias, r.Port)}},
					},
				},
			},
		},
	}
	return yaml.Marshal(doc)
}

// configFiles are hashed in addition to the compose file: a change to any
// of them restarts the preview (dependency or env changes need a rebuild).
var configFiles = []string{
	".env", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", "go.sum", "Gemfile.lock",
	"poetry.lock", "Pipfile.lock", "uv.lock", "Cargo.lock", "composer.lock", "mix.lock", "Dockerfile",
}

// ConfigHash hashes the compose files, .env and lockfiles in dir. Code
// edits do not change it: hot reload is the dev server's job.
func ConfigHash(dir string) (string, error) {
	names := append(append([]string{}, ComposeFileNames...), OverrideFileNames...)
	names = append(names, configFiles...)
	if m, _ := filepath.Glob(filepath.Join(dir, "requirements*.txt")); len(m) > 0 {
		for _, p := range m {
			names = append(names, filepath.Base(p))
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "compose.*.yaml")); len(m) > 0 {
		for _, p := range m {
			names = append(names, filepath.Base(p))
		}
	}
	sort.Strings(names)
	h := sha256.New()
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		f, err := os.Open(filepath.Join(dir, n))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00", n)
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
