package app

import (
	"context"
	"strings"
	"testing"

	"icaro/internal/config"
	"icaro/internal/container"
	"icaro/internal/doctor"
)

func injectConfig(c *config.Config) {
	p := c.Profiles["claude"]
	p.Inject = map[string]string{"anthropic": "anthropic-key"}
	c.Profiles["claude"] = p
	q := c.Profiles["codex"]
	q.Inject = map[string]string{"openai": "openai-key"}
	c.Profiles["codex"] = q
}

func TestInjectWiring(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), injectConfig)
	runDir := a.Layout.RunDir()
	if got := a.Egress.InjectDir("claude"); got != container.InjectSocketDir(runDir, "claude") {
		t.Fatalf("claude: %q", got)
	}
	if got := a.Egress.InjectDir("shell"); got != "" {
		t.Fatalf("a profile without inject must not get the socket: %q", got)
	}
	if a.Egress.InjectUser != a.Sessions.User() {
		t.Fatalf("the relay must run as the session user: %q", a.Egress.InjectUser)
	}
	found := false
	for _, c := range a.components {
		found = found || c.Name() == "inject"
	}
	if !found {
		t.Fatal("the injector is not a daemon component")
	}
	if p := InjectSocketPath(a.Cfg, "claude"); !strings.HasPrefix(p, runDir) || !strings.HasSuffix(p, "/inject/claude/inject.sock") {
		t.Fatal(p)
	}
	findCheck(t, a.Checks(), "credential injection")
}

func TestNoInjectWiring(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), nil)
	if a.Egress.InjectDir != nil {
		t.Fatal("no profile injects, so no proxy relays")
	}
	for _, c := range a.components {
		if c.Name() == "inject" {
			t.Fatal("injector started without an injecting profile")
		}
	}
	if r := run(t, InjectCheck(a.Cfg, a.Store)); r.Severity != doctor.OK || !strings.Contains(r.Detail, "no profile") {
		t.Fatalf("%+v", r)
	}
}

func TestInjectCheck(t *testing.T) {
	a := newTestApp(t, startFakeDocker(t, &fakeDocker{}), injectConfig)
	ctx := context.Background()
	check := InjectCheck(a.Cfg, a.Store)
	r := run(t, check)
	if r.Severity != doctor.Warning || !strings.Contains(r.Detail, "secret anthropic-key, which does not exist") ||
		!strings.Contains(r.Detail, "icaro secret set openai-key --host api.openai.com") {
		t.Fatalf("missing secrets: %+v", r)
	}
	if err := a.Secrets.Set(ctx, "anthropic-key", "sk-ant-x", []string{"api.anthropic.com"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Secrets.Set(ctx, "openai-key", "sk-x", nil); err != nil {
		t.Fatal(err)
	}
	r = run(t, check)
	if r.Severity != doctor.Warning || strings.Contains(r.Detail, "anthropic-key") || !strings.Contains(r.Detail, "openai-key is not bound to api.openai.com") {
		t.Fatalf("unbound secret: %+v", r)
	}
	if err := a.Secrets.Set(ctx, "openai-key", "sk-x", []string{"api.openai.com"}); err != nil {
		t.Fatal(err)
	}
	if r := run(t, check); r.Severity != doctor.OK || !strings.Contains(r.Detail, "claude, codex") {
		t.Fatalf("all good: %+v", r)
	}
	// A home so long the socket cannot be created.
	long := *a.Cfg
	long.Home = "/" + strings.Repeat("h", 80)
	if r := run(t, InjectCheck(&long, a.Store)); r.Severity != doctor.Warning || !strings.Contains(r.Detail, "Unix limit") {
		t.Fatalf("long home: %+v", r)
	}
	_ = a.Store.Close()
	if r := run(t, check); r.Severity != doctor.Error {
		t.Fatalf("closed store: %+v", r)
	}
}
