//go:build !embed_spa

// Package web normally embeds the built SPA, but without the embed_spa build tag
// it provides a no-op so the backend compiles even when web/dist is absent
// (backend-only dev, CI, or before `npm run build`).
package web

import "io/fs"

// FS reports that no SPA is embedded. Build with `-tags embed_spa` (after
// `npm run build`) to embed web/dist.
func FS() (fs.FS, bool) { return nil, false }
