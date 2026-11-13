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
// dist/ is gitignored except the empty placeholder dist/.gitkeep, which
// keeps //go:embed all:dist compiling in a module-proxy build
// (`go install .../gw/cmd/gwag@version`), where dist/ is never built.
// vite.config.ts restores .gitkeep after each build, so a build leaves
// the tree clean. FS() serves dist/ when it holds index.html and the
// tracked ui/fallback/index.html otherwise. bin/build seeds
// dist/index.html from the fallback when the Vite build fails.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var raw embed.FS

//go:embed fallback
var fallback embed.FS

// FS returns an fs.FS rooted at the UI bundle, suitable to pass to
// gateway.UIHandler. It is the built bundle when dist/index.html was
// embedded, the placeholder page otherwise. Never nil.
func FS() fs.FS {
	if sub, err := fs.Sub(raw, "dist"); err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			return sub
		}
	}
	sub, err := fs.Sub(fallback, "fallback")
	if err != nil {
		// Embed contract: fallback/ exists at build time.
		panic("ui: fs.Sub(fallback): " + err.Error())
	}
	return sub
}
