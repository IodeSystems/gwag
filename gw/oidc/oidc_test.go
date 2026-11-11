package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/oidc/oidctest"
)

const clientID = "gwag-test"

func goodClaims(iss, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss, "aud": clientID, "sub": "u1", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": "ops@example.com", "email_verified": true,
	}
}

// withClaims returns a claims func: goodClaims changed by edit.
func withClaims(edit func(c map[string]any)) func(string, string) map[string]any {
	return func(iss, nonce string) map[string]any {
		c := goodClaims(iss, nonce)
		edit(c)
		return c
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// harness is a standalone gateway with SSO mounted, as gwag up wires it.
type harness struct {
	idp *oidctest.Provider
	sso *SSO
	gw  *gateway.Gateway
	mux *http.ServeMux
}

func newHarness(t *testing.T, edit func(*Config)) *harness {
	t.Helper()
	h := &harness{idp: oidctest.New(t, clientID)}
	cfg := Config{
		Issuer: h.idp.URL, ClientID: clientID, ClientSecret: "shh",
		RedirectURL: "http://gwag.test/auth/callback",
		Rules:       []Rule{{Kind: RuleEmail, Value: "Ops@Example.com", Role: gateway.AdminSessionAdmin}},
	}
	if edit != nil {
		edit(&cfg)
	}
	sso, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.sso = sso
	h.gw = gateway.New(gateway.WithAdminSession(sso), gateway.WithoutMetrics(), gateway.WithAdminDataDir(t.TempDir()))
	t.Cleanup(h.gw.Close)
	h.mux = http.NewServeMux()
	sso.Mount(h.mux, h.gw)
	adminMux, _, err := h.gw.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	h.mux.Handle("/api/admin/", http.StripPrefix("/api", h.gw.AdminMiddleware(adminMux)))
	return h
}

func (h *harness) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, r)
	return rec
}

// login runs /auth/login and returns the state cookie and the state
// the provider would echo back.
func (h *harness) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := h.serve(httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if loc.Path != "/authorize" || q.Get("client_id") != clientID || q.Get("redirect_uri") != h.sso.cfg.RedirectURL ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" ||
		q.Get("scope") != "openid email profile" {
		t.Fatalf("authorize URL missing parameters: %s", loc)
	}
	if err := h.idp.Expect(loc.String()); err != nil {
		t.Fatal(err)
	}
	c := findCookie(rec, stateCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth/" {
		t.Fatalf("state cookie = %+v", c)
	}
	return c, q.Get("state")
}

func (h *harness) callback(state string, c *http.Cookie, extra string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+oidctest.Code+"&state="+url.QueryEscape(state)+extra, nil)
	if c != nil {
		r.AddCookie(c)
	}
	return h.serve(r)
}

func (h *harness) signIn(t *testing.T) *httptest.ResponseRecorder {
	c, state := h.login(t)
	return h.callback(state, c, "")
}

// admin sends method path to the admin API with set applied.
func (h *harness) admin(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if set != nil {
		set(r)
	}
	return h.serve(r)
}

// roleWith is the role /api/admin/whoami reports for cookie c.
func (h *harness) roleWith(t *testing.T, c *http.Cookie) string {
	t.Helper()
	rec := h.admin(http.MethodGet, "/api/admin/whoami", "", func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami = %d: %s", rec.Code, rec.Body)
	}
	var out struct{ Role string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Role
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	c := findCookie(rec, SessionCookie)
	if c == nil || c.Value == "" {
		t.Fatalf("no session cookie (status %d): %s", rec.Code, rec.Body)
	}
	return c
}

func noSession(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body)
	}
	if c := findCookie(rec, SessionCookie); c != nil && c.Value != "" {
		t.Errorf("session cookie issued on a rejected sign-in")
	}
}

