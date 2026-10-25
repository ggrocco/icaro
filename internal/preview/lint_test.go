package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	name, wantID, body string
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/malicious.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	for _, sec := range strings.Split(string(raw), "\n-- ")[1:] {
		header, body, _ := strings.Cut(sec, " --\n")
		f := strings.Fields(header)
		out = append(out, fixture{name: f[0], wantID: f[1], body: body})
	}
	if len(out) < 15 {
		t.Fatalf("only %d fixtures parsed", len(out))
	}
	return out
}

func TestLintBlocksMaliciousFixtures(t *testing.T) {
	dir := t.TempDir()
	for _, fx := range loadFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			p, err := ParseCompose([]byte(fx.body), dir)
			if err != nil {
				t.Fatal(err)
			}
			findings := Lint(p, LintOptions{AllowedRoots: []string{dir}})
			found := findID(findings, fx.wantID)
			if found == nil {
				t.Fatalf("want finding %s, got %+v", fx.wantID, findings)
			}
			if found.Reason == "" || found.Field == "" {
				t.Fatalf("finding must explain itself: %+v", found)
			}
			if !strings.Contains(found.String(), "--finding "+found.ID) {
				t.Fatalf("finding must say how to allow it: %s", found)
			}
		})
	}
}

// findID returns the finding with id, or nil; an id ending in "*"
// matches any suffix (mount IDs end in a hash of what they mount).
func findID(findings []Finding, id string) *Finding {
	for i := range findings {
		got := findings[i].ID
		if got == id || (strings.HasSuffix(id, "*") && strings.HasPrefix(got, strings.TrimSuffix(id, "*"))) {
			return &findings[i]
		}
	}
	return nil
}

func TestLintAllowsSafeCompose(t *testing.T) {
	dir := t.TempDir()
	extra := t.TempDir()
	body := `
services:
  web:
    build: .
    labels: ["icaro.preview.port=3000"]
    cap_add: [CHOWN]
    security_opt: ["no-new-privileges:true"]
    volumes:
      - ./src:/app/src
      - .:/app
      - node_modules:/app/node_modules
      - ` + extra + `/cache:/cache
      - type: tmpfs
        target: /tmp
  db:
    image: postgres
    volumes: ["pgdata:/var/lib/postgresql/data"]
volumes:
  node_modules: {}
  pgdata:
    driver: local
`
	p, err := ParseCompose([]byte(body), dir)
	if err != nil {
		t.Fatal(err)
	}
	if f := Lint(p, LintOptions{AllowedRoots: []string{dir, extra}}); len(f) != 0 {
		t.Fatalf("safe compose flagged: %+v", f)
	}
	// Without mounts.allow the extra root is outside the policy.
	f := Lint(p, LintOptions{AllowedRoots: []string{dir}})
	if len(f) != 1 || findID(f, "web/volumes/_cache-*") == nil {
		t.Fatalf("%+v", f)
	}
	if got := Blocked(f, map[string]bool{f[0].ID: true}); len(got) != 0 {
		t.Fatalf("allowed finding still blocked: %+v", got)
	}
}

// The normalized model from `docker compose config --format json` uses
// long forms everywhere.
func TestParseNormalizedJSON(t *testing.T) {
	dir := t.TempDir()
	js := `{"name":"x","services":{"web":{"image":"nginx","labels":{"icaro.preview.port":"8080"},
	"ports":[{"mode":"ingress","target":8080,"published":"8080","protocol":"tcp"}],
	"devices":[{"source":"/dev/fuse","target":"/dev/fuse","permissions":"rwm"}],
	"volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/s"},{"type":"volume","source":"v","target":"/v"}],
	"networks":{"default":null}}},"volumes":{"v":{"name":"x_v"}}}`
	p, err := ParseCompose([]byte(js), dir)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Services["web"]
	if !s.HasNetworks || s.Ports[0] != 8080 || s.Devices[0] != "/dev/fuse" || len(s.Mounts) != 2 {
		t.Fatalf("%+v", s)
	}
	ids := map[string]bool{}
	for _, f := range Lint(p, LintOptions{AllowedRoots: []string{dir}}) {
		ids[f.ID] = true
	}
	if !ids["web/"+mountID(resolvedSock(t), "/s", false)] || !ids["web/devices/_dev_fuse"] || len(ids) != 2 {
		t.Fatalf("%v", ids)
	}
}

