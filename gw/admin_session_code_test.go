package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/gw/internal/adminsession"
)

type codeHarness struct {
	t   *testing.T
	g   *Gateway
	mux *http.ServeMux
}

func newCodeHarness(t *testing.T, opts ...Option) *codeHarness {
	t.Helper()
	g := New(append([]Option{WithoutMetrics(), WithAdminToken([]byte("admin-token"))}, opts...)...)
	t.Cleanup(g.Close)
	return &codeHarness{t: t, g: g, mux: codeMux(t, g)}
}

func codeMux(t *testing.T, g *Gateway) *http.ServeMux {
	t.Helper()
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	m := http.NewServeMux()
	m.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
	g.MountAdminSessions(m)
	return m
}

func (h *codeHarness) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, r)
	return rec
}

// mint calls POST /api/admin/sessions/code with bearer (hex) or, when
// bearer is "", with cookie c.
func (h *codeHarness) mint(bearer string, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/sessions/code", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if c != nil {
		r.AddCookie(c)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return h.do(r)
}

func (h *codeHarness) mintCode(bearer string) (code, role, subject string) {
	h.t.Helper()
	rec := h.mint(bearer, nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Code, Role, Subject string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Code) < 22 { // ≥ 128 bits base64url
		h.t.Fatalf("code %q too short", out.Code)
	}
	return out.Code, out.Role, out.Subject
}

