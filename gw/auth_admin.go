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
func (g *Gateway) AdminToken() []byte { return g.cfg.adminToken }

// AdminTokenHex is AdminToken hex-encoded — the form a client presents
// in `Authorization: Bearer <hex>` and what we persist on disk.
func (g *Gateway) AdminTokenHex() string { return hex.EncodeToString(g.cfg.adminToken) }

// AdminMiddleware wraps next in admin-auth verification intended for
// /admin/* HTTP routes. Reads (GET/HEAD/OPTIONS) pass through
// unauthenticated; non-read methods are gated by:
//
//  1. Registered AdminAuthorizer delegate at "_admin_auth/v1", if any.
//     OK accepts; DENIED rejects without falling through. Any other
//     code (UNAVAILABLE, NOT_CONFIGURED, UNSPECIFIED, transport err)
//     falls through to step 2.
//  2. Boot-token Bearer check (the unconditional fallback).
//
// GraphQL reads of admin_* fields (which dispatch GET to /admin/*)
// stay public for the UI; mutations require auth end-to-end. Future
// destructive reads will need explicit opt-in once they exist.
//
// Authenticated requests carry IsAdminAuth(ctx) == true on the way
// through.
func (g *Gateway) AdminMiddleware(next http.Handler) http.Handler {
	tok := g.cfg.adminToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPublicMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		switch outcome, _ := g.consultAdminDelegate(r.Context(), r); outcome {
		case adminDelegateAccept:
			g.cfg.metrics.RecordAdminAuth(r.Method, "ok_delegate")
			next.ServeHTTP(w, r.WithContext(WithAdminAuth(r.Context())))
			return
		case adminDelegateReject:
			g.cfg.metrics.RecordAdminAuth(r.Method, "denied_delegate")
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Fall through to boot-token check.
		if len(tok) == 0 {
			g.cfg.metrics.RecordAdminAuth(r.Method, "no_token_configured")
			http.Error(w, "admin token not configured", http.StatusInternalServerError)
			return
		}
		if !checkBearerEqual(r, tok) {
			g.cfg.metrics.RecordAdminAuth(r.Method, "denied_bearer")
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		g.cfg.metrics.RecordAdminAuth(r.Method, "ok_bearer")
		next.ServeHTTP(w, r.WithContext(WithAdminAuth(r.Context())))
	})
}

func isAdminPublicMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func checkBearerEqual(r *http.Request, want []byte) bool {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		return false
	}
	got := strings.TrimSpace(authz[len(prefix):])
	gotBytes, err := decodeAdminTokenString(got)
	if err != nil {
		return false
	}
	if subtle.ConstantTimeEq(int32(len(gotBytes)), int32(len(want))) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(gotBytes, want) == 1
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
	if dir == "" {
		return generateAdminToken()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("admin token dir: %w", err)
	}
	path := filepath.Join(dir, adminTokenFilename)
	b, err := os.ReadFile(path)
	if err == nil {
		s := strings.TrimSpace(string(b))
		decoded, derr := hex.DecodeString(s)
		if derr != nil {
			return nil, fmt.Errorf("admin token at %s: %w", path, derr)
		}
		if len(decoded) == 0 {
			return nil, fmt.Errorf("admin token at %s: empty", path)
		}
		return decoded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read admin token at %s: %w", path, err)
	}
	tok, err := generateAdminToken()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(tok)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write admin token at %s: %w", path, err)
	}
	return tok, nil
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
func (g *Gateway) AdminTokenPath() string {
	if g.cfg.adminDataDir == "" {
		return ""
	}
	return filepath.Join(g.cfg.adminDataDir, adminTokenFilename)
}

type adminAuthCtxKey struct{}

// WithAdminAuth marks ctx as having passed AdminMiddleware bearer
// verification. Used by handlers that want to short-circuit additional
// checks once the middleware has approved.
func WithAdminAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminAuthCtxKey{}, true)
}

// IsAdminAuth reports whether ctx was authenticated by AdminMiddleware.
func IsAdminAuth(ctx context.Context) bool {
	v, _ := ctx.Value(adminAuthCtxKey{}).(bool)
	return v
}
