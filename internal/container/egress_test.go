package container

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"icaro/internal/container/docker"
)

// failEngine is fakeEngine with an error injected per method name.
type failEngine struct {
	*fakeEngine
	fail    map[string]error
	removed []string
}

func newFailEngine(fail map[string]error) *failEngine {
	return &failEngine{fakeEngine: newFake(), fail: fail}
}

func (f *failEngine) EnsureImage(ctx context.Context, ref string) error {
	if err := f.fail["EnsureImage"]; err != nil {
		return err
	}
	return f.fakeEngine.EnsureImage(ctx, ref)
}
func (f *failEngine) PullImage(ctx context.Context, ref string) error {
	if err := f.fail["PullImage"]; err != nil {
		return err
	}
	return f.fakeEngine.PullImage(ctx, ref)
}
func (f *failEngine) ImageDigest(ctx context.Context, ref string) (string, error) {
	if err := f.fail["ImageDigest"]; err != nil {
		return "", err
	}
	return f.fakeEngine.ImageDigest(ctx, ref)
}
func (f *failEngine) ImportImage(ctx context.Context, r io.Reader, ref string, ch []string) error {
	if err := f.fail["ImportImage"]; err != nil {
		return err
	}
	return f.fakeEngine.ImportImage(ctx, r, ref, ch)
}
func (f *failEngine) CreateNetwork(ctx context.Context, name string, internal bool, l map[string]string) error {
	if err := f.fail["CreateNetwork"]; err != nil {
		return err
	}
	return f.fakeEngine.CreateNetwork(ctx, name, internal, l)
}
func (f *failEngine) ConnectNetwork(ctx context.Context, network, id string, aliases []string) error {
	if err := f.fail["ConnectNetwork"]; err != nil {
		return err
	}
	return f.fakeEngine.ConnectNetwork(ctx, network, id, aliases)
}
func (f *failEngine) CreateContainerOn(ctx context.Context, spec docker.Spec, n docker.NetSpec) (string, error) {
	if err := f.fail["CreateContainerOn"]; err != nil {
		return "", err
	}
	return f.fakeEngine.CreateContainerOn(ctx, spec, n)
}
func (f *failEngine) StartContainer(ctx context.Context, id string) error {
	if err := f.fail["StartContainer"]; err != nil {
		return err
	}
	return f.fakeEngine.StartContainer(ctx, id)
}
func (f *failEngine) Inspect(ctx context.Context, id string) (docker.State, error) {
	if err := f.fail["Inspect"]; err != nil {
		return docker.State{}, err
	}
	return f.fakeEngine.Inspect(ctx, id)
}
func (f *failEngine) RemoveContainer(ctx context.Context, id string) error {
	f.removed = append(f.removed, id)
	return f.fakeEngine.RemoveContainer(ctx, id)
}
func (f *failEngine) ListByLabel(ctx context.Context, labels map[string]string) ([]string, error) {
	if err := f.fail["ListByLabel"]; err != nil {
		return nil, err
	}
	return f.fakeEngine.ListByLabel(ctx, labels)
}

func TestNamespace(t *testing.T) {
	a := Namespace("/home/u/.icaro")
	if len(a) != 8 || a != Namespace("/home/u/.icaro/") || a != Namespace("/home/u/./.icaro") {
		t.Fatalf("namespace must be 8 hex chars of the cleaned root: %q", a)
	}
	if a == Namespace("/home/v/.icaro") {
		t.Fatal("different roots must not share a namespace")
	}
}

func TestDockerEgressNamespaced(t *testing.T) {
	f := newFake()
	var logs bytes.Buffer
	eg := &DockerEgress{Engine: f, HelperImage: "helper:1", Namespace: "abcd1234", OutsideNetwork: "outside",
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	att, err := eg.Ensure(context.Background(), "claude", []string{"b.com", "a.com"})
	if err != nil {
		t.Fatal(err)
	}
	if att.Network != "icaro-egress-abcd1234-claude" || !f.networks[att.Network] {
		t.Fatalf("namespaced network: %+v", att)
	}
	p := f.created[0]
	if p.Labels[LabelEgress] != "abcd1234/claude" || !strings.HasPrefix(p.Name, "icaro-egress-abcd1234-claude-proxy-") {
		t.Fatalf("namespaced proxy: %+v", p)
	}
	if string(p.HostConfig.NetworkMode) != "outside" || p.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("proxy host config: %+v", p.HostConfig)
	}
	// The allowlist is sorted, so its order never changes the proxy hash.
	if !strings.Contains(strings.Join(p.Entrypoint, " "), "--allow a.com,b.com") {
		t.Fatal(p.Entrypoint)
	}
	if _, err := eg.Ensure(context.Background(), "claude", []string{"a.com", "b.com"}); err != nil || len(f.created) != 1 {
		t.Fatalf("reordered allowlist must reuse the proxy: %v %d", err, len(f.created))
	}
	if !strings.Contains(logs.String(), "egress proxy started") {
		t.Fatal(logs.String())
	}
}

