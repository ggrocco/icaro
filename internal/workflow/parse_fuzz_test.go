package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FuzzParse feeds arbitrary documents through workflow decoding, schema
// validation and the semantic rules (05-engineering: fuzz parsers and
// schema). It must never panic, whatever the input. Seeds: the shipped
// examples plus a few hand-written edge cases.
func FuzzParse(f *testing.F) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil {
		f.Fatal(err)
	}
	if len(files) == 0 {
		f.Fatal("no example workflows to seed from")
	}
	for _, p := range files {
		b, err := os.ReadFile(p) //nolint:gosec // test fixture
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{
		"", "null", "[]", "name: x", "{name: x, steps: [{id: a, run: echo}]}",
		"name: x\non: {cron: '* * * * *'}\nsteps:\n  - id: a\n    needs: [a]\n    run: echo ${{ steps.a.outputs.x }}\n",
		"name: x\nsteps:\n  - id: a\n    uses: http.request\n    with: {url: '${{ secrets.X }}'}\n    timeout: 99999h\n",
	} {
		f.Add([]byte(s))
	}
	opts := Options{MaxTimeout: 30 * time.Minute, SecretExists: func(string) bool { return false }}
	f.Fuzz(func(t *testing.T, src []byte) {
		_, _, _ = Parse(src, opts)
		_, _, _ = Parse(src, Options{})
	})
}
