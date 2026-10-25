// Package preview gives each worktree with a compose file a live URL
// (architecture §7). It lints the user's compose file, generates an
// override that joins the routed service to the dedicated icaro-preview
// network (never editing the user's file), runs `docker compose`, and
// routes a hostname to it through an Ícaro-managed Traefik.
package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ComposeFileNames are the files that make a directory a preview candidate,
// in compose's own precedence order.
var ComposeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// OverrideFileNames are the override files compose merges over the
// compose file when it runs without -f, in compose's precedence order.
var OverrideFileNames = []string{"compose.override.yml", "compose.override.yaml", "docker-compose.override.yml", "docker-compose.override.yaml"}

// FindComposeFile returns the compose file in dir, or "".
func FindComposeFile(dir string) string { return firstFile(dir, ComposeFileNames) }

// ComposeFiles returns the files compose itself would load from dir (the
// compose file and its override, if any), so the preview runs and lints
// the same stack as a plain `docker compose up`. It is nil without a
// compose file.
func ComposeFiles(dir string) []string {
	file := FindComposeFile(dir)
	if file == "" {
		return nil
	}
	if o := firstFile(dir, OverrideFileNames); o != "" {
		return []string{file, o}
	}
	return []string{file}
}

func firstFile(dir string, names []string) string {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// Project is the subset of a compose model the preview manager needs.
type Project struct {
	Dir      string
	Services map[string]*Service
	// Volumes are the top-level named volumes (driver_opts can bind host paths).
	Volumes map[string]TopVolume
	// Networks are the top-level networks, by their key in the file.
	Networks map[string]TopNetwork
}

// Service is one compose service.
type Service struct {
	Name        string
	Privileged  bool
	Pid         string
	NetworkMode string
	Ipc         string
	Userns      string
	Cgroup      string
	Uts         string
	CapAdd      []string
	SecurityOpt []string
	Devices     []string
	Mounts      []Mount
	Labels      map[string]string
	Ports       []int // container ports from ports and expose
	HasNetworks bool  // the service lists networks explicitly
	// Networks maps each network key the service joins to its aliases.
	Networks      map[string][]string
	VolumesFrom   []string
	ContainerName string
}

// Mount is a normalized service volume.
type Mount struct {
	Type     string // bind | volume | tmpfs | npipe
	Source   string // absolute host path for binds
	Target   string
	ReadOnly bool
	Index    int
}

// TopVolume is a top-level named volume.
type TopVolume struct {
	Driver string
	Device string // driver_opts.device when o includes bind
	Bind   bool
}

// TopNetwork is a top-level network.
type TopNetwork struct {
	Name     string // the explicit (or, from compose config, computed) engine name
	External bool
}

// ParseCompose parses a compose document (raw YAML, or the JSON produced
// by `docker compose config --format json`). Relative bind sources are
// resolved against dir.
func ParseCompose(data []byte, dir string) (*Project, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	p := &Project{Dir: dir, Services: map[string]*Service{}, Volumes: map[string]TopVolume{}, Networks: map[string]TopNetwork{}}
	services, _ := asMap(doc["services"])
	if len(services) == 0 {
		return nil, fmt.Errorf("compose file has no services")
	}
	for name, raw := range services {
		m, ok := asMap(raw)
		if !ok {
			return nil, fmt.Errorf("service %q is not a mapping", name)
		}
		s := &Service{Name: name, Labels: map[string]string{}}
		s.Privileged = asBool(m["privileged"])
		s.Pid, s.NetworkMode, s.Ipc = asString(m["pid"]), asString(m["network_mode"]), asString(m["ipc"])
		s.Userns, s.Cgroup, s.Uts = asString(m["userns_mode"]), asString(m["cgroup"]), asString(m["uts"])
		s.CapAdd = asStrings(m["cap_add"])
		s.SecurityOpt = asStrings(m["security_opt"])
		for _, d := range asList(m["devices"]) {
			if dm, ok := asMap(d); ok {
				s.Devices = append(s.Devices, asString(dm["source"]))
			} else {
				s.Devices = append(s.Devices, asString(d))
			}
		}
		s.Labels = asLabels(m["labels"])
		_, s.HasNetworks = m["networks"]
		s.Networks = parseServiceNetworks(m["networks"])
		s.VolumesFrom = asStrings(m["volumes_from"])
		s.ContainerName = asString(m["container_name"])
		for i, v := range asList(m["volumes"]) {
			mt, err := parseMount(v, dir)
			if err != nil {
				return nil, fmt.Errorf("service %q volume %d: %w", name, i, err)
			}
			mt.Index = i
			s.Mounts = append(s.Mounts, mt)
		}
		s.Ports = append(parsePorts(m["ports"]), parseExpose(m["expose"])...)
		p.Services[name] = s
	}
	vols, _ := asMap(doc["volumes"])
	for name, raw := range vols {
		vm, _ := asMap(raw)
		tv := TopVolume{Driver: asString(vm["driver"])}
		if opts, ok := asMap(vm["driver_opts"]); ok {
			tv.Device = asString(opts["device"])
			o := asString(opts["o"])
			tv.Bind = strings.Contains(o, "bind") || asString(opts["type"]) == "none"
			if tv.Device != "" && !filepath.IsAbs(tv.Device) {
				tv.Device = filepath.Join(dir, tv.Device)
			}
		}
		p.Volumes[name] = tv
	}
	nets, _ := asMap(doc["networks"])
	for name, raw := range nets {
		nm, _ := asMap(raw)
		tn := TopNetwork{Name: asString(nm["name"])}
		// external is a bool, or the legacy {name: ...} form.
		if em, ok := asMap(nm["external"]); ok {
			tn.External = true
			if n := asString(em["name"]); n != "" {
				tn.Name = n
			}
		} else {
			tn.External = asBool(nm["external"])
		}
		p.Networks[name] = tn
	}
	return p, nil
}

// parseServiceNetworks reads a service's networks in the list or the
// mapping form.
func parseServiceNetworks(v any) map[string][]string {
	out := map[string][]string{}
	if m, ok := asMap(v); ok {
		for k, val := range m {
			nm, _ := asMap(val)
			out[k] = asStrings(nm["aliases"])
		}
		return out
	}
	for _, x := range asList(v) {
		out[asString(x)] = nil
	}
	return out
}

// MergeProjects overlays override on base for the raw-file fallback, used
// only when the docker CLI is missing. It is a superset rather than
// compose's exact merge: lists are joined and flags ORed, so lint sees
// everything either file asks for; labels and scalar settings from the
// override win, as in compose.
func MergeProjects(base, override *Project) *Project {
	for name, o := range override.Services {
		s, ok := base.Services[name]
		if !ok {
			base.Services[name] = o
			continue
		}
		s.Privileged = s.Privileged || o.Privileged
		for _, f := range []struct {
			dst *string
			src string
		}{
			{&s.Pid, o.Pid}, {&s.NetworkMode, o.NetworkMode}, {&s.Ipc, o.Ipc},
			{&s.Userns, o.Userns}, {&s.Cgroup, o.Cgroup}, {&s.Uts, o.Uts},
		} {
			if f.src != "" {
				*f.dst = f.src
			}
		}
		s.CapAdd = append(s.CapAdd, o.CapAdd...)
		s.SecurityOpt = append(s.SecurityOpt, o.SecurityOpt...)
		s.Devices = append(s.Devices, o.Devices...)
		s.VolumesFrom = append(s.VolumesFrom, o.VolumesFrom...)
		if o.ContainerName != "" {
			s.ContainerName = o.ContainerName
		}
		if s.Networks == nil {
			s.Networks = map[string][]string{}
		}
		for k, aliases := range o.Networks {
			s.Networks[k] = append(s.Networks[k], aliases...)
		}
		for _, mt := range o.Mounts {
			mt.Index = len(s.Mounts)
			s.Mounts = append(s.Mounts, mt)
		}
		for k, v := range o.Labels {
			s.Labels[k] = v
		}
		s.Ports = append(s.Ports, o.Ports...)
		s.HasNetworks = s.HasNetworks || o.HasNetworks
	}
	for name, v := range override.Volumes {
		base.Volumes[name] = v
	}
	for name, n := range override.Networks {
		if b, ok := base.Networks[name]; ok {
			// Superset: keep what either file says about the engine name.
			n.External = n.External || b.External
			if n.Name == "" {
				n.Name = b.Name
			}
		}
		base.Networks[name] = n
	}
	return base
}

// ServiceNames returns service names sorted.
func (p *Project) ServiceNames() []string {
	out := make([]string, 0, len(p.Services))
	for n := range p.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func parseMount(v any, dir string) (Mount, error) {
	if m, ok := asMap(v); ok {
		mt := Mount{Type: asString(m["type"]), Source: asString(m["source"]), Target: asString(m["target"]), ReadOnly: asBool(m["read_only"])}
		if mt.Type == "" {
			mt.Type = "volume"
		}
		if mt.Type == "bind" {
			mt.Source = resolveHostPath(mt.Source, dir)
		}
		return mt, nil
	}
	s := asString(v)
	if s == "" {
		return Mount{}, fmt.Errorf("empty volume")
	}
	parts := splitVolume(s)
	switch len(parts) {
	case 1:
		return Mount{Type: "volume", Target: parts[0]}, nil
	default:
		src, dst := parts[0], parts[1]
		ro := len(parts) == 3 && slices.Contains(strings.Split(parts[2], ","), "ro")
		if isPathLike(src) {
			return Mount{Type: "bind", Source: resolveHostPath(src, dir), Target: dst, ReadOnly: ro}, nil
		}
		return Mount{Type: "volume", Source: src, Target: dst, ReadOnly: ro}, nil
	}
}

// splitVolume splits "src:dst[:mode]" (Windows drive letters are not supported).
func splitVolume(s string) []string { return strings.SplitN(s, ":", 3) }

func isPathLike(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.Contains(s, "/") || strings.Contains(s, "$")
}

func resolveHostPath(src, dir string) string {
	if strings.HasPrefix(src, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			src = filepath.Join(home, strings.TrimPrefix(src, "~"))
		}
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(dir, src)
	}
	return filepath.Clean(src)
}

// parsePorts returns the container ports of TCP mappings; a UDP or SCTP
// port cannot serve a preview, so it never takes part in port discovery.
func parsePorts(v any) []int {
	var out []int
	for _, p := range asList(v) {
		if m, ok := asMap(p); ok {
			if proto := asString(m["protocol"]); proto != "" && !strings.EqualFold(proto, "tcp") {
				continue
			}
			if n := atoi(asString(m["target"])); n > 0 {
				out = append(out, n)
			}
			continue
		}
		s, proto, _ := strings.Cut(asString(p), "/")
		if proto != "" && !strings.EqualFold(proto, "tcp") {
			continue
		}
		parts := strings.Split(s, ":")
		if n := atoi(parts[len(parts)-1]); n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func parseExpose(v any) []int {
	var out []int
	for _, p := range asList(v) {
		s, proto, _ := strings.Cut(asString(p), "/")
		if proto != "" && !strings.EqualFold(proto, "tcp") {
			continue
		}
		if n := atoi(s); n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out, true
	}
	return nil, false
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case json.Number:
		return s.String()
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		ok, _ := strconv.ParseBool(b)
		return ok
	}
	return false
}

func asStrings(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, asString(x))
	}
	return out
}

// asLabels accepts both the mapping and the "k=v" list forms.
func asLabels(v any) map[string]string {
	out := map[string]string{}
	if m, ok := asMap(v); ok {
		for k, val := range m {
			out[k] = asString(val)
		}
		return out
	}
	for _, x := range asList(v) {
		k, val, _ := strings.Cut(asString(x), "=")
		out[k] = val
	}
	return out
}