func TestDockerEgressDropsStaleProxies(t *testing.T) {
	f := newFake()
	eg := &DockerEgress{Engine: f, HelperImage: "helper:1"}
	ctx := context.Background()
	if _, err := eg.Ensure(ctx, "p", []string{"a.com"}); err != nil {
		t.Fatal(err)
	}
	id := f.created[0].Name
	// A stopped proxy with the right hash is not reused: it is replaced.
	st := f.containers[id]
	st.Running = false
	f.containers[id] = st
	// Of two running proxies with the right hash one is kept, one removed.
	f.containers["dup1"] = docker.State{Exists: true, Running: true, Labels: st.Labels}
	f.containers["dup2"] = docker.State{Exists: true, Running: true, Labels: st.Labels}
	if _, err := eg.Ensure(ctx, "p", []string{"a.com"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.containers[id]; ok {
		t.Fatal("stopped proxy kept")
	}
	if len(f.containers) != 1 {
		t.Fatalf("exactly one proxy must remain: %v", f.containers)
	}
}

func TestDockerEgressErrors(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		method, want string
		removes      bool
	}{
		{"CreateNetwork", "egress network", false},
		{"EnsureImage", "boom", false},
		{"ListByLabel", "boom", false},
		{"CreateContainerOn", "create egress proxy", false},
		{"ConnectNetwork", "attach egress proxy", true},
		{"StartContainer", "start egress proxy", true},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			f := newFailEngine(map[string]error{c.method: boom})
			eg := &DockerEgress{Engine: f, HelperImage: "helper:1"}
			_, err := eg.Ensure(context.Background(), "p", []string{"a.com"})
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if c.removes && (len(f.removed) != 1 || len(f.containers) != 0) {
				t.Fatalf("a half-created proxy must be removed: %v", f.removed)
			}
		})
	}
	t.Run("Inspect", func(t *testing.T) {
		f := newFailEngine(nil)
		eg := &DockerEgress{Engine: f, HelperImage: "helper:1"}
		if _, err := eg.Ensure(context.Background(), "p", []string{"a.com"}); err != nil {
			t.Fatal(err)
		}
		f.fail = map[string]error{"Inspect": boom}
		if _, err := eg.Ensure(context.Background(), "p", []string{"a.com"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
}

// writeELF writes a minimal 64-bit little-endian ELF executable; with
// interp it carries a PT_INTERP program header (a dynamic binary).
func writeELF(t *testing.T, interp bool) string {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b.Write(make([]byte, 8))
	le := binary.LittleEndian
	w := func(v any) { _ = binary.Write(&b, le, v) }
	w(uint16(2))  // e_type: EXEC
	w(uint16(62)) // e_machine: x86-64
	w(uint32(1))  // e_version
	w(uint64(0))  // e_entry
	phoff, phnum := uint64(0), uint16(0)
	if interp {
		phoff, phnum = 64, 1
	}
	w(phoff)      // e_phoff
	w(uint64(0))  // e_shoff
	w(uint32(0))  // e_flags
	w(uint16(64)) // e_ehsize
	w(uint16(56)) // e_phentsize
	w(phnum)      // e_phnum
	w(uint16(64)) // e_shentsize
	w(uint16(0))  // e_shnum
	w(uint16(0))  // e_shstrndx
	if interp {
		w(uint32(3)) // p_type: PT_INTERP
		w(uint32(4)) // p_flags
		for range 6 {
			w(uint64(0)) // offset, vaddr, paddr, filesz, memsz, align
		}
	}
	p := filepath.Join(t.TempDir(), "icaro")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHelperImageFromBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		if _, _, err := HelperImageFromBinary(writeELF(t, false)); !errors.Is(err, ErrNoHelper) {
			t.Fatal(err)
		}
		t.Skip("helper images are only built from Linux binaries")
	}
	static := writeELF(t, false)
	ref, data, err := HelperImageFromBinary(static)
	if err != nil || !strings.HasPrefix(ref, "icaro-helper:") || len(data) == 0 {
		t.Fatalf("%s %v", ref, err)
	}
	ref2, _, _ := HelperImageFromBinary(static)
	if ref2 != ref {
		t.Fatal("the helper ref must be a content hash")
	}
	if _, _, err := HelperImageFromBinary(writeELF(t, true)); !errors.Is(err, ErrNoHelper) || !strings.Contains(err.Error(), "dynamically linked") {
		t.Fatalf("dynamic binary: %v", err)
	}

	// The egress imports the helper image once and reuses it afterwards.
	f := newFailEngine(nil)
	eg := &DockerEgress{Engine: f, HelperBinary: static}
	ctx := context.Background()
	if _, err := eg.Ensure(ctx, "p", []string{"a.com"}); err != nil || f.imported != 1 || f.created[0].Image != ref {
		t.Fatalf("%v imported=%d", err, f.imported)
	}
	if _, err := eg.Ensure(ctx, "q", []string{"a.com"}); err != nil || f.imported != 1 {
		t.Fatalf("helper image must be reused: %v imported=%d", err, f.imported)
	}
	f2 := newFailEngine(map[string]error{"ImportImage": errors.New("boom")})
	eg2 := &DockerEgress{Engine: f2, HelperBinary: static}
	if _, err := eg2.Ensure(ctx, "p", []string{"a.com"}); err == nil || !strings.Contains(err.Error(), "import helper image") {
		t.Fatal(err)
	}
}

func TestHelperImageDefaultsToExecutable(t *testing.T) {
	// With no helper image or binary the running executable is used; the
	// test binary may be static or dynamic, so either outcome is valid,
	// but a dynamic one must fail with ErrNoHelper.
	f := newFake()
	eg := &DockerEgress{Engine: f}
	_, err := eg.Ensure(context.Background(), "p", []string{"a.com"})
	if err != nil && !errors.Is(err, ErrNoHelper) {
		t.Fatal(err)
	}
}
