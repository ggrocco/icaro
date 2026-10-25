package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseCompose feeds arbitrary documents through the compose parser
// and the lint (05-engineering: fuzz parsers). Neither may panic; the lint
// runs on every document the parser accepts, because it is the security
// boundary for untrusted compose files. Seeds: the s6 fixtures and every
// malicious lint fixture.
func FuzzParseCompose(f *testing.F) {
	files, err := filepath.Glob("testdata/s6/*/*.y*ml")
	if err != nil {
		f.Fatal(err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p) //nolint:gosec // test fixture
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	raw, err := os.ReadFile("testdata/malicious.txt")
	if err != nil {
		f.Fatal(err)
	}
	for _, sec := range strings.Split(string(raw), "\n-- ")[1:] {
		_, body, _ := strings.Cut(sec, " --\n")
		f.Add([]byte(body))
	}
	f.Add([]byte(`{"services":{"web":{"image":"nginx","ports":["80:80"]}}}`))
	f.Add([]byte("services:\n  web:\n    volumes: [\"/:/host\"]\n"))
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseCompose(data, dir)
		if err != nil {
			return
		}
		_ = p.ServiceNames()
		_ = Lint(p, LintOptions{AllowedRoots: []string{dir}})
	})
}
