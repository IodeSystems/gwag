package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/oidc"
	"github.com/iodesystems/gwag/v2/gw/oidc/oidctest"
)

// A session cookie minted on node A passes on node B; a revocation on
// B ends it on A.
func TestAdminSessionAcrossNodes(t *testing.T) {
	p := oidctest.New(t, "gwag-test")
	newSSO := func() *oidc.SSO {
		s, err := oidc.New(oidc.Config{
			Issuer: p.URL, ClientID: "gwag-test", RedirectURL: "http://gwag.test/auth/callback",
			Rules: []oidc.Rule{{Kind: oidc.RuleDomain, Value: "example.com", Role: gateway.AdminSessionAdmin}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ssoA, ssoB := newSSO(), newSSO()
	a, b := gateway.SharedClusterPair(t,
		[]gateway.Option{gateway.WithAdminSession(ssoA)},
		[]gateway.Option{gateway.WithAdminSession(ssoB)})
	mux := func(g *gateway.Gateway, s *oidc.SSO) *http.ServeMux {
		m := http.NewServeMux()
		s.Mount(m, g)
		adminMux, _, err := g.AdminHumaRouter()
		if err != nil {
			t.Fatal(err)
		}
		m.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
		return m
	}
	muxA, muxB := mux(a, ssoA), mux(b, ssoB)
	serve := func(m *http.ServeMux, r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, r)
		return rec
	}

	// The level watch opens the sessions bucket shortly after start.
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for _, g := range []*gateway.Gateway{a, b} {
		for {
			_, err := g.AdminSessionKey(ctx)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("session key: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ka, _ := a.AdminSessionKey(ctx)
	kb, _ := b.AdminSessionKey(ctx)
	if string(ka) != string(kb) {
		t.Fatal("nodes have different session keys")
	}

	// Sign in on A.
	rec := serve(muxA, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if err := p.Expect(loc); err != nil {
		t.Fatal(err)
	}
	state := loc[strings.Index(loc, "state=")+len("state="):]
	if i := strings.IndexByte(state, '&'); i >= 0 {
		state = state[:i]
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+oidctest.Code+"&state="+state, nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec = serve(muxA, cb)
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidc.SessionCookie && c.Value != "" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatalf("no session from A: %d %s", rec.Code, rec.Body)
	}

	role := func(m *http.ServeMux) string {
		r := httptest.NewRequest(http.MethodGet, "/api/admin/whoami", nil)
		r.AddCookie(sess)
		var out struct{ Role string }
		_ = json.Unmarshal(serve(m, r).Body.Bytes(), &out)
		return out.Role
	}
	if got := role(muxB); got != "admin" {
		t.Fatalf("A's cookie on B: role %q", got)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/admin/sessions/revoke", strings.NewReader(`{"subject":"ops@example.com"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+b.AdminTokenHex())
	if rec := serve(muxB, r); rec.Code != http.StatusOK {
		t.Fatalf("revoke on B: %d %s", rec.Code, rec.Body)
	}
	deadline = time.Now().Add(5 * time.Second)
	for role(muxA) != "anonymous" {
		if time.Now().After(deadline) {
			t.Fatal("revocation on B did not reach A")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
