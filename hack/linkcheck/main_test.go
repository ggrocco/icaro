package main

import (
	"bytes"
	"os"
	"path/filepath"
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
