package preview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func mustProject(t *testing.T, body string) *Project {
	t.Helper()
	p, err := ParseCompose([]byte(body), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSelectRoute(t *testing.T) {
	cases := []struct {
		body    string
		want    Route
		wantErr string
	}{
		{"services:\n  web: {image: x, labels: {icaro.preview.port: \"3000\"}}\n  db: {image: y, ports: [\"80\"]}\n", Route{"web", 3000}, ""},
		{"services:\n  web: {image: x, labels: [\"icaro.preview.port=5173\"]}\n", Route{"web", 5173}, ""},
		{"services:\n  web: {image: x, ports: [\"8080:80\"]}\n  db: {image: y, expose: [\"5432\"]}\n", Route{"web", 80}, ""},
		{"services:\n  a: {image: x, expose: [\"80\"]}\n  b: {image: y, ports: [\"80\"]}\n", Route{}, "all use port 80"},
		{"services:\n  a: {image: x, ports: [\"3000\"]}\n", Route{}, "add `labels"},
		{"services:\n  a: {image: x, labels: {icaro.preview.port: \"1\"}}\n  b: {image: y, labels: {icaro.preview.port: \"2\"}}\n", Route{}, "several services"},
		{"services:\n  a: {image: x, labels: {icaro.preview.port: \"http\"}}\n", Route{}, "is not a port"},
	}
	for i, c := range cases {
		got, err := SelectRoute(mustProject(t, c.body))
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("case %d: err = %v, want %q", i, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("case %d: %+v %v", i, got, err)
		}
	}
	_, err := SelectRoute(mustProject(t, "services:\n  a: {image: x}\n"))
	if !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
}

func TestNamingAvoidsCollisions(t *testing.T) {
	a, b := NameFor("/w/proj-1/feature"), NameFor("/other/feature")
	if a.Slug != b.Slug || a.Project == b.Project || a.Alias == b.Alias {
		t.Fatalf("same slug must not collide: %+v %+v", a, b)
	}
	if NameFor("/w/proj-1/feature") != a {
		t.Fatal("naming must be deterministic")
	}
	taken := map[string]bool{}
	ha := Hostname(a, "", func(h string) bool { return taken[h] })
	taken[ha] = true
	hb := Hostname(b, "", func(h string) bool { return taken[h] })
	if ha != "feature.localhost" || hb != "feature-"+b.Hash+".localhost" {
		t.Fatalf("%s %s", ha, hb)
	}
	if Hostname(a, "test", nil) != "feature.test" {
		t.Fatal("domain")
	}
	for in, want := range map[string]string{"Feature/Login_Page!": "feature-login-page", "___": "preview", strings.Repeat("a", 40): strings.Repeat("a", 30)} {
		if got := Slugify(in); got != want {
			t.Fatalf("Slugify(%q) = %q", in, got)
		}
	}
}

func TestOverride(t *testing.T) {
	p := mustProject(t, "services:\n  web: {image: x, ports: [\"80\"]}\n  api: {image: y, networks: [back]}\nnetworks: {back: {}}\n")
	n := NameFor("/w/app")
	out, err := Override(p, Route{"web", 80}, n, OverrideOptions{InstanceID: "ID1", Hostname: "app.localhost"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	nets := web["networks"].(map[string]any)
	if _, ok := nets["default"]; !ok {
		t.Fatal("implicit default network must be kept")
	}
	if nets[Network].(map[string]any)["aliases"].([]any)[0] != n.Alias {
		t.Fatal("alias")
	}
	if _, ok := web["environment"]; ok {
		t.Fatal("polling env only on macOS")
	}
	ext := doc["networks"].(map[string]any)[Network].(map[string]any)
	if ext["external"] != true {
		t.Fatal("preview network must be external")
	}
	// A service with its own networks keeps them and does not get default.
	out, _ = Override(p, Route{"api", 80}, n, OverrideOptions{MacOS: true})
	_ = yaml.Unmarshal(out, &doc)
	api := doc["services"].(map[string]any)["api"].(map[string]any)
	if _, ok := api["networks"].(map[string]any)["default"]; ok {
		t.Fatal("explicit networks must not gain default")
	}
	env := api["environment"].(map[string]any)
	if env["CHOKIDAR_USEPOLLING"] != "true" || env["WATCHPACK_POLLING"] != "true" {
		t.Fatalf("macOS polling env: %v", env)
	}
	if _, err := Override(mustProject(t, "services:\n  w: {image: x, network_mode: none}\n"), Route{"w", 80}, n, OverrideOptions{}); err == nil {
		t.Fatal("network_mode cannot join the preview network")
	}
	if _, err := Override(p, Route{"nope", 80}, n, OverrideOptions{}); err == nil {
		t.Fatal("unknown service")
	}
	rc, err := TraefikConfig(n, "app.localhost", Route{"web", 80})
	if err != nil || !strings.Contains(string(rc), "Host(`app.localhost`)") || !strings.Contains(string(rc), "http://"+n.Alias+":80") {
		t.Fatalf("%s %v", rc, err)
	}
}

func TestConfigHashIgnoresCodeEdits(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services: {web: {image: x}}\n")
	write("index.html", "v1")
	h1, _ := ConfigHash(dir)
	write("index.html", "v2")
	if h2, _ := ConfigHash(dir); h2 != h1 {
		t.Fatal("code edit changed the config hash")
	}
	for _, f := range []string{".env", "package-lock.json", "requirements-dev.txt", "compose.override.yaml"} {
		before, _ := ConfigHash(dir)
		write(f, "x")
		if after, _ := ConfigHash(dir); after == before {
			t.Fatalf("%s did not change the hash", f)
		}
	}
}
