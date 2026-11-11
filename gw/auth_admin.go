package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// adminTokenFilename is the leaf name under <data-dir> where the boot
// token is persisted. Hex-encoded so cat'ing the file gives a usable
// Authorization value.
const adminTokenFilename = "admin-token"

// AdminToken returns the gateway's admin bearer token. Operators log
// this at boot so they (and the UI) can present it as
// `Authorization: Bearer <hex>` to /admin/* and admin_* mutations.
//
// The token is the unconditional fallback — independent of any
// pluggable admin authorizer. It does NOT authenticate services
// calling each other through the gateway; that's a separate concern.
//
// Stability: stable
func (g *Gateway) AdminToken() []byte { return g.cfg.adminToken }

// AdminTokenHex is AdminToken hex-encoded — the form a client presents
// in `Authorization: Bearer <hex>` and what we persist on disk.
//
// Stability: stable
func (g *Gateway) AdminTokenHex() string { return hex.EncodeToString(g.cfg.adminToken) }

// AdminMiddleware wraps next in admin-auth verification intended for
// /admin/* HTTP routes. It resolves the caller's role — anonymous
// (no bearer), dev (the dev token) or admin (the boot token) — and
// lets the request through when the role is enough for the route:
//
//   - Routes AdminHumaRouter registered carry their own requirement
//     (admin_roles.go): a few reads are open to anonymous callers and
//     answer with public services only; the other reads need dev; the
//     external-dependent writes need dev; every other write needs
//     admin.
//   - Any other route: GET/HEAD/OPTIONS are open; other methods need
//     admin. Paths registered via WithDestructiveReads need admin for
//     every method.
//
// A presented bearer is checked even on an open read (a wrong one is
// 401), so a client learns at once that its token is stale. The
// bearer is checked by:
//
//  1. Registered AdminAuthorizer delegate at "_admin_auth/v1", if any.
//     Consulted for every request that needs more than anonymous and
//     for every request that presents a bearer. OK accepts with the
//     role the delegate names (unset = admin); DENIED rejects without
//     falling through. Any other code (UNAVAILABLE, NOT_CONFIGURED,
//     UNSPECIFIED, transport err) falls through to step 2.
//  2. Boot-token, then dev-token Bearer check (constant time; the
//     unconditional fallback).
//
// A role below the route's requirement gets 401 when it is anonymous
// and 403 otherwise. The role rides on the request context.
//
// With WithAdminSession, a request that presents no bearer is first
// checked for a session cookie (admin_session.go). A valid session
// sets the role and subject, ahead of the delegate; a valid session on
// a request the CSRF rule refuses is 403. No valid session: the steps
// above, as if no cookie was sent.
//
// A bearer in the session-bearer form (gwo_, AdminSessionBearerPrefix)
// is checked by the session authenticator only, never the delegate.
//
// A bearer in the system-key form (gws_, system_keys.go) is checked
// against the gateway's system keys only, never the delegate: on the
// routes a system key may call it passes with the system role; on
// every other route a valid one is 403.
//
// Stability: stable
func (g *Gateway) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := g.adminRouteNeed(r)
		bearer := bearerFromRequest(r)
		if _, ok := parseSystemKey(bearer); ok {
			g.serveSystemKey(w, r, next, bearer)
			return
		}
		var (
			role           adminRole
			subject        string
			outcome        string
			status         int
			sessionFound   bool
			sessionRefusal string
		)
		if bearer == "" {
			role, subject, sessionFound, sessionRefusal = g.adminSessionOf(r)
		}
		if need == adminRoleAnonymous && bearer == "" && !sessionFound {
			next.ServeHTTP(w, r.WithContext(withAdminRole(r.Context(), adminRoleAnonymous)))
			return
		}
		switch {
		case sessionRefusal != "":
			g.recordAdminAuth(r.Method, "denied_session_csrf", adminRoleAnonymous)
			http.Error(w, "forbidden: cross-site request with a session cookie ("+sessionRefusal+")", http.StatusForbidden)
			return
		case sessionFound:
			outcome = "ok_session"
		default:
			role, subject, outcome, status = g.authenticateAdminSubject(r.Context(), bearer, r.Method, r.URL.Path, r.RemoteAddr, need > adminRoleAnonymous)
		}
		if status == 0 && role < need {
			outcome = "forbidden_role"
			status = http.StatusForbidden
			if role == adminRoleAnonymous {
				outcome = "denied_bearer"
				status = http.StatusUnauthorized
			}
		}
		g.recordAdminAuth(r.Method, outcome, role)
		switch status {
		case 0:
			ctx := withAdminSubject(withAdminRole(r.Context(), role), subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		case http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case http.StatusForbidden:
			http.Error(w, "forbidden: this route needs the "+need.String()+" role", http.StatusForbidden)
		default:
			http.Error(w, "admin token not configured", status)
		}
	})
}

// authenticateAdmin resolves a bearer to a role. status is 0 when the
// caller may proceed with role (anonymous when no bearer was sent),
// otherwise the HTTP status to answer with. outcome is the
// admin_auth_total label. needAuth is true when the route needs more
// than anonymous: the delegate is then consulted even without a
// bearer (it decides about anonymous calls), as before roles existed.
func (g *Gateway) authenticateAdmin(ctx context.Context, bearer, method, path, remoteAddr string, needAuth bool) (role adminRole, outcome string, status int) {
	role, _, outcome, status = g.authenticateAdminSubject(ctx, bearer, method, path, remoteAddr, needAuth)
	return role, outcome, status
}

