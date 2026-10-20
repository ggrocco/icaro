package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostConfigStandard(t *testing.T) {
	hc, err := HostConfig(Options{WorkspaceVolume: "ws", IOVolume: "io", Limits: Limits{MemoryBytes: 1 << 30, NanoCPUs: 1e9}})
	if err != nil {
		t.Fatal(err)
	}
	if !hc.ReadonlyRootfs || hc.CapDrop[0] != "ALL" || hc.Privileged || string(hc.NetworkMode) != "bridge" {
		t.Fatalf("defaults: %+v", hc)
	}
	if *hc.PidsLimit != 256 || hc.Memory != 1<<30 || hc.MemorySwap != 1<<30 || hc.NanoCPUs != 1e9 {
		t.Fatalf("limits: %+v", hc.Resources)
	}
	var seccomp string
	for _, so := range hc.SecurityOpt {
		if strings.HasPrefix(so, "seccomp=") {
			seccomp = strings.TrimPrefix(so, "seccomp=")
		}
	}
	if seccomp == "" || !contains(hc.SecurityOpt, "no-new-privileges") {
		t.Fatalf("security opts: %v", hc.SecurityOpt)
	}
	var prof struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names []string `json:"names"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(seccomp), &prof); err != nil {
		t.Fatal(err)
	}
	if prof.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatal(prof.DefaultAction)
	}
	allowed := map[string]bool{}
	for _, s := range prof.Syscalls {
		for _, n := range s.Names {
			allowed[n] = true
		}
	}
	for _, denied := range []string{"ptrace", "bpf", "mount", "umount2", "unshare", "setns", "keyctl", "init_module", "finit_module", "kexec_load"} {
		if allowed[denied] {
			t.Fatalf("%s must not be allowed", denied)
		}
	}
	for _, needed := range []string{"execve", "openat", "read", "write", "clone", "socket", "connect"} {
		if !allowed[needed] {
			t.Fatalf("%s must be allowed", needed)
		}
	}
	if len(hc.Mounts) != 2 || hc.Mounts[0].Target != WorkspacePath || hc.Mounts[1].Target != IOPath {
		t.Fatalf("mounts: %+v", hc.Mounts)
	}
	if hc.Tmpfs["/home"] == "" || hc.Tmpfs["/tmp"] == "" {
		t.Fatalf("tmpfs: %v", hc.Tmpfs)
	}
}

func TestHostConfigStrict(t *testing.T) {
	hc, err := HostConfig(Options{Profile: Strict, Network: NetworkNone, WorkspaceVolume: "ws", IOVolume: "io", StrictRuntime: "runsc"})
	if err != nil {
		t.Fatal(err)
	}
	if string(hc.NetworkMode) != "none" || *hc.PidsLimit != 64 || hc.Runtime != "runsc" {
		t.Fatalf("strict: %+v", hc)
	}
}

func TestTighten(t *testing.T) {
	if Tighten(100, 0) != 100 || Tighten(100, 200) != 100 || Tighten(100, 50) != 50 {
		t.Fatal("tighten")
	}
}

func TestForbiddenPaths(t *testing.T) {
	for _, p := range []string{"/var/run/docker.sock", "/proc/1", "/sys", "/", "/etc/passwd"} {
		if !IsForbiddenPath(p) {
			t.Fatalf("%s should be forbidden", p)
		}
	}
	if IsForbiddenPath("/data/x") {
		t.Fatal("/data/x should be allowed")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
