package gateway

import (
	"io/fs"
	"net/http"
	"strings"
)

// UIHandler serves a single-page application from uiFS. Patterns:
//   - exact-match files (e.g. `/assets/index-XXX.js`) are served directly
//     so the browser gets the correct Content-Type + ETag.
//   - everything else falls through to `index.html` so client-side
//     routing (TanStack Router, React Router, etc.) takes over.
//
// Pair with API routes mounted under a distinct prefix (e.g. `/api/`)
// and an explicit JSON-404 for unmatched API paths — otherwise an
// unknown `/api/foo` would render the SPA, masking misroutes. See the
// example gateway for the canonical wiring.
//
// Caller passes either an embed.FS subtree (via fs.Sub) or an
// os.DirFS for dev. Empty filesystems are accepted; requests get a
// plain 404 until something is embedded.
//
// Stability: stable
func UIHandler(uiFS fs.FS) http.Handler {
	if uiFS == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "ui not embedded", http.StatusNotFound)
		})
	}
	fileServer := http.FileServerFS(uiFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(uiFS, path); err != nil {
			// Not an asset → SPA fallback.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