func payloadOf(t *testing.T, h *harness, c *http.Cookie) sessionPayload {
	t.Helper()
	key, err := h.gw.AdminSessionKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sp sessionPayload
	if !verify(key, "session", c.Value, &sp) {
		t.Fatal("session cookie does not verify")
	}
	return sp
}

func TestSSOHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.signIn(t)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d → %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sess := sessionFrom(t, rec)
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.MaxAge != int(DefaultSessionTTL.Seconds()) {
		t.Errorf("session cookie attributes = %+v", sess)
	}
	if sess.Secure {
		t.Errorf("Secure set on a plain-http deployment; the browser would drop it")
	}
	if sp := payloadOf(t, h, sess); sp.Email != "ops@example.com" || sp.Exp-time.Now().Unix() > int64(DefaultSessionTTL.Seconds()) {
		t.Errorf("payload = %+v", sp)
	}
	if got := h.roleWith(t, sess); got != "admin" {
		t.Errorf("admin whoami with session = %q, want admin", got)
	}
	if got := h.roleWith(t, nil); got != "anonymous" {
		t.Errorf("admin whoami without = %q, want anonymous", got)
	}
	// A dev route.
	if rec := h.admin(http.MethodGet, "/api/admin/peers", "", func(r *http.Request) { r.AddCookie(sess) }); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Errorf("GET peers with session = %d: %s", rec.Code, rec.Body)
	}

	r := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	r.AddCookie(sess)
	var who struct {
		SSO           bool   `json:"sso"`
		Authenticated bool   `json:"authenticated"`
		Email         string `json:"email"`
		Role          string `json:"role"`
		Via           string `json:"via"`
	}
	_ = json.Unmarshal(h.serve(r).Body.Bytes(), &who)
	if !who.SSO || !who.Authenticated || who.Email != "ops@example.com" || who.Role != "admin" || who.Via != "sso" {
		t.Errorf("whoami = %+v", who)
	}
	_ = json.Unmarshal(h.serve(httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)).Body.Bytes(), &who)
	if !who.SSO || who.Authenticated {
		t.Errorf("whoami without a session = %+v", who)
	}

	// Logout: same-origin only; clears the cookie.
	if rec := h.serve(httptest.NewRequest(http.MethodPost, "/auth/logout", nil)); rec.Code != http.StatusForbidden {
		t.Errorf("logout without Sec-Fetch-Site = %d, want 403", rec.Code)
	}
	lr := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	lr.Header.Set("Sec-Fetch-Site", "same-origin")
	if c := findCookie(h.serve(lr), SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout did not clear the session cookie: %+v", c)
	}
}

func TestSSOSecureCookieBehindTLS(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RedirectURL = "https://gwag.example.com/auth/callback" })
	if c := sessionFrom(t, h.signIn(t)); !c.Secure {
		t.Errorf("session cookie not Secure with an https redirect URL")
	}
}

func TestSSOWrongStateRejected(t *testing.T) {
	h := newHarness(t, nil)
	c, _ := h.login(t)
	noSession(t, h.callback("forged-state", c, ""), http.StatusBadRequest)
}

func TestSSOMissingStateCookieRejected(t *testing.T) {
	h := newHarness(t, nil)
	_, state := h.login(t)
	noSession(t, h.callback(state, nil, ""), http.StatusBadRequest)
}

func TestSSOTamperedStateCookieRejected(t *testing.T) {
	h := newHarness(t, nil)
	c, state := h.login(t)
	c.Value = sign([]byte("another key"), "oidc-state", statePayload{State: state, Exp: time.Now().Add(time.Hour).Unix()})
	noSession(t, h.callback(state, c, ""), http.StatusBadRequest)
}

