package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// Admin browser sessions.
//
// A browser holds a session cookie instead of a bearer. The gateway
// does not know how the cookie was minted: an AdminSessionAuthenticator
// (gw/oidc is one) reads it and names the subject and role. The
// gateway decides when a cookie is accepted at all (adminSessionAllowed)
// and stores what every node must share: the key that signs cookies and
// the per-subject revocation cutoffs (admin_session_store.go).
//
// A bearer always wins: a request that presents one is authenticated
// by the bearer path alone, cookie or not.

// AdminSessionRole is the role an admin session grants.
//
// Stability: experimental
type AdminSessionRole string

const (
	// AdminSessionAdmin grants the admin role.
	AdminSessionAdmin AdminSessionRole = "admin"
	// AdminSessionDev grants the dev role.
	AdminSessionDev AdminSessionRole = "dev"
)

// AdminSession is who holds a valid session cookie.
//
// Stability: experimental
type AdminSession struct {
	// Subject names the person (an email). Recorded as the audit
	// identity ("admin:alice@example.com").
	Subject string
	Role    AdminSessionRole
}

// AdminSessionAuthenticator reads the session a browser request
// carries. ok is false when there is none, or it is not valid
// (signature, expiry, revocation, no role). Called on every admin
// request that presents no bearer.
//
// Stability: experimental
type AdminSessionAuthenticator interface {
	AdminSession(r *http.Request) (AdminSession, bool)
}

// WithAdminSession accepts browser session cookies on admin routes,
// read by a. AdminMiddleware and the admin_events subscription consult
// it when a request presents no bearer. Cookie-authenticated requests
// are refused unless they come from the gateway's own origin: a write
// (any method but GET and HEAD) needs `Sec-Fetch-Site: same-origin`, a
// WebSocket upgrade an Origin whose host is the request host.
//
// The admin_* GraphQL fields reach /admin/* through the self-ingested
// admin OpenAPI source; forward AdminForwardHeaders on it, or a
// cookie-authenticated GraphQL call reaches /admin/* as anonymous.
//
// Stability: experimental
func WithAdminSession(a AdminSessionAuthenticator) Option {
	return func(cfg *config) { cfg.adminSession = a }
}

// AdminForwardHeaders are the headers to forward on the self-ingested
// admin OpenAPI source (ForwardHeaders(AdminForwardHeaders...)): the
// bearer, and the session cookie with the header the CSRF check reads.
//
// Stability: experimental
var AdminForwardHeaders = []string{"Authorization", "Cookie", "Sec-Fetch-Site"}

// adminSessionRole maps a session role to the admin role. Anything
// else is no role.
func adminSessionRole(r AdminSessionRole) (adminRole, bool) {
	switch r {
	case AdminSessionAdmin:
		return adminRoleAdmin, true
	case AdminSessionDev:
		return adminRoleDev, true
	}
	return adminRoleAnonymous, false
}

// adminSessionOf resolves the session on r. found is false when no
// authenticator is set or r carries no valid session. refused is
// non-empty when a valid session came on a request the CSRF rule
// does not accept; the request must then fail, not fall back to
// anonymous.
func (g *Gateway) adminSessionOf(r *http.Request) (role adminRole, subject string, found bool, refused string) {
	if r == nil || g.cfg.adminSession == nil {
		return adminRoleAnonymous, "", false, ""
	}
	s, ok := g.cfg.adminSession.AdminSession(r)
	if !ok {
		return adminRoleAnonymous, "", false, ""
	}
	role, ok = adminSessionRole(s.Role)
	if !ok {
		return adminRoleAnonymous, "", false, ""
	}
	if why := adminSessionAllowed(r); why != "" {
		return adminRoleAnonymous, "", true, why
	}
	return role, sanitizeSubject(s.Subject), true, ""
}

// adminSessionAllowed is the CSRF rule for cookie-authenticated
// requests; "" accepts. The cookie is SameSite=Lax, so a browser sends
// it on a top-level GET from another site; a GET there is a read.
//
//   - WebSocket upgrade: Origin present and its host equal to the
//     request host (or the first X-Forwarded-Host). Browsers do not
//     send Sec-Fetch-Site reliably on WebSocket handshakes, and a page
//     cannot forge Origin.
//   - GET, HEAD: accepted.
//   - Anything else: Sec-Fetch-Site must be same-origin. A missing
//     header is refused: every browser that sends cookies sends it, and
//     a client that is not a browser should present a bearer.
func adminSessionAllowed(r *http.Request) string {
	if isWebSocketUpgrade(r) {
		o := r.Header.Get("Origin")
		if o == "" {
			return "websocket without Origin"
		}
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return "websocket with a bad Origin"
		}
		if strings.EqualFold(u.Host, r.Host) {
			return ""
		}
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && strings.EqualFold(u.Host, strings.TrimSpace(strings.Split(fh, ",")[0])) {
			return ""
		}
		return "websocket Origin " + u.Host + " is not this host"
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return ""
	}
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "same-origin":
		return ""
	case "":
		return "no Sec-Fetch-Site header"
	default:
		return "Sec-Fetch-Site " + site
	}
}
