package preview

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"icaro/internal/store"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestComposeFiles(t *testing.T) {
	dir := t.TempDir()
	if ComposeFiles(dir) != nil {
		t.Fatal("no compose file, no files")
	}
	// An override alone does not make a preview.
	write(t, dir, "compose.override.yaml", "services: {}\n")
	if ComposeFiles(dir) != nil {
		t.Fatal("an override without a compose file is not a project")
	}
	write(t, dir, "compose.yaml", "services: {}\n")
	if got := ComposeFiles(dir); !slices.Equal(got, []string{filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "compose.override.yaml")}) {
		t.Fatal(got)
	}
	// Compose's own precedence: compose.override.yml first.
	write(t, dir, "compose.override.yml", "services: {}\n")
	if got := ComposeFiles(dir); got[1] != filepath.Join(dir, "compose.override.yml") {
		t.Fatal(got)
	}
	// A directory with an override's name is not one.
	legacy := t.TempDir()
	write(t, legacy, "docker-compose.yml", "services: {}\n")
	if err := os.Mkdir(filepath.Join(legacy, "docker-compose.override.yml"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := ComposeFiles(legacy); len(got) != 1 {
		t.Fatal(got)
	}
}

// A UDP or SCTP port cannot serve a preview, so it is never a candidate.
func TestPortsAreTCPOnly(t *testing.T) {
	p := mustProject(t, `services:
  a:
    image: x
    ports: ["80:80/udp", "8080:80/tcp", {target: 80, protocol: udp}, {target: 443, protocol: tcp}, {target: 9000}]
    expose: ["53/udp", "3000/tcp", "4000"]
`)
	if got := p.Services["a"].Ports; !slices.Equal(got, []int{80, 443, 9000, 3000, 4000}) {
		t.Fatal(got)
	}
}

func TestMergeProjectsIsASuperset(t *testing.T) {
	base := mustProject(t, "services:\n  web: {image: x, ports: [\"80\"], volumes: [\"./a:/a\"], labels: {a: \"1\"}}\nvolumes:\n  data: {}\n")
	over := mustProject(t, `services:
  web:
    privileged: true
    network_mode: host
    cap_add: [SYS_ADMIN]
    security_opt: [seccomp=unconfined]
    devices: ["/dev/kvm"]
    volumes: ["/:/host"]
    labels: {a: "2", icaro.preview.port: "3000"}
    ports: ["3000"]
    networks: [x]
  extra: {image: y}
volumes:
  cache: {driver: local}
`)
	m := MergeProjects(base, over)
	w := m.Services["web"]
	if !w.Privileged || w.NetworkMode != "host" || !w.HasNetworks || len(w.CapAdd) != 1 || len(w.SecurityOpt) != 1 || len(w.Devices) != 1 {
		t.Fatalf("flags and lists from the override must be kept: %+v", w)
	}
	if len(w.Mounts) != 2 || w.Mounts[1].Index != 1 {
		t.Fatalf("mounts are joined and reindexed: %+v", w.Mounts)
	}
	if w.Labels["a"] != "2" || w.Labels[PortLabel] != "3000" || !slices.Equal(w.Ports, []int{80, 3000}) {
		t.Fatalf("labels from the override win, ports join: %+v", w)
	}
	if m.Services["extra"] == nil || len(m.Volumes) != 2 {
		t.Fatalf("new services and volumes are added: %+v", m)
	}
}

func TestSelectRouteNamesTheCandidates(t *testing.T) {
	_, err := SelectRoute(mustProject(t, "services:\n  web: {image: x, ports: [\"3000\", \"9229\"]}\n  db: {image: y, ports: [\"5432:5432\"]}\n  worker: {image: z}\n"))
	if err == nil || !strings.Contains(err.Error(), "db (5432), web (3000, 9229)") || strings.Contains(err.Error(), "worker") {
		t.Fatal(err)
	}
	_, err = SelectRoute(mustProject(t, "services:\n  web: {image: x, ports: [\"80/udp\"]}\n"))
	if err == nil || !strings.Contains(err.Error(), "no service publishes or exposes a TCP port") {
		t.Fatal(err)
	}
}

func TestConfigHashSeesEveryOverrideName(t *testing.T) {
	for _, name := range OverrideFileNames {
		dir := t.TempDir()
		write(t, dir, "compose.yaml", "services: {web: {image: x}}\n")
		before, _ := ConfigHash(dir)
		write(t, dir, name, "services: {web: {ports: [\"80\"]}}\n")
		if after, _ := ConfigHash(dir); after == before {
			t.Errorf("editing %s must restart the preview", name)
		}
	}
}

// The override is part of the stack: lint sees it, and up runs it. Without
// the docker CLI the raw files are merged.
func TestOverrideIsLintedAndRun(t *testing.T) {
	t.Parallel()
	e := newFailEnv(t).start()
	e.compose.setConfigErr(fmt.Errorf("docker: %w", exec.ErrNotFound))

	// The label lives in the override only.
	dir := worktree(t, t.TempDir(), "w", "services:\n  web: {image: busybox}\n")
	write(t, dir, "compose.override.yaml", "services:\n  web: {labels: {icaro.preview.port: \"8000\"}}\n")
	p := e.add(dir)
	if p.Status != store.PreviewRunning || p.Service != "web" || p.Port != 8000 {
		t.Fatalf("%+v", p)
	}
	files := e.compose.filesFor(p.ComposeProject)
	if len(files) != 3 || files[0] != filepath.Join(norm(dir), "compose.yaml") || files[1] != filepath.Join(norm(dir), "compose.override.yaml") {
		t.Fatalf("up must get the compose file, its override and ours: %v", files)
	}

	// A privileged override is blocked like a privileged compose file.
	bad := worktree(t, t.TempDir(), "b", webCompose)
	write(t, bad, "compose.override.yaml", "services:\n  web: {privileged: true}\n")
	res, err := e.m.Lint(e.ctx, bad)
	if err != nil || len(res.Blocked) == 0 {
		t.Fatalf("lint must see the override: %+v %v", res, err)
	}
	if p := e.add(bad); p.Status != store.PreviewBlocked {
		t.Fatalf("%+v", p)
	}
}