// A state cookie with the right state but another PKCE verifier: the
// provider refuses the code exchange.
func TestSSOPKCEMismatchRejected(t *testing.T) {
	h := newHarness(t, nil)
	c, state := h.login(t)
	key, _ := h.gw.AdminSessionKey(context.Background())
	var sp statePayload
	if !verify(key, "oidc-state", c.Value, &sp) {
		t.Fatal("state cookie does not verify")
	}
	sp.Verifier = "a-different-verifier-a-different-verifier-0123"
	c.Value = sign(key, "oidc-state", sp)
	noSession(t, h.callback(state, c, ""), http.StatusBadGateway)
}

func TestSSOWrongNonceRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.SetClaims(func(iss, _ string) map[string]any { return goodClaims(iss, "someone-elses-nonce") })
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOWrongAudienceRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.SetClaims(withClaims(func(c map[string]any) { c["aud"] = "another-client" }))
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOExpiredTokenRejected(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.SetClaims(withClaims(func(c map[string]any) {
		c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
	}))
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

func TestSSOProviderErrorPage(t *testing.T) {
	h := newHarness(t, nil)
	c, state := h.login(t)
	rec := h.callback(state, c, "&error=access_denied&error_description="+url.QueryEscape("<script>x</script>"))
	noSession(t, rec, http.StatusUnauthorized)
	body := rec.Body.String()
	if !strings.Contains(body, "access_denied") || strings.Contains(body, "<script>") {
		t.Errorf("error page = %s", body)
	}
}

func TestSSOUnverifiedEmailRefused(t *testing.T) {
	for name, v := range map[string]any{"false": false, "string false": "false", "missing": nil} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.idp.SetClaims(withClaims(func(c map[string]any) {
				if v == nil {
					delete(c, "email_verified")
				} else {
					c["email_verified"] = v
				}
			}))
			rec := h.signIn(t)
			noSession(t, rec, http.StatusForbidden)
			if !strings.Contains(rec.Body.String(), "has not verified") {
				t.Errorf("page = %s", rec.Body)
			}
		})
	}
	h := newHarness(t, func(c *Config) { c.AllowUnverifiedEmail = true })
	h.idp.SetClaims(withClaims(func(c map[string]any) { delete(c, "email_verified") }))
	sessionFrom(t, h.signIn(t))
}

// First match wins: the email rule above the domain rule decides for
// ops@, the domain rule for everyone else there; another domain gets
// nothing.
func TestSSORoleRulesInOrder(t *testing.T) {
	rules := []Rule{
		{Kind: RuleEmail, Value: "ops@example.com", Role: gateway.AdminSessionDev},
		{Kind: RuleDomain, Value: "example.com", Role: gateway.AdminSessionAdmin},
		{Kind: RuleEmail, Value: "ops@example.com", Role: gateway.AdminSessionAdmin},
	}
	for email, want := range map[string]string{
		"ops@example.com":   "dev",
		"Alice@Example.COM": "admin",
	} {
		h := newHarness(t, func(c *Config) { c.Rules = rules })
		h.idp.SetClaims(withClaims(func(c map[string]any) { c["email"] = email }))
		if got := h.roleWith(t, sessionFrom(t, h.signIn(t))); got != want {
			t.Errorf("%s: role %q, want %q", email, got, want)
		}
	}
	h := newHarness(t, func(c *Config) { c.Rules = rules })
	h.idp.SetClaims(withClaims(func(c map[string]any) { c["email"] = "alice@a.example.com" }))
	noSession(t, h.signIn(t), http.StatusForbidden)
}

