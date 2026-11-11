package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/oidc/oidctest"
)

const testVerifier = "verifier-verifier-verifier-verifier-verifier"

func challengeOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return b64(sum[:])
}

func cliQuery(port, state, challenge string) string {
	return "/auth/cli?" + url.Values{"port": {port}, "state": {state}, "challenge": {challenge}}.Encode()
}

// cliSignIn runs /auth/cli and the callback, and returns the loopback
// redirect.
func (h *harness) cliSignIn(t *testing.T, port, state string) *url.URL {
	t.Helper()
	rec := h.serve(httptest.NewRequest(http.MethodGet, cliQuery(port, state, challengeOf(testVerifier)), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/auth/cli = %d: %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if err := h.idp.Expect(loc.String()); err != nil {
		t.Fatal(err)
	}
	cb := h.callback(loc.Query().Get("state"), findCookie(rec, stateCookie), "")
	if cb.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d: %s", cb.Code, cb.Body)
	}
	if c := findCookie(cb, SessionCookie); c != nil && c.Value != "" {
		t.Error("CLI sign-in set a browser session cookie")
	}
	out, err := url.Parse(cb.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// exchange posts code and verifier to /auth/cli/token.
func (h *harness) exchange(code, verifier string) (*httptest.ResponseRecorder, CLITokenResponse) {
	b, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	rec := h.admin(http.MethodPost, "/auth/cli/token", string(b), nil)
	var out CLITokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (h *harness) cliToken(t *testing.T) CLITokenResponse {
	t.Helper()
	u := h.cliSignIn(t, "40000", "st")
	rec, out := h.exchange(u.Query().Get("code"), testVerifier)
	if rec.Code != http.StatusOK || !strings.HasPrefix(out.Token, gateway.AdminSessionBearerPrefix) {
		t.Fatalf("exchange = %d: %s", rec.Code, rec.Body)
	}
	return out
}

func (h *harness) roleWithBearer(t *testing.T, tok string) (string, int) {
	t.Helper()
	rec := h.admin(http.MethodGet, "/api/admin/whoami", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
	})
	var out struct{ Role string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Role, rec.Code
}

func TestCLIParamValidation(t *testing.T) {
	h := newHarness(t, nil)
	good := challengeOf(testVerifier)
	for name, q := range map[string]string{
		"no port":         cliQuery("", "st", good),
		"port low":        cliQuery("1023", "st", good),
		"port high":       cliQuery("65536", "st", good),
		"port not number": cliQuery("80a", "st", good),
		"no state":        cliQuery("4000", "", good),
		"state bad chars": cliQuery("4000", "a b", good),
		"state too long":  cliQuery("4000", strings.Repeat("a", maxCLIState+1), good),
		"no challenge":    cliQuery("4000", "st", ""),
		"challenge short": cliQuery("4000", "st", good[:42]),
		"challenge chars": cliQuery("4000", "st", good[:42]+"."),
	} {
		rec := h.serve(httptest.NewRequest(http.MethodGet, q, nil))
		if rec.Code != http.StatusBadRequest || findCookie(rec, stateCookie) != nil {
			t.Errorf("%s: status %d, want 400 and no flow cookie", name, rec.Code)
		}
	}
	for _, port := range []string{"1024", "65535"} {
		if rec := h.serve(httptest.NewRequest(http.MethodGet, cliQuery(port, "st", good), nil)); rec.Code != http.StatusFound {
			t.Errorf("port %s: status %d, want 302", port, rec.Code)
		}
	}
}

// The redirect goes to 127.0.0.1 whatever host the request names.
func TestCLIRedirectOnlyToLoopback(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.serve(httptest.NewRequest(http.MethodGet, cliQuery("40000", "st", challengeOf(testVerifier)), nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if err := h.idp.Expect(loc.String()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+oidctest.Code+"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	r.Host = "evil.example"
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.AddCookie(findCookie(rec, stateCookie))
	cb := h.serve(r)
	u, err := url.Parse(cb.Header().Get("Location"))
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:40000" || u.Path != "/callback" ||
		u.Query().Get("state") != "st" || u.Query().Get("code") == "" {
		t.Fatalf("redirect = %q", cb.Header().Get("Location"))
	}
}

func TestCLICodeSingleUseAndVerifier(t *testing.T) {
	h := newHarness(t, nil)
	code := h.cliSignIn(t, "40000", "st").Query().Get("code")
	if rec, _ := h.exchange(code, testVerifier+"x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong verifier = %d", rec.Code)
	}
	// The wrong attempt spent the code.
	if rec, _ := h.exchange(code, testVerifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("after wrong verifier = %d, want 400", rec.Code)
	}
	code = h.cliSignIn(t, "40000", "st").Query().Get("code")
	if rec, out := h.exchange(code, testVerifier); rec.Code != http.StatusOK || out.Email != "ops@example.com" || out.Role != "admin" {
		t.Fatalf("exchange = %d: %s", rec.Code, rec.Body)
	}
	if rec, _ := h.exchange(code, testVerifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("second use = %d, want 400", rec.Code)
	}
	if rec, _ := h.exchange("made-up", testVerifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown code = %d, want 400", rec.Code)
	}
}

// ttlStore shortens the code TTL.
type ttlStore struct {
	Store
	ttl time.Duration
	got time.Duration
}

func (s *ttlStore) PutAdminLoginCode(ctx context.Context, id string, v []byte, ttl time.Duration) error {
	s.got = ttl
	return s.Store.PutAdminLoginCode(ctx, id, v, s.ttl)
}

func TestCLICodeTTL(t *testing.T) {
	h := newHarness(t, nil)
	st := &ttlStore{Store: h.gw, ttl: 30 * time.Millisecond}
	h.sso.store.Store(&storeBox{st})
	code := h.cliSignIn(t, "40000", "st").Query().Get("code")
	if st.got != codeTTL || codeTTL != 60*time.Second {
		t.Errorf("code TTL = %v, want 60s", st.got)
	}
	time.Sleep(60 * time.Millisecond)
	if rec, _ := h.exchange(code, testVerifier); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired code = %d, want 400", rec.Code)
	}
}

func TestCLIBearer(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.CLITokenTTL = 2 * time.Hour })
	out := h.cliToken(t)
	if exp, err := time.Parse(time.RFC3339, out.ExpiresAt); err != nil || time.Until(exp) > 2*time.Hour || time.Until(exp) < time.Hour {
		t.Errorf("expires_at = %q", out.ExpiresAt)
	}
	if role, _ := h.roleWithBearer(t, out.Token); role != "admin" {
		t.Fatalf("bearer role %q", role)
	}
	// An admin write with the bearer and no Sec-Fetch-Site: a header is
	// not a cookie, the CSRF rule does not apply.
	rec := h.admin(http.MethodPost, "/api/admin/dev-token/rotate", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+out.Token)
	})
	if rec.Code != http.StatusOK {
		t.Errorf("admin write with bearer = %d: %s", rec.Code, rec.Body)
	}
	// Not accepted as a cookie, with or without the prefix.
	for _, v := range []string{out.Token, strings.TrimPrefix(out.Token, gateway.AdminSessionBearerPrefix)} {
		if got := h.roleWith(t, &http.Cookie{Name: SessionCookie, Value: v}); got != "anonymous" {
			t.Errorf("bearer as cookie: role %q", got)
		}
	}
	// A session cookie value is not a bearer.
	sess := sessionFrom(t, h.signIn(t))
	if role, code := h.roleWithBearer(t, gateway.AdminSessionBearerPrefix+sess.Value); code != http.StatusUnauthorized {
		t.Errorf("cookie as bearer: %d %q", code, role)
	}
	// Expired.
	h.sso.cfg.Now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	if _, code := h.roleWithBearer(t, out.Token); code != http.StatusUnauthorized {
		t.Errorf("expired bearer = %d, want 401", code)
	}
}

func TestCLITokenTTLCappedBySession(t *testing.T) {
	s, err := New(Config{Issuer: "http://x", ClientID: "x", SessionTTL: time.Hour, CLITokenTTL: 5 * time.Hour,
		Rules: []Rule{{Kind: RuleDomain, Value: "example.com", Role: gateway.AdminSessionAdmin}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.CLITokenTTL != time.Hour {
		t.Errorf("CLITokenTTL = %v, want 1h", s.cfg.CLITokenTTL)
	}
}

func TestCLIBearerRevokedAndRoleChange(t *testing.T) {
	h := newHarness(t, nil)
	out := h.cliToken(t)
	h.sso.cfg.Rules = []Rule{{Kind: RuleEmail, Value: "ops@example.com", Role: gateway.AdminSessionDev}}
	if role, _ := h.roleWithBearer(t, out.Token); role != "dev" {
		t.Errorf("after rule change: role %q, want dev", role)
	}
	h.sso.cfg.Rules = []Rule{{Kind: RuleEmail, Value: "other@example.com", Role: gateway.AdminSessionAdmin}}
	if _, code := h.roleWithBearer(t, out.Token); code != http.StatusUnauthorized {
		t.Errorf("no rule matches: %d, want 401", code)
	}
	h.sso.cfg.Rules = []Rule{{Kind: RuleEmail, Value: "ops@example.com", Role: gateway.AdminSessionAdmin}}
	if _, err := h.gw.RevokeAdminSessions(context.Background(), "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, code := h.roleWithBearer(t, out.Token); code != http.StatusUnauthorized {
		t.Errorf("revoked bearer = %d, want 401", code)
	}
}

// A code taken during the revoke allowance gets no bearer; a bearer
// from a fast clock is ended by the revoke.
func TestCLIRevocationSkew(t *testing.T) {
	h := newHarness(t, nil)
	code := h.cliSignIn(t, "40000", "st").Query().Get("code")
	h.sso.cfg.Now = func() time.Time { return time.Now().Add(20 * time.Second) }
	fast := h.cliToken(t)
	h.sso.cfg.Now = nil
	if _, err := h.gw.RevokeAdminSessions(context.Background(), "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := h.exchange(code, testVerifier); rec.Code != http.StatusForbidden {
		t.Errorf("exchange right after revoke = %d: %s", rec.Code, rec.Body)
	}
	if _, st := h.roleWithBearer(t, fast.Token); st != http.StatusUnauthorized {
		t.Errorf("bearer from a fast clock after revoke = %d", st)
	}
}
