package preview

import (
	"context"
	"testing"

	"icaro/internal/container/docker"
	"icaro/internal/doctor"
)

// Regression: a fresh install has neither the network nor the proxy (both
// come with the first preview), and that must not fail `icaro doctor`.
func TestNetworkCheckStates(t *testing.T) {
	ctx := context.Background()
	e := newFakeEngine()
	check := NetworkCheck(e, "icaro-traefik-test")

	if r := check.Run(ctx); r.Severity != doctor.OK {
		t.Fatalf("fresh install: %+v", r)
	}

	e.networks[Network] = true
	e.proxy = &docker.State{Exists: true, Running: true}
	if r := check.Run(ctx); r.Severity != doctor.OK {
		t.Fatalf("healthy: %+v", r)
	}

	e.proxy.Running = false
	r := check.Run(ctx)
	if r.Severity != doctor.Warning || r.Fix == nil {
		t.Fatalf("stopped proxy: %+v", r)
	}

	// A proxy left without its network is the real problem; the fix
	// recreates the network and drops the proxy.
	delete(e.networks, Network)
	r = check.Run(ctx)
	if r.Severity != doctor.Error || r.Fix == nil {
		t.Fatalf("proxy without network: %+v", r)
	}
	if _, err := r.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.networks[Network] || e.proxy != nil {
		t.Fatalf("fix did not recreate the network / drop the proxy: %v %v", e.networks, e.proxy)
	}
	if r := check.Run(ctx); r.Severity != doctor.OK {
		t.Fatalf("after fix: %+v", r)
	}
}