// A group rule reads the configured claim; only the groups the rules
// name are kept in the cookie.
func TestSSOGroupRule(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.GroupsClaim = "roles"
		c.Rules = []Rule{
			{Kind: RuleGroup, Value: "gwag-admins", Role: gateway.AdminSessionAdmin},
			{Kind: RuleGroup, Value: "gwag-devs", Role: gateway.AdminSessionDev},
		}
	})
	h.idp.SetClaims(withClaims(func(c map[string]any) { c["roles"] = []any{"staff", "gwag-devs", "everyone"} }))
	sess := sessionFrom(t, h.signIn(t))
	if got := h.roleWith(t, sess); got != "dev" {
		t.Errorf("role %q, want dev", got)
	}
	if sp := payloadOf(t, h, sess); len(sp.Groups) != 1 || sp.Groups[0] != "gwag-devs" {
		t.Errorf("cookie groups = %v, want [gwag-devs]", sp.Groups)
	}

	// Groups only at userinfo.
	h.idp.SetClaims(goodClaims)
	h.idp.SetUserinfo(map[string]any{"sub": "u1", "roles": []any{"gwag-admins"}})
	if got := h.roleWith(t, sessionFrom(t, h.signIn(t))); got != "admin" {
		t.Errorf("groups from userinfo: role %q, want admin", got)
	}

	// The default claim name is groups; not a member of any: refused.
	h2 := newHarness(t, func(c *Config) {
		c.Rules = []Rule{{Kind: RuleGroup, Value: "gwag-admins", Role: gateway.AdminSessionAdmin}}
	})
	h2.idp.SetClaims(withClaims(func(c map[string]any) { c["groups"] = []any{"gwag-admins"} }))
	sessionFrom(t, h2.signIn(t))
	h2.idp.SetClaims(withClaims(func(c map[string]any) { c["groups"] = "staff" }))
	noSession(t, h2.signIn(t), http.StatusForbidden)
}

func TestSSONoRuleMatchesDenied(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.SetClaims(withClaims(func(c map[string]any) { c["email"] = "stranger@example.com" }))
	rec := h.signIn(t)
	noSession(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "stranger@example.com has no role") {
		t.Errorf("refusal page does not name the email: %s", rec.Body)
	}
}

func TestSSOEmailFromUserinfo(t *testing.T) {
	noEmail := withClaims(func(c map[string]any) { delete(c, "email"); delete(c, "email_verified") })
	h := newHarness(t, nil)
	h.idp.SetClaims(noEmail)
	h.idp.SetUserinfo(map[string]any{"sub": "u1", "email": "ops@example.com", "email_verified": true})
	sessionFrom(t, h.signIn(t))

	h.idp.SetUserinfo(map[string]any{"sub": "someone-else", "email": "ops@example.com", "email_verified": true})
	noSession(t, h.signIn(t), http.StatusUnauthorized)
}

// Cookie-authenticated writes need Sec-Fetch-Site: same-origin; reads
// do not; a bearer needs neither.
func TestSSOCSRF(t *testing.T) {
	h := newHarness(t, nil)
	sess := sessionFrom(t, h.signIn(t))
	rotate := func(set func(*http.Request)) int {
		return h.admin(http.MethodPost, "/api/admin/dev-token/rotate", "", func(r *http.Request) {
			r.AddCookie(sess)
			set(r)
		}).Code
	}
	for site, want := range map[string]int{"": 403, "cross-site": 403, "same-site": 403, "none": 403, "same-origin": 200} {
		code := rotate(func(r *http.Request) {
			if site != "" {
				r.Header.Set("Sec-Fetch-Site", site)
			}
		})
		if code != want {
			t.Errorf("POST with session, Sec-Fetch-Site %q = %d, want %d", site, code, want)
		}
	}
	// A cross-site GET is a read.
	if rec := h.admin(http.MethodGet, "/api/admin/peers", "", func(r *http.Request) {
		r.AddCookie(sess)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}); rec.Code != http.StatusOK {
		t.Errorf("cross-site GET with session = %d", rec.Code)
	}
	// Bearer path unchanged.
	if rec := h.admin(http.MethodPost, "/api/admin/dev-token/rotate", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+h.gw.AdminTokenHex())
		r.AddCookie(sess)
	}); rec.Code != http.StatusOK {
		t.Errorf("bearer POST = %d", rec.Code)
	}
	// A dev session may not call an admin write even same-origin.
	hd := newHarness(t, func(c *Config) {
		c.Rules = []Rule{{Kind: RuleDomain, Value: "example.com", Role: gateway.AdminSessionDev}}
	})
	dsess := sessionFrom(t, hd.signIn(t))
	if rec := hd.admin(http.MethodPost, "/api/admin/dev-token/rotate", "", func(r *http.Request) {
		r.AddCookie(dsess)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}); rec.Code != http.StatusForbidden {
		t.Errorf("dev session admin write = %d, want 403", rec.Code)
	}
}

