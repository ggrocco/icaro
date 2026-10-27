package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse feeds arbitrary documents through manifest decoding and
// validation (05-engineering: fuzz parsers and schema). Manifests come
// from third-party packs, so parsing must never panic. Seeds: the
// built-in manifests.
func FuzzParse(f *testing.F) {
	files, err := filepath.Glob("builtin/*.yaml")
	if err != nil {
		f.Fatal(err)
	}
	if len(files) == 0 {
		f.Fatal("no built-in manifests to seed from")
	}
	for _, p := range files {
		b, err := os.ReadFile(p) //nolint:gosec // test fixture
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, s := range []string{"", "null", "[]", "name: x", "name: x\nevents: {a: {}}\nactions: {b: {request: {url: 'http://[::1'}}}\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		_, _, _ = Parse(src)
	})
}
