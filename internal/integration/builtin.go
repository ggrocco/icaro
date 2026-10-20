package integration

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// Pack names with special meaning.
const (
	PackBuiltin = "builtin"
	PackLocal   = "local"
)

// builtinFiles returns the embedded manifests as name -> source.
func builtinFiles() map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if !isManifestFile(e.Name()) {
			continue
		}
		b, err := builtinFS.ReadFile(path.Join("builtin", e.Name()))
		if err != nil {
			panic(err)
		}
		out[e.Name()] = b
	}
	return out
}

// BuiltinNames lists the integrations shipped in the binary.
func BuiltinNames() []string {
	var out []string
	for f := range builtinFiles() {
		out = append(out, strings.TrimSuffix(strings.TrimSuffix(f, ".yaml"), ".yml"))
	}
	sort.Strings(out)
	return out
}

func isManifestFile(name string) bool {
	return (strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) && !strings.HasPrefix(name, ".")
}
