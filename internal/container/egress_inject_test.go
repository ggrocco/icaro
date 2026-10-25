package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerEgressInjection: a profile that injects credentials gets a
// proxy that mounts the daemon's socket directory read-only, relays the
// injection port, runs as the session user, and tells agents where the
// relay is; profiles that inject nothing are unchanged.
func TestDockerEgressInjection(t *testing.T) {
	f := newFake()
	run := t.TempDir()
	eg := &DockerEgress{Engine: f, HelperImage: "helper:1", InjectUser: "1234:1234", InjectDir: func(p string) string {
		if p == "claude" || p == "offline" {
			return InjectSocketDir(run, p)
		}
		return ""
	}}
	ctx := context.Background()
	att, err := eg.Ensure(ctx, "claude", []string{"api.anthropic.com"})
	if err != nil {
		t.Fatal(err)
	}
	if att.Inject != "http://icaro-egress:3129" {
		t.Fatalf("inject URL: %+v", att)
	}
	env := strings.Join(att.Env, " ")
	if !strings.Contains(env, "HTTPS_PROXY=http://icaro-egress:3128") || !strings.Contains(env, "NO_PROXY=localhost,127.0.0.1,icaro-egress") {
		t.Fatalf("env: %s", env)
	}
	p := f.created[0]
	entry := strings.Join(p.Entrypoint, " ")
	if !strings.Contains(entry, "--inject-listen :3129 --inject-socket /icaro-inject/inject.sock") {
		t.Fatal(entry)
	}
	if p.User != "1234:1234" {
		t.Fatalf("the relay must run as the socket's owner: %s", p.User)
	}
	dir := InjectSocketDir(run, "claude")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v %v", fi, err)
	}
	var found bool
	for _, m := range p.HostConfig.Mounts {
		if m.Target == InjectMount {
			found = m.Source == dir && m.ReadOnly
		}
	}
	if !found {
		t.Fatalf("socket dir mount: %+v", p.HostConfig.Mounts)
	}
	if string(p.HostConfig.NetworkMode) != "bridge" || len(f.connected) != 1 {
		t.Fatalf("a proxy with an allowlist reaches out: %v %v", p.HostConfig.NetworkMode, f.connected)
	}

	// Injection only, no allowlist: the proxy stays on the internal
	// network and agents get no forward proxy.
	att, err = eg.Ensure(ctx, "offline", nil)
	if err != nil {
		t.Fatal(err)
	}
	p = f.created[1]
	if att.Inject == "" || strings.Contains(strings.Join(att.Env, " "), "HTTPS_PROXY") {
		t.Fatalf("%+v", att)
	}
	if string(p.HostConfig.NetworkMode) != NetworkName("offline") || len(f.connected) != 1 {
		t.Fatalf("an injection-only proxy must not reach the outside: %v %v", p.HostConfig.NetworkMode, f.connected)
	}

	// No injection: no mount, nobody user, no relay, no Inject URL.
	att, err = eg.Ensure(ctx, "codex", []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	p = f.created[2]
	if att.Inject != "" || p.User != "65534:65534" || len(p.HostConfig.Mounts) != 0 || strings.Contains(strings.Join(p.Entrypoint, " "), "inject") {
		t.Fatalf("%+v %+v", att, p)
	}
	if strings.Contains(strings.Join(att.Env, " "), "icaro-egress,") || !strings.Contains(strings.Join(att.Env, " "), "NO_PROXY=localhost,127.0.0.1") {
		t.Fatal(att.Env)
	}

	// Turning injection on for a profile recreates its proxy.
	eg2 := &DockerEgress{Engine: f, HelperImage: "helper:1", InjectDir: func(string) string { return InjectSocketDir(run, "codex") }}
	if _, err := eg2.Ensure(ctx, "codex", []string{"api.openai.com"}); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 4 || f.created[3].User != "65534:65534" {
		t.Fatalf("a changed injection setting must replace the proxy: %d", len(f.created))
	}
}

func TestDockerEgressInjectionFailures(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eg := &DockerEgress{Engine: newFake(), HelperImage: "helper:1", InjectDir: func(string) string { return filepath.Join(blocker, "d") }}
	if _, err := eg.Ensure(context.Background(), "p", nil); err == nil || !strings.Contains(err.Error(), "injection socket directory") {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	f := newFailEngine(map[string]error{"CreateContainerOn": boom})
	eg = &DockerEgress{Engine: f, HelperImage: "helper:1", InjectDir: func(string) string { return t.TempDir() }}
	if _, err := eg.Ensure(context.Background(), "p", nil); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
