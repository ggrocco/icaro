package container

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"icaro/internal/container/docker"
	"icaro/internal/store"
)

func TestAgentSpecBuild(t *testing.T) {
	git := filepath.Join(t.TempDir(), ".git")
	_ = os.MkdirAll(filepath.Join(git, "hooks"), 0o700)
	_ = os.WriteFile(filepath.Join(git, "config"), nil, 0o600)
	spec, ns := AgentSpec{
		Name: "s", Image: "img@sha256:x", User: "1:2", Workspace: "/w", GitDir: git, Home: HomeMount{Volume: "v"},
		IcaroDir: "/i", Egress: Attachment{Network: "n", Env: []string{"HTTPS_PROXY=p"}},
		Extra: []Mount{{Source: "/h", Target: "/home/host", Masks: []string{"/home/host/.ssh"}}},
	}.Build()
	if ns.Network != "n" || string(spec.HostConfig.NetworkMode) != "n" || spec.WorkingDir != WorkspacePath || spec.User != "1:2" {
		t.Fatalf("%+v %+v", spec, ns)
	}
	var got []string
	for _, m := range spec.HostConfig.Mounts {
		got = append(got, string(m.Type)+":"+m.Target+":"+map[bool]string{true: "ro", false: "rw"}[m.ReadOnly])
	}
	want := []string{"bind:/workspace:rw", "bind:" + git + ":rw", "bind:" + git + "/hooks:ro", "bind:" + git + "/config:ro",
		"volume:/home/icaro:rw", "bind:/icaro:rw", "bind:/home/host:rw", "tmpfs:/home/host/.ssh:rw"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("mounts:\n%v\nwant\n%v", got, want)
	}
	if _, ok := spec.HostConfig.Tmpfs["/home"]; ok {
		t.Fatal("profile home replaces tmpfs /home")
	}
	if !strings.Contains(strings.Join(spec.Env, " "), "HTTPS_PROXY=p") || spec.Entrypoint[0] != "sh" {
		t.Fatal(spec.Env)
	}
}

