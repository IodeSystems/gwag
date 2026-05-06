// Package ui embeds the compiled SPA bundle (`dist/`) as an
// embed.FS so the gateway binary can serve the admin UI without
// any external static directory. Pair with gateway.UIHandler to
// mount it at the SPA root.
//
// Build flow:
//
//	cd ui && pnpm install && pnpm run build   # populates dist/
//	go build ./...                              # embeds dist/* into the binary
//
// On a fresh clone before the first `pnpm run build`, dist/ contains
// only a placeholder index.html that asks the developer to run the
// UI build. The Go build itself succeeds either way — that placeholder
// is committed to satisfy the embed pattern.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var raw embed.FS

// FS returns an fs.FS rooted at the dist directory (the build
// output), suitable to pass to gateway.UIHandler. Returns a non-nil
// FS even when only the placeholder is present.
func FS() fs.FS {
	sub, err := fs.Sub(raw, "dist")
	if err != nil {
		// Embed contract: dist/ exists at build time, so Sub cannot
		// fail. Panic to surface broken builds early.
		panic("ui: fs.Sub(dist): " + err.Error())
	}
	return sub
}