func TestSSORevocation(t *testing.T) {
	h := newHarness(t, nil)
	sess := sessionFrom(t, h.signIn(t))
	revoke := func(subject string) {
		t.Helper()
		rec := h.admin(http.MethodPost, "/api/admin/sessions/revoke", `{"subject":"`+subject+`"}`, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+h.gw.AdminTokenHex())
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
		}
	}
	// Another subject: no effect.
	revoke("someone@example.com")
	if got := h.roleWith(t, sess); got != "admin" {
		t.Fatalf("after revoking someone else: %q", got)
	}
	revoke("OPS@example.com")
	if got := h.roleWith(t, sess); got != "anonymous" {
		t.Errorf("revoked session: role %q, want anonymous", got)
	}
	time.Sleep(5 * time.Millisecond)
	sess2 := sessionFrom(t, h.signIn(t))
	if got := h.roleWith(t, sess2); got != "admin" {
		t.Errorf("new session after revoke: role %q", got)
	}
	revoke("*")
	if got := h.roleWith(t, sess2); got != "anonymous" {
		t.Errorf("after revoke *: role %q", got)
	}
	// A session cookie cannot call revoke cross-site.
	time.Sleep(5 * time.Millisecond)
	sess3 := sessionFrom(t, h.signIn(t))
	if rec := h.admin(http.MethodPost, "/api/admin/sessions/revoke", `{"subject":"*"}`, func(r *http.Request) {
		r.AddCookie(sess3)
	}); rec.Code != http.StatusForbidden {
		t.Errorf("revoke with session and no Sec-Fetch-Site = %d, want 403", rec.Code)
	}
}

func TestSSOTamperedOrExpiredSession(t *testing.T) {
	h := newHarness(t, nil)
	sess := sessionFrom(t, h.signIn(t))
	key, _ := h.gw.AdminSessionKey(context.Background())
	body, sig, _ := strings.Cut(sess.Value, ".")
	forged := sessionPayload{Email: "ops@example.com", Exp: time.Now().Add(1000 * time.Hour).Unix(), Issued: time.Now().UnixMilli()}
	fb, _ := json.Marshal(forged)
	for name, v := range map[string]string{
		"payload swapped":  b64(fb) + "." + sig,
		"sig flipped":      body + "." + strings.Repeat("A", len(sig)),
		"no sig":           body,
		"other key":        sign([]byte("not the gateway key"), "session", forged),
		"state as session": sign(key, "oidc-state", forged),
	} {
		if got := h.roleWith(t, &http.Cookie{Name: SessionCookie, Value: v}); got != "anonymous" {
			t.Errorf("%s: role %q", name, got)
		}
	}
	h.sso.cfg.Now = func() time.Time { return time.Now().Add(DefaultSessionTTL + time.Minute) }
	if got := h.roleWith(t, sess); got != "anonymous" {
		t.Errorf("expired session: role %q", got)
	}
}

type failingStore struct{ err error }

func (f failingStore) AdminSessionKey(context.Context) ([]byte, error) { return nil, f.err }
func (f failingStore) AdminSessionNotBefore(context.Context, string) (int64, error) {
	return 0, f.err
}