// authenticateAdminSubject is authenticateAdmin that also returns the
// subject the AdminAuthorizer delegate named for the caller ("" for a
// token match or a delegate that names none).
func (g *Gateway) authenticateAdminSubject(ctx context.Context, bearer, method, path, remoteAddr string, needAuth bool) (role adminRole, subject, outcome string, status int) {
	if _, ok := parseSystemKey(bearer); ok {
		// A system key is checked only by AdminMiddleware, on the
		// routes it may call; never sent to the delegate.
		return adminRoleAnonymous, "", "denied_system_key", http.StatusUnauthorized
	}
	if strings.HasPrefix(bearer, AdminSessionBearerPrefix) {
		// A session bearer is checked by the session authenticator
		// only; never sent to the delegate.
		if role, subject, ok := g.adminSessionBearerOf(ctx, bearer); ok {
			return role, subject, "ok_session_bearer", 0
		}
		return adminRoleAnonymous, "", "denied_session_bearer", http.StatusUnauthorized
	}
	if needAuth || bearer != "" {
		switch outcome, delegateRole, _, delegateSubject := g.consultAdminDelegate(ctx, bearer, method, path, remoteAddr); outcome {
		case adminDelegateAccept:
			return delegateRole, delegateSubject, "ok_delegate", 0
		case adminDelegateReject:
			return adminRoleAnonymous, "", "denied_delegate", http.StatusUnauthorized
		}
	}
	role, outcome, status = g.authenticateAdminToken(bearer, needAuth)
	return role, "", outcome, status
}

// authenticateAdminToken is the boot-token / dev-token step of
// authenticateAdmin.
func (g *Gateway) authenticateAdminToken(bearer string, needAuth bool) (role adminRole, outcome string, status int) {
	tok := g.cfg.adminToken
	if len(tok) == 0 {
		if !needAuth && bearer == "" {
			return adminRoleAnonymous, "", 0
		}
		return adminRoleAnonymous, "no_token_configured", http.StatusInternalServerError
	}
	if bearer == "" {
		return adminRoleAnonymous, "denied_bearer", 0
	}
	got, err := decodeAdminTokenString(bearer)
	if err != nil {
		return adminRoleAnonymous, "denied_bearer", http.StatusUnauthorized
	}
	// Both comparisons run every time, so the response time does not
	// tell which token was close.
	isAdmin := tokenEqual(got, tok)
	isDev := tokenEqual(got, g.DevToken())
	switch {
	case isAdmin:
		return adminRoleAdmin, "ok_bearer", 0
	case isDev:
		return adminRoleDev, "ok_bearer", 0
	}
	return adminRoleAnonymous, "denied_bearer", http.StatusUnauthorized
}

func isAdminPublicMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// isDestructiveRead reports whether path was opted into admin gating via
// WithDestructiveReads. Exact match, or prefix when the registered entry
// ends with "*".
func (g *Gateway) isDestructiveRead(path string) bool {
	for _, p := range g.cfg.destructiveReads {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(path, p[:len(p)-1]) {
				return true
			}
		} else if path == p {
			return true
		}
	}
	return false
}

// tokenEqual compares a presented token with want in constant time.
// An empty want never matches.
func tokenEqual(got, want []byte) bool {
	if len(want) == 0 {
		return false
	}
	if subtle.ConstantTimeEq(int32(len(got)), int32(len(want))) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// decodeAdminTokenString accepts hex (the canonical form, what we
// emit and persist). Falls back to raw bytes for callers who set
// WithAdminToken to a non-hex string.
func decodeAdminTokenString(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	if s == "" {
		return nil, errors.New("empty bearer")
	}
	return []byte(s), nil
}

// loadOrGenerateAdminToken returns the gateway's boot token. If dir is
// set and a token file already exists, it's loaded; otherwise a fresh
// 32-byte token is generated, persisted (when dir != ""), and
// returned. dir == "" means in-memory only (regenerated each boot).
func loadOrGenerateAdminToken(dir string) ([]byte, error) {
	return loadOrGenerateToken(dir, adminTokenFilename)
}

// loadOrGenerateToken is loadOrGenerateAdminToken for any token file
// under dir (the admin token, the dev token).
func loadOrGenerateToken(dir, name string) ([]byte, error) {
	if dir == "" {
		return generateAdminToken()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s dir: %w", name, err)
	}
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err == nil {
		s := strings.TrimSpace(string(b))
		decoded, derr := hex.DecodeString(s)
		if derr != nil {
			return nil, fmt.Errorf("%s at %s: %w", name, path, derr)
		}
		if len(decoded) == 0 {
			return nil, fmt.Errorf("%s at %s: empty", name, path)
		}
		return decoded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s at %s: %w", name, path, err)
	}
	tok, err := generateAdminToken()
	if err != nil {
		return nil, err
	}
	if err := writeTokenFile(path, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// writeTokenFile persists tok hex-encoded at path (0600), replacing
// the file atomically.
func writeTokenFile(path string, tok []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(tok)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func generateAdminToken() ([]byte, error) {
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, fmt.Errorf("admin token rand: %w", err)
	}
	return tok, nil
}

// AdminTokenPath returns the on-disk path the gateway will persist its
// boot token to (or "" if no data dir was configured). Useful for the
// boot log.
//
// Stability: stable
func (g *Gateway) AdminTokenPath() string {
	if g.cfg.adminDataDir == "" {
		return ""
	}
	return filepath.Join(g.cfg.adminDataDir, adminTokenFilename)
}

// isAdminAuth reports whether ctx was authenticated by AdminMiddleware
// with the admin role.
func isAdminAuth(ctx context.Context) bool {
	r, ok := adminRoleFromContext(ctx)
	return ok && r == adminRoleAdmin
}
