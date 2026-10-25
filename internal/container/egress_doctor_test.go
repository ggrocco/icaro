package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icaro/internal/container/docker"
	"icaro/internal/doctor"
)

type netFake struct {
	ping, list, remove error
	nets               []docker.NetworkInfo
	removed            []string
}

func (f *netFake) Ping(context.Context) error { return f.ping }
func (f *netFake) ListNetworks(_ context.Context, prefix string) ([]docker.NetworkInfo, error) {
	if prefix != "icaro-egress-" {
		return nil, errors.New("unexpected prefix " + prefix)
	}
	return f.nets, f.list
}
func (f *netFake) RemoveNetwork(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return f.remove
}

// TestEgressNetworkCheck: a non-internal egress network is an error; the
// fix removes only the unused ones.
func TestEgressNetworkCheck(t *testing.T) {
	ctx := context.Background()
	f := &netFake{ping: errors.New("down")}
	c := EgressNetworkCheck(f)
	if r := c.Run(ctx); r.Severity != doctor.Skipped {
		t.Fatalf("unreachable: %+v", r)
	}
	f.ping, f.list = nil, errors.New("boom")
	if r := c.Run(ctx); r.Severity != doctor.Error || !strings.Contains(r.Detail, "boom") {
		t.Fatalf("list error: %+v", r)
	}
	f.list = nil
	f.nets = []docker.NetworkInfo{{Name: "icaro-egress-claude", Internal: true}}
	if r := c.Run(ctx); r.Severity != doctor.OK {
		t.Fatalf("internal: %+v", r)
	}
	f.nets = append(f.nets, docker.NetworkInfo{Name: "icaro-egress-ab12-shell"}, docker.NetworkInfo{Name: "icaro-egress-codex", Containers: 2})
	r := c.Run(ctx)
	if r.Severity != doctor.Error || !strings.Contains(r.Detail, "icaro-egress-ab12-shell, icaro-egress-codex") ||
		!strings.Contains(r.Detail, "icaro-egress-codex (2 container(s))") || r.Fix == nil {
		t.Fatalf("not internal: %+v", r)
	}
	f.remove = errors.New("in use")
	if _, err := r.Fix(ctx); err == nil {
		t.Fatal("fix error must surface")
	}
	f.remove, f.removed = nil, nil
	if msg, err := r.Fix(ctx); err != nil || !strings.Contains(msg, "icaro-egress-ab12-shell") || len(f.removed) != 1 {
		t.Fatalf("fix: %q %v %v", msg, err, f.removed)
	}
	// Only networks in use: nothing to fix automatically.
	f.nets = []docker.NetworkInfo{{Name: "icaro-egress-codex", Containers: 1}}
	if r := c.Run(ctx); r.Severity != doctor.Error || r.Fix != nil {
		t.Fatalf("in use: %+v", r)
	}
}

// TestNetworkMismatch: an existing network is reused only when it is
// internal (if asked) and carries the requested labels.
func TestNetworkMismatch(t *testing.T) {
	labels := map[string]string{LabelEgress: "claude"}
	ok := docker.NetworkInfo{Name: "n", Internal: true, Labels: map[string]string{LabelEgress: "claude", "x": "y"}}
	if err := docker.NetworkMismatch(ok, true, labels); err != nil {
		t.Fatal(err)
	}
	if err := docker.NetworkMismatch(docker.NetworkInfo{Name: "n", Labels: labels}, true, labels); err == nil || !strings.Contains(err.Error(), "not internal") {
		t.Fatalf("not internal: %v", err)
	}
	if err := docker.NetworkMismatch(docker.NetworkInfo{Name: "n"}, false, nil); err != nil {
		t.Fatalf("no requirements: %v", err)
	}
	if err := docker.NetworkMismatch(docker.NetworkInfo{Name: "n", Internal: true, Labels: map[string]string{LabelEgress: "other"}}, true, labels); err == nil || !strings.Contains(err.Error(), "not created by Ícaro") {
		t.Fatalf("foreign labels: %v", err)
	}
}
