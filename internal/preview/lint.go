package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one blocked field in a compose file (03-security, compose
// lint). ID is stable so a user can allow exactly this finding, and it
// changes whenever what the finding allows changes (ADR 0018).
type Finding struct {
	ID      string `json:"id"`
	Service string `json:"service,omitempty"`
	Field   string `json:"field"`
	Value   string `json:"value,omitempty"`
	Reason  string `json:"reason"`
}

func (f Finding) String() string {
	if f.Value != "" {
		return fmt.Sprintf("%s: %s = %s — %s (allow with: --finding %s)", f.ID, f.Field, f.Value, f.Reason, f.ID)
	}
	return fmt.Sprintf("%s: %s — %s (allow with: --finding %s)", f.ID, f.Field, f.Reason, f.ID)
}

// DangerousCaps are capabilities that let a container escape or control
// the host network or kernel.
var DangerousCaps = map[string]bool{
	"ALL": true, "SYS_ADMIN": true, "NET_ADMIN": true, "SYS_PTRACE": true, "SYS_MODULE": true,
	"DAC_READ_SEARCH": true, "SYS_RAWIO": true, "SYS_BOOT": true, "SYS_TIME": true, "MAC_ADMIN": true,
	"MAC_OVERRIDE": true, "BPF": true, "PERFMON": true, "NET_RAW": true, "SYSLOG": true, "DAC_OVERRIDE": true,
	"SYS_RESOURCE": true, "LINUX_IMMUTABLE": true, "SETFCAP": true,
}

// forbiddenHostPaths may never be bind-mounted into a preview, even when
// they are under an allowed root.
var forbiddenHostPaths = []string{"/var/run", "/run", "/proc", "/sys", "/etc", "/dev", "/boot", "/var/lib/docker"}

// LintOptions say which host paths a preview may mount.
type LintOptions struct {
	// AllowedRoots are directories binds may come from: the worktree itself
	// and the config's mounts.allow list.
	AllowedRoots []string
}

// Lint returns every blocked field in p, sorted by ID.
func Lint(p *Project, o LintOptions) []Finding {
	var out []Finding
	add := func(svc, field, value, idSuffix, reason string) {
		id := idSuffix
		if svc != "" {
			id = svc + "/" + idSuffix
		}
		out = append(out, Finding{ID: id, Service: svc, Field: field, Value: value, Reason: reason})
	}
	roots := resolveRoots(o.AllowedRoots)
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		base := "services." + name + "."
		if s.Privileged {
			add(name, base+"privileged", "true", "privileged", "a privileged container has full access to the host kernel and devices")
		}
		for _, ns := range []struct{ field, val, why string }{
			{"pid", s.Pid, "shares the host process namespace (can see and signal host processes)"},
			{"network_mode", s.NetworkMode, "shares the host network stack (bypasses the preview network and can reach host-only services)"},
			{"ipc", s.Ipc, "shares host IPC (shared memory of host processes)"},
			{"userns_mode", s.Userns, "disables user-namespace remapping"},
			{"cgroup", s.Cgroup, "shares the host cgroup namespace"},
			{"uts", s.Uts, "shares the host UTS namespace"},
		} {
			switch {
			case strings.EqualFold(ns.val, "host"):
				add(name, base+ns.field, ns.val, ns.field, ns.why)
			case joinsOutside(p, ns.val):
				// The value is part of the ID: allowing one container's
				// namespace never allows another's, or the host's.
				add(name, base+ns.field, ns.val, ns.field+"/"+sanitizeID(ns.val),
					"joins the namespace of a container outside this compose project (an agent container, another preview or the proxy)")
			}
		}
		for _, vf := range s.VolumesFrom {
			if volumesFromOutside(p, vf) {
				add(name, base+"volumes_from", vf, "volumes_from/"+sanitizeID(vf),
					"mounts the volumes of a container outside this compose project, which can hold another container's home and credentials")
			}
		}
		for _, c := range s.CapAdd {
			cn := strings.TrimPrefix(strings.ToUpper(c), "CAP_")
			if DangerousCaps[cn] {
				add(name, base+"cap_add", c, "cap_add/"+cn, "capability "+cn+" lets a container escalate to the host or its network")
			}
		}
		for _, so := range s.SecurityOpt {
			l := strings.ToLower(strings.ReplaceAll(so, "=", ":"))
			if strings.Contains(l, "unconfined") || l == "label:disable" || strings.HasPrefix(l, "no-new-privileges:false") {
				add(name, base+"security_opt", so, "security_opt/"+sanitizeID(so), "disables a kernel confinement profile (seccomp/AppArmor/SELinux)")
			}
		}
		for _, d := range s.Devices {
			add(name, base+"devices", d, "devices/"+sanitizeID(d), "passes a host device into the container")
		}
		for _, m := range s.Mounts {
			if m.Type != "bind" {
				continue
			}
			resolved, reason := bindProblem(m.Source, roots)
			if reason != "" {
				add(name, fmt.Sprintf("%svolumes[%d]", base, m.Index), m.Source+":"+m.Target, mountID(resolved, m.Target, m.ReadOnly), reason)
			}
		}
		lintPreviewNames(p, name, s, add)
	}
	vols := make([]string, 0, len(p.Volumes))
	for n := range p.Volumes {
		vols = append(vols, n)
	}
	sort.Strings(vols)
	for _, n := range vols {
		v := p.Volumes[n]
		if v.Device == "" || !v.Bind {
			continue
		}
		if resolved, reason := bindProblem(v.Device, roots); reason != "" {
			add("", "volumes."+n+".driver_opts.device", v.Device, "volumes/"+sanitizeID(n)+"-"+shortHash(resolved), "named volume binds a host path: "+reason)
		}
	}
	lintNetworks(p, add)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// joinsOutside reports a namespace mode (pid, network_mode, ipc, uts, …)
// that joins a container which is not a service of this project.
func joinsOutside(p *Project, mode string) bool {
	l := strings.ToLower(mode)
	if strings.HasPrefix(l, "container:") {
		return true
	}
	if strings.HasPrefix(l, "service:") {
		return p.Services[mode[len("service:"):]] == nil
	}
	return false
}

// volumesFromOutside reports a volumes_from entry ("svc", "service:svc",
// "container:name", each with an optional ":ro"/":rw") that is not a
// service of this project.
func volumesFromOutside(p *Project, ref string) bool {
	l := strings.ToLower(ref)
	if strings.HasPrefix(l, "container:") {
		return true
	}
	if strings.HasPrefix(l, "service:") {
		ref = ref[len("service:"):]
	}
	svc, _, _ := strings.Cut(ref, ":")
	return p.Services[svc] == nil
}

// icaroName reports a name in the icaro- namespace Ícaro uses for its own
// networks, compose projects, aliases and containers.
func icaroName(s string) bool { return strings.HasPrefix(strings.ToLower(s), "icaro-") }

// takeoverReason explains every icaro- name finding (ADR 0017: a compose
// project can never claim a route).
const takeoverReason = "uses a name Ícaro reserves for its own networks and routes; it could join the preview network or answer for another preview's route"

// lintPreviewNames flags a service that would answer for an Ícaro name
// on a network: its name, container_name or a network alias. Ícaro's own
// override (which adds the routed service's alias) is never linted.
func lintPreviewNames(p *Project, name string, s *Service, add func(svc, field, value, idSuffix, reason string)) {
	base := "services." + name
	if icaroName(name) {
		add(name, base, name, "service_name", "the service name is a DNS alias on every network it joins, and it "+takeoverReason)
	}
	if icaroName(s.ContainerName) {
		add(name, base+".container_name", s.ContainerName, "container_name/"+sanitizeID(s.ContainerName), "container_name "+takeoverReason)
	}
	keys := make([]string, 0, len(s.Networks))
	for k := range s.Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, declared := p.Networks[k]; !declared && icaroName(k) {
			add(name, base+".networks", k, "networks/"+sanitizeID(k), "the network "+takeoverReason)
		}
		for _, a := range s.Networks[k] {
			if icaroName(a) {
				add(name, base+".networks."+k+".aliases", a, "networks/aliases/"+sanitizeID(a), "the alias "+takeoverReason)
			}
		}
	}
}