func TestParseErrorsAndShortForms(t *testing.T) {
	for _, bad := range []string{"services: {}", ":", "services:\n  web: 3\n"} {
		if _, err := ParseCompose([]byte(bad), "/x"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	p, err := ParseCompose([]byte("services:\n  w:\n    image: x\n    volumes: [\"/data\", \"~/x:/y\"]\n    expose: [\"80/tcp\"]\n    ports: [\"127.0.0.1:8080:80\", \"9000\"]\n"), "/proj")
	if err != nil {
		t.Fatal(err)
	}
	s := p.Services["w"]
	if s.Mounts[0].Type != "volume" || s.Mounts[1].Type != "bind" || !filepath.IsAbs(s.Mounts[1].Source) {
		t.Fatalf("%+v", s.Mounts)
	}
	if len(s.Ports) != 3 {
		t.Fatalf("ports %v", s.Ports)
	}
	if FindComposeFile(t.TempDir()) != "" {
		t.Fatal("empty dir has no compose file")
	}
}

func lintIDs(t *testing.T, body, dir string, roots ...string) map[string]Finding {
	t.Helper()
	p, err := ParseCompose([]byte(body), dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Finding{}
	for _, f := range Lint(p, LintOptions{AllowedRoots: append([]string{dir}, roots...)}) {
		out[f.ID] = f
	}
	return out
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// Docker follows symlinks in a bind source, so a link inside the worktree
// must not smuggle in the engine socket, the host root or any other path.
func TestLintResolvesSymlinkedBindSources(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	symlink(t, "/var/run/docker.sock", filepath.Join(dir, "sock"))
	symlink(t, "/", filepath.Join(dir, "root"))
	symlink(t, "/etc", filepath.Join(dir, "etcdir"))
	symlink(t, other, filepath.Join(dir, "out"))
	symlink(t, "loop2", filepath.Join(dir, "loop1"))
	symlink(t, "loop1", filepath.Join(dir, "loop2"))
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink(t, "missing/../file.txt/x", filepath.Join(dir, "notdir"))
	symlink(t, "sub/new", filepath.Join(dir, "inside")) // dangling, but stays in the worktree
	body := `
services:
  web:
    image: nginx
    volumes:
      - ./sock:/var/run/docker.sock
      - ./root:/host
      - ./etcdir/passwd:/p
      - ./out/new/deeper:/o
      - ./loop1:/l
      - ./notdir:/n
      - ./not-yet/created:/fine
      - ./inside:/fine2
`
	ids := lintIDs(t, body, dir)
	want := map[string]string{
		"web/volumes/_var_run_docker.sock-*": "engine socket",
		"web/volumes/_host-*":                "host root",
		"web/volumes/_p-*":                   "/etc",
		"web/volumes/_o-*":                   "outside the worktree",
		"web/volumes/_l-*":                   "cannot be resolved",
		"web/volumes/_n-*":                   "cannot be resolved",
	}
	list := make([]Finding, 0, len(ids))
	for _, f := range ids {
		list = append(list, f)
	}
	for id, why := range want {
		f := findID(list, id)
		if f == nil || !strings.Contains(f.Reason, why) {
			t.Fatalf("%s: want %q, got %+v in %+v", id, why, f, ids)
		}
	}
	if len(ids) != len(want) {
		t.Fatalf("a missing source under the worktree is fine: %+v", ids)
	}
	if f := findID(list, "web/volumes/_host-*"); !strings.Contains(f.Reason, "through a symlink") {
		t.Fatalf("the reason names the symlink: %+v", f)
	}

	// An allowed root reached through a symlink still allows its real path.
	link := filepath.Join(t.TempDir(), "alias")
	symlink(t, other, link)
	if ids := lintIDs(t, "services:\n  web:\n    image: x\n    volumes: [\""+other+"/c:/c\"]\n", dir, link); len(ids) != 0 {
		t.Fatalf("%+v", ids)
	}
	if _, err := resolveSymlinks("relative/path"); err == nil {
		t.Fatal("a relative path cannot be resolved")
	}
}

// ADR 0018: allowing one finding never widens to another. A mount's ID
// follows what it mounts, so changing the source, target or mode behind
// an allowed finding is a new, blocked finding.
func TestLintMountIDsFollowTheMount(t *testing.T) {
	dir := t.TempDir()
	one := func(vol string) string {
		ids := lintIDs(t, "services:\n  web:\n    image: x\n    volumes: [\""+vol+"\"]\n", dir)
		if len(ids) != 1 {
			t.Fatalf("%s: %+v", vol, ids)
		}
		for id := range ids {
			return id
		}
		return ""
	}
	allowed := one("/opt/cache:/data:ro")
	if allowed != one("/opt/cache:/data:ro") {
		t.Fatal("the ID is stable for the same mount")
	}
	for _, worse := range []string{"/opt/cache:/data", "/var/run/docker.sock:/data:ro", "/opt/cache:/other:ro"} {
		if one(worse) == allowed {
			t.Fatalf("%s reuses the allowed ID %s", worse, allowed)
		}
	}
	// The same holds for a symlink whose target changes.
	symlink(t, "/opt/cache", filepath.Join(dir, "c"))
	before := one("./c:/data")
	if err := os.Remove(filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	symlink(t, "/var/run/docker.sock", filepath.Join(dir, "c"))
	if one("./c:/data") == before {
		t.Fatal("a retargeted symlink reuses the allowed ID")
	}
	// And for a named volume's device.
	named := func(dev string) string {
		ids := lintIDs(t, "services:\n  web:\n    image: x\nvolumes:\n  d:\n    driver_opts: {type: none, o: bind, device: "+dev+"}\n", dir)
		for id := range ids {
			return id
		}
		t.Fatalf("%s not flagged", dev)
		return ""
	}
	if named("/opt") == named("/var/lib/docker") {
		t.Fatal("a named volume's new device reuses the allowed ID")
	}
}

// Joining or borrowing another container (an agent's, another preview's,
// the proxy) is blocked; sharing within the project is fine.
func TestLintNamespacesWithinProjectAreFine(t *testing.T) {
	dir := t.TempDir()
	body := `
services:
  web:
    image: x
    network_mode: service:db
    pid: "service:db"
    ipc: shareable
    volumes_from: [db, "db:ro", "service:db:rw"]
    networks:
      back:
        aliases: [api]
  db:
    image: postgres
    networks: [back, default]
networks:
  back: {}
  ext:
    external: true
    name: shared-dev
`
	if ids := lintIDs(t, body, dir); len(ids) != 0 {
		t.Fatalf("%+v", ids)
	}
	// From `docker compose config -p <project>`, the default network
	// carries a computed icaro- name in the project's own namespace.
	own := NameFor(dir).Project
	js := `{"name":"` + own + `","services":{"web":{"image":"x","networks":{"default":null}}},
	"networks":{"default":{"name":"` + own + `_default"}}}`
	if ids := lintIDs(t, js, dir); len(ids) != 0 {
		t.Fatalf("%+v", ids)
	}
	// Ícaro's own override joins the routed service to icaro-preview; it
	// is generated after the lint and never linted, so a plain routed
	// compose file is clean.
	if ids := lintIDs(t, webCompose, dir); len(ids) != 0 {
		t.Fatalf("%+v", ids)
	}
}

// The raw-file fallback merges an override file; what it adds is linted.
func TestLintSeesOverrideAdditions(t *testing.T) {
	dir := t.TempDir()
	base, err := ParseCompose([]byte("services:\n  web:\n    image: x\n    networks: [default]\nnetworks:\n  default: {name: x}\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	over, err := ParseCompose([]byte(`
services:
  web:
    volumes_from: ["container:agent"]
    container_name: icaro-x-web
    networks:
      default:
        aliases: [icaro-other-0123abcd-web]
  side:
    image: x
    pid: container:agent
networks:
  default:
    external: {name: icaro-preview}
`), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range Lint(MergeProjects(base, over), LintOptions{AllowedRoots: []string{dir}}) {
		got[f.ID] = true
	}
	for _, id := range []string{
		"web/volumes_from/container_agent", "web/container_name/icaro-x-web",
		"web/networks/aliases/icaro-other-0123abcd-web", "side/pid/container_agent", "networks/icaro-preview",
	} {
		if !got[id] {
			t.Fatalf("missing %s in %v", id, got)
		}
	}
}

// resolvedSock is where /var/run/docker.sock really leads on this host
// (on Linux /var/run is usually a symlink to /run).
func resolvedSock(t *testing.T) string {
	t.Helper()
	r, err := resolveSymlinks("/var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
