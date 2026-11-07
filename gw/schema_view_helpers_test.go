package gateway

import "net/http"

// schemaAsAdmin wraps a schema export handler so every request carries
// g's admin token: the full view, which tests of the export content
// expect. Without a token the export shows public services only
// (schema_view.go).
func schemaAsAdmin(g *Gateway, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(RoleTokenHeader, g.AdminTokenHex())
		h.ServeHTTP(w, r)
	})
}