// lintNetworks flags top-level networks that resolve to an Ícaro-managed
// engine network (icaro-preview, or another preview's project network).
// A network compose names itself lives in this project's own namespace
// (<project>_<key>) and is fine.
func lintNetworks(p *Project, add func(svc, field, value, idSuffix, reason string)) {
	own := NameFor(p.Dir).Project
	keys := make([]string, 0, len(p.Networks))
	for k := range p.Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := p.Networks[k]
		name := n.Name
		if name == "" && n.External {
			name = k
		}
		if !icaroName(name) || (!n.External && name == own+"_"+k) {
			continue
		}
		add("", "networks."+k, name, "networks/"+sanitizeID(name), "the network "+takeoverReason)
	}
}

// mountID derives a bind finding's ID from what it mounts (the resolved
// source, the target and the mode), so a changed mount is a new finding
// that an earlier allowance does not cover.
func mountID(src, target string, readOnly bool) string {
	return "volumes/" + sanitizeID(target) + "-" + shortHash(fmt.Sprintf("%s\x00%s\x00%t", src, target, readOnly))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// resolveRoots resolves symlinks in the allowed roots, so a source is
// compared with where a root really is.
func resolveRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		r := filepath.Clean(root)
		if r == "" || r == "." {
			continue
		}
		if res, err := resolveSymlinks(r); err == nil {
			r = res
		}
		out = append(out, r)
	}
	return out
}