func TestExecArgsAndExitCode(t *testing.T) {
	args := ExecArgs(ExecOptions{Container: "c", User: "1:1", TTY: true, Interactive: true, Env: []string{"A=b"}, Cmd: []string{"claude", "-p", "hi"}})
	if strings.Join(args, " ") != "exec -i -t -u 1:1 -w /workspace -e A=b c claude -p hi" {
		t.Fatal(args)
	}
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 {
		t.Fatal("exit codes")
	}
	err := exec.Command("sh", "-c", "exit 42").Run()
	if ExitCode(err) != 42 || ExitCode(&wrapped{err}) != 42 {
		t.Fatalf("%v", err)
	}
	if QuoteArgs([]string{"a", "b c", ""}) != "a 'b c' ''" {
		t.Fatal(QuoteArgs([]string{"a", "b c", ""}))
	}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// fakeEngine records calls for egress and image tests.
type fakeEngine struct {
	networks   map[string]bool
	containers map[string]docker.State
	created    []docker.Spec
	connected  []string
	images     map[string]string
	pulled     []string
	imported   int
}

func newFake() *fakeEngine {
	return &fakeEngine{networks: map[string]bool{}, containers: map[string]docker.State{}, images: map[string]string{}}
}

func (f *fakeEngine) EnsureImage(_ context.Context, ref string) error {
	if _, ok := f.images[ref]; !ok {
		f.images[ref] = ref + "@sha256:1"
		f.images[ref+"@sha256:1"] = ref + "@sha256:1"
	}
	return nil
}
func (f *fakeEngine) PullImage(_ context.Context, ref string) error {
	f.pulled = append(f.pulled, ref)
	f.images[ref] = strings.Split(ref, "@")[0] + "@sha256:2"
	f.images[f.images[ref]] = f.images[ref]
	return nil
}
func (f *fakeEngine) ImageDigest(_ context.Context, ref string) (string, error) {
	d, ok := f.images[ref]
	if !ok {
		return "", docker.ErrNotFound
	}
	return d, nil
}
func (f *fakeEngine) ImportImage(_ context.Context, r io.Reader, ref string, _ []string) error {
	f.imported++
	f.images[ref] = "sha256:local"
	return nil
}
func (f *fakeEngine) EnsureNetwork(_ context.Context, name string, internal bool, _ map[string]string) error {
	f.networks[name] = internal
	return nil
}
func (f *fakeEngine) ConnectNetwork(_ context.Context, network, _ string, aliases []string) error {
	f.connected = append(f.connected, network+"="+strings.Join(aliases, ","))
	return nil
}
func (f *fakeEngine) CreateContainerOn(_ context.Context, spec docker.Spec, _ docker.NetSpec) (string, error) {
	f.created = append(f.created, spec)
	id := spec.Name
	f.containers[id] = docker.State{Exists: true, Labels: spec.Labels}
	return id, nil
}
func (f *fakeEngine) StartContainer(_ context.Context, id string) error {
	st := f.containers[id]
	st.Running = true
	f.containers[id] = st
	return nil
}
func (f *fakeEngine) Inspect(_ context.Context, id string) (docker.State, error) {
	return f.containers[id], nil
}
func (f *fakeEngine) RemoveContainer(_ context.Context, id string) error {
	delete(f.containers, id)
	return nil
}
func (f *fakeEngine) ListByLabel(_ context.Context, labels map[string]string) ([]string, error) {
	var out []string
	for id, st := range f.containers {
		ok := true
		for k, v := range labels {
			if st.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func TestDockerEgress(t *testing.T) {
	f := newFake()
	eg := &DockerEgress{Engine: f, HelperImage: "helper:1"}
	ctx := context.Background()
	att, err := eg.Ensure(ctx, "claude", []string{"api.anthropic.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.networks["icaro-egress-claude"] || att.Network != "icaro-egress-claude" || len(f.created) != 1 || f.connected[0] != "icaro-egress-claude=icaro-egress" {
		t.Fatalf("%+v %+v", f, att)
	}
	p := f.created[0]
	if p.User != "65534:65534" || !strings.Contains(strings.Join(p.Entrypoint, " "), "--allow api.anthropic.com") || !p.HostConfig.ReadonlyRootfs {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(strings.Join(att.Env, " "), "HTTPS_PROXY=http://icaro-egress:3128") {
		t.Fatal(att.Env)
	}
	// Same allowlist: reuse. New allowlist: replace.
	if _, err := eg.Ensure(ctx, "claude", []string{"api.anthropic.com"}); err != nil || len(f.created) != 1 {
		t.Fatal("proxy not reused")
	}
	if _, err := eg.Ensure(ctx, "claude", []string{"api.anthropic.com", "x.com"}); err != nil || len(f.created) != 2 || len(f.containers) != 1 {
		t.Fatalf("proxy not replaced: %d %d", len(f.created), len(f.containers))
	}
	// No allowlist: network only, no proxy, no proxy env.
	att, err = eg.Ensure(ctx, "shell", nil)
	if err != nil || len(att.Env) != 0 || len(f.created) != 2 {
		t.Fatalf("%+v %v", att, err)
	}
	// Helper image import from a non-ELF fails with a clear error.
	eg2 := &DockerEgress{Engine: f, HelperBinary: filepath.Join(t.TempDir(), "nope")}
	if _, err := eg2.Ensure(ctx, "p2", []string{"a"}); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("%v", err)
	}
}

type memPins struct{ m map[string]string }

func (p *memPins) PinImage(_ context.Context, ref, d string) error {
	p.m[ref] = d
	return nil
}
func (p *memPins) GetPinnedImage(_ context.Context, ref string) (*store.PinnedImage, error) {
	d, ok := p.m[ref]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &store.PinnedImage{Ref: ref, Digest: d}, nil
}
func (p *memPins) ListPinnedImages(context.Context) ([]*store.PinnedImage, error) {
	var out []*store.PinnedImage
	for r, d := range p.m {
		out = append(out, &store.PinnedImage{Ref: r, Digest: d})
	}
	return out, nil
}

func TestImagesPinByDigest(t *testing.T) {
	f := newFake()
	pins := &memPins{m: map[string]string{}}
	im := &Images{Engine: f, Pins: pins}
	ctx := context.Background()
	d, err := im.Resolve(ctx, "agent:1")
	if err != nil || d != "agent:1@sha256:1" || pins.m["agent:1"] != d {
		t.Fatalf("%s %v", d, err)
	}
	// The tag moving does not change what sessions run.
	f.images["agent:1"] = "agent:1@sha256:9"
	if d2, _ := im.Resolve(ctx, "agent:1"); d2 != d {
		t.Fatal("pin not honored")
	}
	// A missing pinned digest is unverified until re-pulled by digest.
	delete(f.images, d)
	probs, _ := im.Verify(ctx)
	if len(probs) != 1 || !probs[0].Fixable {
		t.Fatal(probs)
	}
	if err := im.Repair(ctx, probs[0]); err != nil || f.pulled[0] != d {
		t.Fatal(f.pulled)
	}
	pins.m["local:1"] = "sha256:gone"
	probs, _ = im.Verify(ctx)
	for _, p := range probs {
		if p.Ref == "local:1" && (p.Fixable || im.Repair(ctx, p) == nil) {
			t.Fatal("local images cannot be re-pulled")
		}
	}
	if _, err := im.Resolve(ctx, "local:1"); err == nil {
		t.Fatal("gone local image must error")
	}
	if d, err := im.Pin(ctx, "agent:1", true); err != nil || !strings.HasSuffix(d, "sha256:2") {
		t.Fatal(d, err)
	}
}
