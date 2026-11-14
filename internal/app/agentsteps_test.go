package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/config"
	"icaro/internal/container/docker"
	"icaro/internal/creds"
	"icaro/internal/doctor"
)

func TestAgentStepsCheck(t *testing.T) {
	cfg := testConfig(t, "")
	ctx := context.Background()
	if r := AgentStepsCheck(cfg, nil).Run(ctx); r.Severity != doctor.OK || !strings.Contains(r.Detail, "agent steps are off") {
		t.Fatalf("%+v", r)
	}
	cfg.Profiles["coder"] = config.ProfileConfig{Agent: "codex", Image: "img/codex", Inject: map[string]string{"openai": "key"}}
	cfg.Profiles["sh"] = config.ProfileConfig{}
	cfg.Profiles["vim"] = config.ProfileConfig{Agent: "vim"}
	cfg.Workflows.AgentProfiles = []string{"claude", "coder"}
	version := map[string]string{"img/codex": "codex-cli 0.41.2"}
	var verErr error
	versions := func(_ context.Context, image string, cmd []string) (string, error) {
		if verErr != nil {
			return "", verErr
		}
		if v, ok := version[image]; ok {
			return v, nil
		}
		return "", errNotPulled
	}
	run := func() doctor.Result { return AgentStepsCheck(cfg, versions).Run(ctx) }
	// claude is not signed in; coder injects and its CLI is new enough.
	r := run()
	if r.Severity != doctor.Warning || !strings.Contains(r.Detail, "icaro login claude") || strings.Contains(r.Detail, "coder") {
		t.Fatalf("%+v", r)
	}
	seed := (&creds.Homes{Layout: cfg.Layout()}).SeedDir("claude")
	_ = os.MkdirAll(filepath.Join(seed, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(seed, ".claude", ".credentials.json"), []byte("{}"), 0o600)
	p := cfg.Profiles["claude"]
	p.Egress = append(p.Egress, "api.anthropic.com")
	cfg.Profiles["claude"] = p
	r = run()
	if r.Severity != doctor.OK || !strings.Contains(r.Detail, "not pulled yet") || !strings.Contains(r.Detail, "profile coder (codex) is ready") {
		t.Fatalf("%+v", r)
	}
	version["img/codex"] = "codex-cli 0.9.1"
	if r := run(); r.Severity != doctor.Warning || !strings.Contains(r.Detail, "codex 0.9.1 is older than 0.40.0") {
		t.Fatalf("%+v", r)
	}
	version["img/codex"] = "garbage"
	if r := run(); !strings.Contains(r.Detail, `printed no version ("garbage")`) {
		t.Fatalf("%+v", r)
	}
	verErr = errors.New("no docker")
	if r := run(); !strings.Contains(r.Detail, "cannot run codex --version") {
		t.Fatalf("%+v", r)
	}
	cfg.Workflows.AgentProfiles = []string{"sh", "vim"}
	if r := AgentStepsCheck(cfg, nil).Run(ctx); r.Severity != doctor.Warning || !strings.Contains(r.Detail, "cannot run workflow steps") || !strings.Contains(r.Detail, `unknown agent "vim"`) {
		t.Fatalf("%+v", r)
	}
}

func TestVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		less bool
	}{{"1.2.3", "1.10.0", true}, {"2.0", "2.0.0", false}, {"2.0.1", "2.0", false}, {"0.39.9", "0.40.0", true}} {
		if versionLess(tc.a, tc.b) != tc.less {
			t.Errorf("%s < %s", tc.a, tc.b)
		}
	}
	if versionIn("Claude Code 2.0.14 (build)") != "2.0.14" || versionIn("none") != "" {
		t.Fatal("versionIn")
	}
}

// probeFake adds ImageDigest to the Docker fake.
type probeFake struct {
	*docker.Fake
	local map[string]bool
}

func (p probeFake) ImageDigest(_ context.Context, ref string) (string, error) {
	if !p.local[ref] {
		return "", docker.ErrNotFound
	}
	return ref + "@sha256:1", nil
}

func TestDockerVersions(t *testing.T) {
	f := docker.NewFake()
	f.OnStart = func(c *docker.FakeContainer) docker.Behavior {
		return docker.Behavior{Stdout: strings.Join(c.Spec.Entrypoint, " ") + " 2.1.0\n"}
	}
	probe := probeFake{Fake: f, local: map[string]bool{"img/claude": true}}
	v := dockerVersions(probe)
	ctx := context.Background()
	out, err := v(ctx, "img/claude", []string{"claude", "--version"})
	if err != nil || out != "claude --version 2.1.0\n" {
		t.Fatalf("%q %v", out, err)
	}
	for _, c := range f.Containers {
		if !c.Removed || c.Spec.HostConfig.NetworkMode != "none" {
			t.Fatalf("probe container %+v", c)
		}
	}
	if _, err := v(ctx, "img/other", []string{"x"}); !errors.Is(err, errNotPulled) {
		t.Fatal(err)
	}
	for _, op := range []string{"inspect", "create", "start", "wait", "logs"} {
		if _, err := dockerVersions(failingProbe{probe, op})(ctx, "img/claude", []string{"x"}); err == nil || !strings.Contains(err.Error(), op) {
			t.Errorf("%s: %v", op, err)
		}
	}
}

// failingProbe fails one call.
type failingProbe struct {
	probeFake
	op string
}

func (p failingProbe) fail(op string) error {
	if p.op == op {
		return errors.New(op + " failed")
	}
	return nil
}

func (p failingProbe) ImageDigest(ctx context.Context, ref string) (string, error) {
	if err := p.fail("inspect"); err != nil {
		return "", err
	}
	return p.probeFake.ImageDigest(ctx, ref)
}

func (p failingProbe) CreateContainer(ctx context.Context, spec docker.Spec) (string, error) {
	if err := p.fail("create"); err != nil {
		return "", err
	}
	return p.probeFake.CreateContainer(ctx, spec)
}

func (p failingProbe) StartContainer(ctx context.Context, id string) error {
	if err := p.fail("start"); err != nil {
		return err
	}
	return p.probeFake.StartContainer(ctx, id)
}

func (p failingProbe) Wait(ctx context.Context, id string) (int, error) {
	if err := p.fail("wait"); err != nil {
		return 0, err
	}
	return p.probeFake.Wait(ctx, id)
}

func (p failingProbe) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	if err := p.fail("logs"); err != nil {
		return nil, err
	}
	return p.probeFake.Logs(ctx, id, follow)
}

func TestAgentStepsChecksFactory(t *testing.T) {
	cfg := testConfig(t, "")
	if cs := AgentStepsChecks(context.Background(), cfg, nil, nil); len(cs) != 1 || cs[0].Name != "agent steps" {
		t.Fatal(cs)
	}
	eng, err := docker.New("unix:///nonexistent.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	if cs := AgentStepsChecks(context.Background(), cfg, nil, eng); len(cs) != 1 {
		t.Fatal(cs)
	}
}
