// Package adminsession is the one implementation of admin session
// cookies and bearers, shared by the gateway core (sign-in codes,
// gwag login --web) and gw/oidc (SSO). It holds the signed format,
// the checks every session passes (signature, expiry, revocation
// cutoff, logout denylist) and the cookie flags. It imports nothing
// outside the standard library, so the core stays free of go-oidc.
//
// A signed value is base64url(JSON).base64url(HMAC-SHA256). The MAC
// covers a purpose string, so a value signed for one purpose never
// passes as another:
//
//   - PurposeSSO: the SSO session cookie. The role is not in it; the
//     SSO rules give it per request from the email and groups.
//   - PurposeCLIBearer: the gwag login --sso bearer (gwo_...).
//   - PurposeToken: a session redeemed from a sign-in code (gwag login
//     --web). The role is in it (Payload.Role), fixed when the code
//     was minted. A gateway that predates this purpose refuses such a
//     cookie (bad signature), so a token session never reaches the
//     SSO rules on an older node.
package adminsession

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// CookieName carries the signed session, whatever its purpose.
	CookieName = "gwag_session"

	PurposeSSO       = "session"
	PurposeCLIBearer = "cli-bearer"
	PurposeToken     = "token-session"

	// DefaultTTL is how long a session lasts when nothing sets it.
	DefaultTTL = 12 * time.Hour
)

// Payload is a signed session.
type Payload struct {
	// Email names the subject: an email for SSO, "token:admin" or a
	// delegate subject for a token session.
	Email string `json:"e"`
	// Groups are the SSO groups the rules name. SSO only.
	Groups []string `json:"g,omitempty"`
	// Role is the role of a token session. Empty for SSO, where the
	// rules decide.
	Role string `json:"r,omitempty"`
	Exp  int64  `json:"x"`
	// Issued is unix ms, checked against the revocation cutoff.
	Issued int64 `json:"i"`
	// ID names this one session; logout puts it on the denylist. A
	// payload without one is refused.
	ID string `json:"j"`
}

// Store is what every node shares. *gateway.Gateway is one.
type Store interface {
	AdminSessionKey(ctx context.Context) ([]byte, error)
	AdminSessionNotBefore(ctx context.Context, subject string) (int64, error)
	AdminSessionDenied(ctx context.Context, id string) (bool, error)
	DenyAdminSession(ctx context.Context, id string, until time.Time) error
}

// Sign encodes v under purpose.
func Sign(key []byte, purpose string, v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, purpose, body))
}

// Verify checks the MAC of value under purpose and decodes it into
// out. It checks nothing else.
func Verify(key []byte, purpose, value string, out any) bool {
	body, sig, ok := strings.Cut(value, ".")
	if !ok || len(key) == 0 {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, purpose, body)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

func mac(key []byte, purpose, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("gwag-" + purpose + "\x00" + body))
	return m.Sum(nil)
}

// Check verifies a signed session: signature under purpose, expiry
// (against now), subject and id present, issued after the subject's
// revocation cutoff, not on the logout denylist. A store error
// refuses the session.
func Check(ctx context.Context, st Store, purpose, value string, now time.Time) (Payload, bool) {
	if st == nil || value == "" {
		return Payload{}, false
	}
	key, err := st.AdminSessionKey(ctx)
	if err != nil {
		slog.Warn("admin session: key unavailable; refusing the session", "err", err)
		return Payload{}, false
	}
	var p Payload
	if !Verify(key, purpose, value, &p) || p.Exp < now.Unix() || p.Email == "" || p.ID == "" {
		return Payload{}, false
	}
	nb, err := st.AdminSessionNotBefore(ctx, p.Email)
	if err != nil {
		slog.Warn("admin session: revocation lookup failed; refusing the session", "subject", p.Email, "err", err)
		return Payload{}, false
	}
	if nb > 0 && p.Issued <= nb {
		return Payload{}, false
	}
	denied, err := st.AdminSessionDenied(ctx, p.ID)
	if err != nil {
		slog.Warn("admin session: denylist lookup failed; refusing the session", "subject", p.Email, "err", err)
		return Payload{}, false
	}
	if denied {
		return Payload{}, false
	}
	return p, true
}

// TokenSession is the token session the cookie on r carries: one
// signed under PurposeToken that passes Check and names a role.
func TokenSession(r *http.Request, st Store, now time.Time) (Payload, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return Payload{}, false
	}
	p, ok := Check(r.Context(), st, PurposeToken, c.Value, now)
	if !ok || (p.Role != "admin" && p.Role != "dev") {
		return Payload{}, false
	}
	return p, true
}

// Current is the session the cookie on r carries under either cookie
// purpose (PurposeSSO, PurposeToken), whether or not a rule gives it
// a role now.
func Current(r *http.Request, st Store, now time.Time) (Payload, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return Payload{}, false
	}
	for _, purpose := range []string{PurposeSSO, PurposeToken} {
		if p, ok := Check(r.Context(), st, purpose, c.Value, now); ok {
			return p, true
		}
	}
	return Payload{}, false
}

// End puts the session value on the denylist until it expires, trying
// each purpose in turn. found is false when the value is not a valid
// session under any of them (already refused: nothing to end).
func End(ctx context.Context, st Store, value string, now time.Time, purposes ...string) (p Payload, found bool, err error) {
	for _, purpose := range purposes {
		if p, ok := Check(ctx, st, purpose, value, now); ok {
			return p, true, st.DenyAdminSession(ctx, p.ID, time.Unix(p.Exp, 0))
		}
	}
	return Payload{}, false, nil
}

// SetCookie sets the session cookie: HttpOnly, SameSite=Lax (a
// top-level navigation from elsewhere carries it; adminSessionAllowed
// makes such a request a read), Path /.
func SetCookie(w http.ResponseWriter, value string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearCookie removes cookie name at path.
func ClearCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
}

// RequestSecure reports whether the browser reached the gateway over
// https: TLS, or a proxy that says X-Forwarded-Proto: https.
func RequestSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// RandomString is 32 random bytes, base64url.
func RandomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
