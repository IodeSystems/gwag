package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// System keys.
//
// A system key is the credential one gwag cluster presents to another
// during a cutover (docs/cutover.md). It is minted, stored, listed,
// revoked and expired like an app key (app_keys.go): the same record
// in the same gwag-app-keys bucket, with Kind "system", no
// namespaces, and the form
//
//	gws_<id>_<secret>
//
// The prefix is distinct from gwk_ so a bearer is routed to the right
// check without a KV read, a leaked key names its kind, and neither
// kind can stand in for the other: the record kind is checked too.
//
// A system key authorizes only the admin routes in systemKeyOps and
// systemKeyRoutes (AdminMiddleware): replay verification, the
// cluster state export and import, and the event stream mirror
// (status, start, stop). Every other admin route, the control plane,
// the Schema Registry and the admin GraphQL fields refuse it. Only
// the admin role mints, lists and revokes system keys. There is no
// delegate path: the AdminAuthorizer and AppKeyAuthorizer delegates
// are never sent a system key.
//
// It is also the password of green's NATS leafnode connection to
// blue's mirror listener (cluster_mirror.go).

// appKeyKindSystem is appKeyRecord.Kind of a system key.
const appKeyKindSystem = "system"

// systemKeyPrefix starts every system key.
const systemKeyPrefix = "gws_"

// systemKeyOps are the admin huma operations a system key may call.
var systemKeyOps = map[string]bool{
	"exportClusterState": true,
	"importClusterState": true,
	"getClusterMirror":   true,
	"startClusterMirror": true,
	"stopClusterMirror":  true,
}

// systemKeyRoutes are the plain admin mux routes (not huma
// operations) a system key may call, as "METHOD /path" patterns.
// indexAdminRoutes adds them to the route table with the admin role.
var systemKeyRoutes = []string{
	"POST /admin/replay/verify",
}

// parseSystemKey splits a system key into its id. ok=false when tok
// is not in that form.
func parseSystemKey(tok string) (id string, ok bool) {
	return parseMintedKey(tok, systemKeyPrefix)
}

// createSystemKey mints a system key valid for ttl.
func (g *Gateway) createSystemKey(ctx context.Context, ttl time.Duration, note, by string) (appKeyRecord, string, error) {
	return g.mintKey(ctx, appKeyKindSystem, systemKeyPrefix, nil, ttl, note, by)
}

// checkSystemKey verifies a system key.
func (g *Gateway) checkSystemKey(ctx context.Context, tok string) (appKeyGrant, error) {
	return g.checkMintedKey(ctx, tok, systemKeyPrefix, appKeyKindSystem)
}

// serveSystemKey is AdminMiddleware for a bearer in the system-key
// form: a valid key on a route that admits system keys passes with
// the system role; any other route is 403.
func (g *Gateway) serveSystemKey(w http.ResponseWriter, r *http.Request, next http.Handler, tok string) {
	grant, err := g.checkSystemKey(r.Context(), tok)
	if err != nil {
		g.recordAdminAuth(r.Method, "denied_system_key", adminRoleAnonymous)
		if errors.Is(err, errAppKeyUnavailable) {
			http.Error(w, "system keys are not available yet (cluster store not ready)", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !g.adminRouteSystem(r) {
		g.recordAdminAuth(r.Method, "forbidden_system_key", adminRoleSystem)
		http.Error(w, "forbidden: a system key may call only replay verify, the cluster state export and import, and the event stream mirror", http.StatusForbidden)
		return
	}
	g.recordAdminAuth(r.Method, "ok_system_key", adminRoleSystem)
	ctx := withAdminSubject(withAdminRole(r.Context(), adminRoleSystem), grant.id)
	next.ServeHTTP(w, r.WithContext(ctx))
}
