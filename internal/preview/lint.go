package preview

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one blocked field in a compose file (03-security, compose
// lint). ID is stable so a user can allow exactly this finding.
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
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		base := "services." + name + "."
		if s.Privileged {
			add(name, base+"privileged", "true", "privileged", "a privileged container has full access to the host kernel and devices")
		}
		for _, hostNS := range []struct{ field, val, why string }{
			{"pid", s.Pid, "shares the host process namespace (can see and signal host processes)"},
			{"network_mode", s.NetworkMode, "shares the host network stack (bypasses the preview network and can reach host-only services)"},
			{"ipc", s.Ipc, "shares host IPC (shared memory of host processes)"},
			{"userns_mode", s.Userns, "disables user-namespace remapping"},
			{"cgroup", s.Cgroup, "shares the host cgroup namespace"},
			{"uts", s.Uts, "shares the host UTS namespace"},
		} {
			if strings.EqualFold(hostNS.val, "host") {
				add(name, base+hostNS.field, hostNS.val, hostNS.field, hostNS.why)
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
			field := fmt.Sprintf("%svolumes[%d]", base, m.Index)
			id := fmt.Sprintf("volumes/%d", m.Index)
			if m.Type != "bind" {
				continue
			}
			if reason := bindProblem(m.Source, o.AllowedRoots); reason != "" {
				add(name, field, m.Source+":"+m.Target, id, reason)
			}
		}
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
		if reason := bindProblem(v.Device, o.AllowedRoots); reason != "" {
			add("", "volumes."+n+".driver_opts.device", v.Device, "volumes/"+n, "named volume binds a host path: "+reason)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// bindProblem explains why a host path may not be mounted, or "" if it may.
func bindProblem(src string, allowed []string) string {
	if strings.Contains(src, "$") {
		return "host path uses variable interpolation that could not be resolved; run the lint where `docker compose config` works"
	}
	clean := filepath.Clean(src)
	if strings.HasSuffix(clean, "docker.sock") || strings.HasSuffix(clean, "containerd.sock") || strings.HasSuffix(clean, "podman.sock") {
		return "mounts a container engine socket, which is root on the host"
	}
	if clean == "/" {
		return "mounts the host root filesystem"
	}
	for _, f := range forbiddenHostPaths {
		if clean == f || strings.HasPrefix(clean, f+"/") {
			return "mounts the sensitive host path " + f
		}
	}
	for _, root := range allowed {
		r := filepath.Clean(root)
		if r == "" || r == "." {
			continue
		}
		if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
			return ""
		}
	}
	return "mounts a host path outside the worktree and the mounts.allow list"
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
