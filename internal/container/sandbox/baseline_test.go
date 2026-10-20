package sandbox

import (
	"testing"

	"github.com/moby/moby/api/types/mount"
)

func TestBaseline(t *testing.T) {
	hc := Baseline(BaselineOptions{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: "/w", Target: "/workspace"}}, Limits: Limits{MemoryBytes: 1 << 30}})
	if !hc.ReadonlyRootfs || hc.CapDrop[0] != "ALL" || hc.Privileged || string(hc.NetworkMode) != "none" {
		t.Fatalf("%+v", hc)
	}
	if _, ok := hc.Tmpfs["/home"]; ok {
		t.Fatal("no tmpfs home unless asked")
	}
	if *hc.PidsLimit != 512 || hc.MemorySwap != 1<<30 || len(hc.Mounts) != 1 || !contains(hc.SecurityOpt, "no-new-privileges") {
		t.Fatalf("%+v", hc.Resources)
	}
	hc = Baseline(BaselineOptions{TmpfsHome: true, NetworkMode: "net", Runtime: "runsc"})
	if _, ok := hc.Tmpfs["/home"]; !ok || hc.NetworkMode != "net" || hc.Runtime != "runsc" {
		t.Fatalf("%+v", hc)
	}
	if SeccompProfile() == "" {
		t.Fatal("seccomp")
	}
}
