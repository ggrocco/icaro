//go:build integration

package preview

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestComposeSamples is spike S6: real project layouts in
// testdata/s6, normalized by `docker compose config` exactly as the
// manager does, then put through port discovery. A layout either routes
// to the expected service or is refused with a message that names the
// fix; it is never routed by a guess.
func TestComposeSamples(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	for _, tc := range []struct {
		dir   string
		route string // "service:port", or "" when refused
		want  string // substring of the refusal
	}{
		{"nginx-static", "web:80", ""},
		{"wordpress", "wordpress:80", ""},
		{"proxy-app-db", "proxy:80", ""},
		{"expose-only", "web:80", ""},
		{"long-syntax", "web:80", ""},
		{"host-ip", "web:80", ""},
		{"vite-labelled", "web:5173", ""},
		{"label-list", "web:3000", ""},
		{"env-port", "web:4000", ""},        // .env interpolation
		{"profiles", "web:80", ""},          // inactive profiles are not candidates
		{"include", "web:80", ""},           // top-level include
		{"extends", "web:80", ""},           // extends from another file
		{"port-range", "proxy:80", ""},      // ranges are expanded
		{"override", "web:80", ""},          // ports from compose.override.yaml
		{"legacy-override", "app:3000", ""}, // label from docker-compose.override.yml
		{"vite-spa", "", "web (5173)"},      // one dev server, not on 80: no guess
		{"django", "", "web (8000)"},        //
		{"rails-db-redis", "", "db (5432), redis (6379), web (3000)"},
		{"frontend-api", "", "api (8080), frontend (3000)"},
		{"two-on-80", "", "docs, site all use port 80"},
		{"udp-80", "", "app (3000)"},                                     // a UDP port 80 is not HTTP
		{"build-only", "", "no service publishes or exposes a TCP port"}, // Dockerfile EXPOSE is not visible
	} {
		t.Run(tc.dir, func(t *testing.T) {
			dir, _ := filepath.Abs(filepath.Join("testdata", "s6", tc.dir))
			files := ComposeFiles(dir)
			data, err := CLI{}.Config(context.Background(), NameFor(dir).Project, dir, files)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ParseCompose(data, dir)
			if err != nil {
				t.Fatal(err)
			}
			r, err := SelectRoute(p)
			if tc.route != "" {
				if err != nil || r.Service+":"+strconv.Itoa(r.Port) != tc.route {
					t.Fatalf("route = %+v %v, want %s", r, err, tc.route)
				}
				return
			}
			if !errors.Is(err, ErrNoRoute) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), PortLabel) {
				t.Fatalf("want a refusal naming %q and the label, got %+v %v", tc.want, r, err)
			}
		})
	}
}