func redeemReq(code, site string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/auth/session", strings.NewReader(`{"code":"`+code+`"}`))
	r.Header.Set("Content-Type", "application/json")
	if site != "" {
		r.Header.Set("Sec-Fetch-Site", site)
	}
	return r
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == adminsession.CookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

func (h *codeHarness) redeem(code string) *http.Cookie {
	h.t.Helper()
	rec := h.do(redeemReq(code, "same-origin"))
	c := sessionCookie(rec)
	if rec.Code != http.StatusOK || c == nil {
		h.t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	return c
}

func (h *codeHarness) role(c *http.Cookie) string {
	r := httptest.NewRequest(http.MethodGet, "/api/admin/whoami", nil)
	if c != nil {
		r.AddCookie(c)
	}
	var out struct{ Role string }
	_ = json.Unmarshal(h.do(r).Body.Bytes(), &out)
	return out.Role
}

func TestAdminSessionCodeAdmin(t *testing.T) {
	h := newCodeHarness(t)
	code, role, subject := h.mintCode(h.g.AdminTokenHex())
	if role != "admin" || subject != "token:admin" {
		t.Fatalf("role %q subject %q", role, subject)
	}
	rec := h.do(redeemReq(code, "same-origin"))
	c := sessionCookie(rec)
	if rec.Code != http.StatusOK || c == nil {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Errorf("cookie flags: %+v", c)
	}
	if c.MaxAge != int(DefaultAdminSessionTTL.Seconds()) {
		t.Errorf("max-age %d", c.MaxAge)
	}
	if got := h.role(c); got != "admin" {
		t.Errorf("cookie role %q, want admin", got)
	}
	// Single use.
	rec = h.do(redeemReq(code, "same-origin"))
	if rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil {
		t.Errorf("second redeem: %d %s", rec.Code, rec.Body)
	}
	// /auth/whoami without SSO reports the session.
	r := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	r.AddCookie(c)
	var who struct {
		SSO, Authenticated bool
		Email, Role, Via   string
	}
	_ = json.Unmarshal(h.do(r).Body.Bytes(), &who)
	if who.SSO || !who.Authenticated || who.Email != "token:admin" || who.Role != "admin" || who.Via != "token" {
		t.Errorf("whoami %+v", who)
	}
}

func TestAdminSessionCodeDev(t *testing.T) {
	h := newCodeHarness(t)
	code, role, subject := h.mintCode(h.g.DevTokenHex())
	if role != "dev" || subject != "token:dev" {
		t.Fatalf("role %q subject %q", role, subject)
	}
	c := h.redeem(code)
	if got := h.role(c); got != "dev" {
		t.Errorf("cookie role %q, want dev", got)
	}
	// A dev session cannot do admin writes.
	r := httptest.NewRequest(http.MethodPost, "/api/admin/dev-token/rotate", nil)
	r.AddCookie(c)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("dev session on an admin write: %d", rec.Code)
	}
}

// A session cookie alone cannot mint a code, whatever its role.
func TestAdminSessionCodeCookieCannotMint(t *testing.T) {
	h := newCodeHarness(t)
	code, _, _ := h.mintCode(h.g.AdminTokenHex())
	c := h.redeem(code)
	if rec := h.mint("", c); rec.Code != http.StatusForbidden {
		t.Errorf("mint with cookie: %d %s", rec.Code, rec.Body)
	}
	if rec := h.mint("", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("mint anonymous: %d %s", rec.Code, rec.Body)
	}
	if rec := h.mint("00ff", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("mint with a wrong bearer: %d %s", rec.Code, rec.Body)
	}
}

// Redeem needs Sec-Fetch-Site: same-origin; a refused attempt leaves
// the code for the real one.
func TestAdminSessionCodeCSRF(t *testing.T) {
	h := newCodeHarness(t)
	code, _, _ := h.mintCode(h.g.AdminTokenHex())
	for _, site := range []string{"", "cross-site", "same-site", "none"} {
		if rec := h.do(redeemReq(code, site)); rec.Code != http.StatusForbidden || sessionCookie(rec) != nil {
			t.Errorf("Sec-Fetch-Site %q: %d", site, rec.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/session?code="+code, nil)
	if rec := h.do(r); rec.Code == http.StatusOK || sessionCookie(rec) != nil {
		t.Errorf("GET redeem: %d", rec.Code)
	}
	h.redeem(code)
}

func TestAdminSessionCodeExpiry(t *testing.T) {
	h := newCodeHarness(t)
	ctx := context.Background()
	v, _ := json.Marshal(adminSessionCode{Subject: "token:admin", Role: "admin"})
	if err := h.g.PutAdminLoginCode(ctx, adminSessionCodeID("short"), v, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if rec := h.do(redeemReq("short", "same-origin")); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired code: %d %s", rec.Code, rec.Body)
	}
	// An expired cookie is refused.
	key, _ := h.g.AdminSessionKey(ctx)
	old := &http.Cookie{Name: adminsession.CookieName, Value: adminsession.Sign(key, adminsession.PurposeToken, adminsession.Payload{
		Email: "token:admin", Role: "admin", Exp: time.Now().Add(-time.Second).Unix(), Issued: time.Now().Add(-time.Hour).UnixMilli(), ID: "x",
	})}
	if got := h.role(old); got != "anonymous" {
		t.Errorf("expired cookie role %q", got)
	}
	// A cookie signed for another purpose (an SSO session) is not a
	// token session: without SSO it is no session at all.
	sso := &http.Cookie{Name: adminsession.CookieName, Value: adminsession.Sign(key, adminsession.PurposeSSO, adminsession.Payload{
		Email: "token:admin", Role: "admin", Exp: time.Now().Add(time.Hour).Unix(), Issued: time.Now().UnixMilli(), ID: "y",
	})}
	if got := h.role(sso); got != "anonymous" {
		t.Errorf("SSO-purpose cookie role %q", got)
	}
}

func TestAdminSessionCodeRevokeAndLogout(t *testing.T) {
	h := newCodeHarness(t)
	code, _, _ := h.mintCode(h.g.AdminTokenHex())
	c := h.redeem(code)

	// Logout ends this session on the server, not only in the browser.
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(c)
	if rec := h.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("logout without Sec-Fetch-Site: %d", rec.Code)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := h.do(r); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	if got := h.role(c); got != "anonymous" {
		t.Errorf("after logout role %q", got)
	}

	code, _, _ = h.mintCode(h.g.DevTokenHex())
	c = h.redeem(code)
	if _, err := h.g.RevokeAdminSessions(context.Background(), "token:dev"); err != nil {
		t.Fatal(err)
	}
	if got := h.role(c); got != "anonymous" {
		t.Errorf("after revoke role %q", got)
	}
	// Signing in again is refused until the cutoff passes.
	code, _, _ = h.mintCode(h.g.DevTokenHex())
	if rec := h.do(redeemReq(code, "same-origin")); rec.Code != http.StatusForbidden {
		t.Errorf("redeem inside the revoke window: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminSessionCodeCap(t *testing.T) {
	h := newCodeHarness(t)
	for range maxAdminSessionCodes {
		h.mintCode(h.g.AdminTokenHex())
	}
	if rec := h.mint(h.g.AdminTokenHex(), nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("mint past the cap: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminSessionCodeSecureCookie(t *testing.T) {
	h := newCodeHarness(t)
	code, _, _ := h.mintCode(h.g.AdminTokenHex())
	r := redeemReq(code, "same-origin")
	r.Header.Set("X-Forwarded-Proto", "https")
	c := sessionCookie(h.do(r))
	if c == nil || !c.Secure {
		t.Fatalf("cookie behind https proxy: %+v", c)
	}
}

// With an authenticator that names a TTL (gw/oidc), the redeemed
// session lasts as long; its /auth/* routes are not mounted twice.
type ttlSession struct{ fixedSession }

func (ttlSession) AdminSessionTTL() time.Duration { return time.Hour }

func TestAdminSessionCodeTTLFromAuthenticator(t *testing.T) {
	h := newCodeHarness(t, WithAdminSession(ttlSession{}))
	code, _, _ := h.mintCode(h.g.AdminTokenHex())
	c := h.redeem(code)
	if c.MaxAge != 3600 {
		t.Errorf("max-age %d, want 3600", c.MaxAge)
	}
	if got := h.role(c); got != "admin" {
		t.Errorf("role %q", got)
	}
	if rec := h.do(httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("/auth/whoami mounted with an authenticator: %d", rec.Code)
	}
}
