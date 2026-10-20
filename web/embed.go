// Package web embeds the built UI (web/dist). Build it with `make web`;
// without a build the daemon serves dist/README.txt instead.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// Built reports whether dist holds a real build.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}