// bindProblem explains why a host path may not be mounted, or "" if it
// may. Docker follows symlinks in a bind source, so the check runs on
// where the path really leads; it returns that resolved path too.
func bindProblem(src string, roots []string) (resolved, reason string) {
	if strings.Contains(src, "$") {
		return src, "host path uses variable interpolation that could not be resolved; run the lint where `docker compose config` works"
	}
	clean := filepath.Clean(src)
	res, err := resolveSymlinks(clean)
	if err != nil {
		return clean, "host path cannot be resolved (" + err.Error() + ")"
	}
	via := ""
	if res != clean {
		via = " (it resolves to " + res + " through a symlink)"
	}
	// The written path is checked too: on macOS /etc and /var/run are
	// themselves symlinks into /private.
	for _, cand := range []string{clean, res} {
		if strings.HasSuffix(cand, "docker.sock") || strings.HasSuffix(cand, "containerd.sock") || strings.HasSuffix(cand, "podman.sock") {
			return res, "mounts a container engine socket, which is root on the host" + via
		}
		if cand == "/" {
			return res, "mounts the host root filesystem" + via
		}
		for _, f := range forbiddenHostPaths {
			if cand == f || strings.HasPrefix(cand, f+"/") {
				return res, "mounts the sensitive host path " + f + via
			}
		}
	}
	for _, r := range roots {
		if res == r || strings.HasPrefix(res, r+string(filepath.Separator)) {
			return res, ""
		}
	}
	return res, "mounts a host path outside the worktree and the mounts.allow list" + via
}

// maxSymlinks bounds symlink resolution, as the kernel does (ELOOP).
const maxSymlinks = 40

// resolveSymlinks resolves every symlink in the absolute path p. Unlike
// filepath.EvalSymlinks it accepts a path whose tail does not exist yet
// (Docker creates a missing bind source): the existing part is resolved,
// and a dangling symlink is followed to where Docker would create it.
func resolveSymlinks(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	sep := string(filepath.Separator)
	resolved := sep
	todo := strings.Split(p, sep)
	for links := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, c)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Mode()&fs.ModeSymlink == 0) {
			resolved = next
			continue
		}
		if err != nil {
			return "", err
		}
		if links++; links > maxSymlinks {
			return "", fmt.Errorf("too many levels of symbolic links at %s", next)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = sep
		}
		todo = append(strings.Split(target, sep), todo...)
	}
	return resolved, nil
}

func sanitizeID(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// Blocked filters findings to those not in allowed (finding IDs the user
// confirmed for this worktree).
func Blocked(findings []Finding, allowed map[string]bool) []Finding {
	var out []Finding
	for _, f := range findings {
		if !allowed[f.ID] {
			out = append(out, f)
		}
	}
	return out
}
