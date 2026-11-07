// Package torrentd embeds the built web UI (web/dist) into the binary (§3).
//
// A placeholder web/dist/index.html is committed, so the binary builds before
// the frontend has been built.
package torrentd

import (
	"embed"
	"io/fs"
)

//go:embed web/dist
var webDist embed.FS

// WebFS returns the embedded SPA, rooted at web/dist (index.html at the top).
func WebFS() fs.FS {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		panic(err) // unreachable: "web/dist" is a valid, embedded path
	}
	return sub
}
