package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "docs/a.md", "# Title\n\n## Open items\n\n## Open items\n\n## `icaro doctor`\n\n<a id=\"custom\"></a>\n")
	write(t, dir, "README.md", strings.Join([]string{
		"[ok](docs/a.md) [ok](docs/a.md#open-items) [dup](docs/a.md#open-items-1) [code](docs/a.md#icaro-doctor)",
		"[id](docs/a.md#custom) [dir](docs/) [ext](https://example.com/x) [mail](mailto:a@b) [self](#readme-heading)",
		"[missing](docs/nope.md) [badanchor](docs/a.md#nope) [abs](/docs/a.md)",
		"`[ignored](in-code.md)`",
		"```",
		"[ignored](in-fence.md)",
		"```",
		"[ref]: docs/gone.md",
		"## README heading",
	}, "\n"))
	write(t, dir, "site/index.html", `<a href="../README.md">r</a><img src="missing.png" alt="x"><a href="https://x">x</a>`)
	write(t, dir, "node_modules/x.md", "[skip](nowhere.md)")
	problems, err := check(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"docs/nope.md: target does not exist", "docs/a.md#nope: no heading", "/docs/a.md: absolute", "docs/gone.md: target does not exist", "missing.png: target does not exist"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if len(problems) != 5 {
		t.Fatalf("want 5 problems, got:\n%s", joined)
	}
	var buf bytes.Buffer
	report(&buf, problems)
	if !strings.Contains(buf.String(), "5 broken link(s)") {
		t.Fatal(buf.String())
	}
	if Slug("ADR 0005 — Credentials: persistent home + egress") != "adr-0005--credentials-persistent-home--egress" {
		t.Fatal(Slug("ADR 0005 — Credentials: persistent home + egress"))
	}
}

func TestLandingPageDiscovery(t *testing.T) {
	const home = "https://ggrocco.github.io/icaro/"
	root := filepath.Join("..", "..")
	html, err := os.ReadFile(filepath.Join(root, "site", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	canonical := regexp.MustCompile(`<link\s+rel="canonical"\s+href="([^"]+)"\s*>`).FindAllStringSubmatch(string(html), -1)
	if len(canonical) != 1 || canonical[0][1] != home {
		t.Fatalf("expected one canonical link to %s, got %v", home, canonical)
	}

	sitemap, err := os.ReadFile(filepath.Join(root, "site", "sitemap.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var published struct {
		XMLName xml.Name `xml:"http://www.sitemaps.org/schemas/sitemap/0.9 urlset"`
		URLs    []string `xml:"url>loc"`
	}
	if err := xml.Unmarshal(sitemap, &published); err != nil {
		t.Fatal(err)
	}
	if len(published.URLs) != 1 || published.URLs[0] != home {
		t.Errorf("expected only %s in the sitemap, got %v", home, published.URLs)
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readme, []byte("]("+home+")")) {
		t.Errorf("README is missing a link to %s", home)
	}
}
