//go:build embed_spa

// Package web embeds the built SPA (web/dist) into the binary. This file is only
// compiled with `-tags embed_spa`, after `npm run build` has produced web/dist.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded SPA filesystem rooted at dist, and true.
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