// The store fails (a cluster below the sessions feature level): login
// says why, whoami reports it, no session is accepted.
func TestSSOStoreUnavailable(t *testing.T) {
	sso, err := New(Config{Issuer: "http://127.0.0.1:1", ClientID: "x", Rules: []Rule{{Kind: RuleDomain, Value: "example.com", Role: gateway.AdminSessionAdmin}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sso.Mount(mux, failingStore{errors.New("admin sessions need cluster feature level 2")})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "feature level 2") {
		t.Errorf("login = %d: %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/whoami", nil))
	if !strings.Contains(rec.Body.String(), `"error":"admin sessions need cluster feature level 2"`) {
		t.Errorf("whoami = %s", rec.Body)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "x.y"})
	if _, ok := sso.AdminSession(r); ok {
		t.Error("session accepted without a store")
	}
}

func TestSSORedirectFromRequest(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RedirectURL = "" })
	login := func(r *http.Request) string {
		rec := h.serve(r)
		if rec.Code != http.StatusFound {
			t.Fatalf("login = %d: %s", rec.Code, rec.Body)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		return loc.Query().Get("redirect_uri")
	}
	if got := login(httptest.NewRequest(http.MethodGet, "http://box1:8080/auth/login", nil)); got != "http://box1:8080/auth/callback" {
		t.Errorf("direct = %q", got)
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/auth/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "gw.example.com")
	if got := login(r); got != "https://gw.example.com/auth/callback" {
		t.Errorf("proxied = %q", got)
	}
}

func TestParseRule(t *testing.T) {
	for in, want := range map[string]Rule{
		"email:Alice@Example.com=admin": {Kind: RuleEmail, Value: "Alice@Example.com", Role: gateway.AdminSessionAdmin},
		"domain:example.com=dev":        {Kind: RuleDomain, Value: "example.com", Role: gateway.AdminSessionDev},
		"group:/org/a=b=ADMIN":          {Kind: RuleGroup, Value: "/org/a=b", Role: gateway.AdminSessionAdmin},
	} {
		got, err := ParseRule(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "email=admin", "email:a@b", "email:ab=admin", "domain:a@b.com=dev", "user:x=admin", "group:x=root", "group:=admin"} {
		if _, err := ParseRule(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
	if _, err := New(Config{Issuer: "x", ClientID: "y"}); err == nil {
		t.Error("New with no rules: no error")
	}
}

// The UI's path: admin GraphQL fields reach /api/admin/* through the
// self-ingested admin OpenAPI source, which forwards the cookie and
// Sec-Fetch-Site (AdminForwardHeaders), so the CSRF rule applies to
// the browser's request.
func TestSSOThroughGraphQL(t *testing.T) {
	h := newHarness(t, nil)
	srv := httptest.NewServer(h.mux)
	t.Cleanup(srv.Close)
	_, spec, err := h.gw.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.gw.AddOpenAPIBytes(spec, gateway.As("admin"), gateway.To(srv.URL+"/api"),
		gateway.ForwardHeaders(gateway.AdminForwardHeaders...)); err != nil {
		t.Fatal(err)
	}
	h.mux.Handle("/api/graphql", h.gw.Handler())
	sess := sessionFrom(t, h.signIn(t))

	gql := func(query, site string) string {
		r, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/graphql", strings.NewReader(`{"query":`+jsonString(query)+`}`))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(sess)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		_, _ = io.Copy(&b, resp.Body)
		return b.String()
	}
	if got := gql(`{ admin { whoami { role } } }`, "same-origin"); !strings.Contains(got, `"role":"admin"`) {
		t.Errorf("whoami through GraphQL = %s", got)
	}
	const rotate = `mutation { admin { rotateDevToken { devToken } } }`
	if got := gql(rotate, "same-origin"); !strings.Contains(got, `"devToken":"`) {
		t.Errorf("same-origin mutation = %s", got)
	}
	for _, site := range []string{"", "cross-site", "same-site"} {
		if got := gql(rotate, site); strings.Contains(got, `"devToken":"`) || !strings.Contains(got, "errors") {
			t.Errorf("mutation with Sec-Fetch-Site %q = %s", site, got)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
