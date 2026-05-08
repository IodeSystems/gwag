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
// dist/ is gitignored entirely — only generated content lives there.
// The tracked placeholder is `ui/fallback/index.html`; bin/build seeds
// (or recovers) dist/index.html from that fallback when pnpm run build
// fails or hasn't run yet, so //go:embed all:dist always finds at
// least one file. Run bin/build before `go build` on a fresh clone.
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
