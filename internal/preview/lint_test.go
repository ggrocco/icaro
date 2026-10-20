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
			var found *Finding
			for i := range findings {
				if findings[i].ID == fx.wantID {
					found = &findings[i]
				}
			}
			if found == nil {
				t.Fatalf("want finding %s, got %+v", fx.wantID, findings)
			}
			if found.Reason == "" || found.Field == "" {
				t.Fatalf("finding must explain itself: %+v", found)
			}
			if !strings.Contains(found.String(), "--finding "+fx.wantID) {
				t.Fatalf("finding must say how to allow it: %s", found)
			}
		})
	}
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
	if len(f) != 1 || f[0].ID != "web/volumes/3" {
		t.Fatalf("%+v", f)
	}
	if got := Blocked(f, map[string]bool{"web/volumes/3": true}); len(got) != 0 {
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
	if !ids["web/volumes/0"] || !ids["web/devices/_dev_fuse"] || len(ids) != 2 {
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
