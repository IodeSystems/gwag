package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixedSession accepts any request carrying cookie "s".
type fixedSession struct{ s AdminSession }

func (f fixedSession) AdminSession(r *http.Request) (AdminSession, bool) {
	if _, err := r.Cookie("s"); err != nil {
		return AdminSession{}, false
	}
	return f.s, true
}

func TestAdminSessionAllowed(t *testing.T) {
	ws := func(origin, host, fwd string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/graphql", nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if fwd != "" {
			r.Header.Set("X-Forwarded-Host", fwd)
		}
		return r
	}
	req := func(method, site string) *http.Request {
		r := httptest.NewRequest(method, "http://gw.test/api/admin/x", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		return r
	}
	for name, c := range map[string]struct {
		r  *http.Request
		ok bool
	}{
		"GET no header":          {req(http.MethodGet, ""), true},
		"GET cross-site":         {req(http.MethodGet, "cross-site"), true},
		"HEAD":                   {req(http.MethodHead, ""), true},
		"POST same-origin":       {req(http.MethodPost, "same-origin"), true},
		"POST no header":         {req(http.MethodPost, ""), false},
		"POST same-site":         {req(http.MethodPost, "same-site"), false},
		"POST cross-site":        {req(http.MethodPost, "cross-site"), false},
		"POST none":              {req(http.MethodPost, "none"), false},
		"DELETE same-origin":     {req(http.MethodDelete, "same-origin"), true},
		"OPTIONS no header":      {req(http.MethodOptions, ""), false},
		"WS same host":           {ws("http://gw.test", "gw.test", ""), true},
		"WS same host, port":     {ws("https://gw.test:8443", "gw.test:8443", ""), true},
		"WS forwarded host":      {ws("https://gw.example.com", "127.0.0.1:8080", "gw.example.com"), true},
		"WS other origin":        {ws("https://evil.example", "gw.test", ""), false},
		"WS no origin":           {ws("", "gw.test", ""), false},
		"WS origin null":         {ws("null", "gw.test", ""), false},
		"WS port differs":        {ws("http://gw.test:9999", "gw.test", ""), false},
		"WS other origin + same": {ws("https://evil.example", "gw.test", "gw.other"), false},
	} {
		if got := adminSessionAllowed(c.r) == ""; got != c.ok {
			t.Errorf("%s: allowed=%v, want %v (%q)", name, got, c.ok, adminSessionAllowed(c.r))
		}
	}
}

// The admin_events subscription takes the role from a session cookie
// on the handshake when no bearer came, only with a matching Origin.
func TestAdminRoleFromRequestContextSession(t *testing.T) {
	g := New(WithoutMetrics(), WithAdminSession(fixedSession{AdminSession{Subject: "a@b.c", Role: AdminSessionDev}}))
	t.Cleanup(g.Close)
	role := func(r *http.Request) adminRole {
		return g.adminRoleFromRequestContext(context.WithValue(context.Background(), httpRequestCtxKey{}, r), "SUBSCRIBE", "x")
	}
	r := httptest.NewRequest(http.MethodGet, "http://gw.test/api/graphql", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Origin", "http://gw.test")
	r.AddCookie(&http.Cookie{Name: "s", Value: "1"})
	if got := role(r); got != adminRoleDev {
		t.Errorf("same origin: %v", got)
	}
	r.Header.Set("Origin", "http://evil.example")
	if got := role(r); got != adminRoleAnonymous {
		t.Errorf("cross origin: %v", got)
	}
	r.Header.Set("Authorization", "Bearer "+g.AdminTokenHex())
	if got := role(r); got != adminRoleAdmin {
		t.Errorf("bearer wins: %v", got)
	}
}

// A role the gateway does not know is no session; the subject is
// recorded as the audit identity.
func TestAdminSessionRoleAndSubject(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = adminIdentityFromContext(r.Context()) })
	for role, want := range map[AdminSessionRole]int{AdminSessionAdmin: 200, AdminSessionDev: 403, "root": 401} {
		g := New(WithoutMetrics(), WithAdminSession(fixedSession{AdminSession{Subject: " a@b.c\n", Role: role}}))
		t.Cleanup(g.Close)
		r := httptest.NewRequest(http.MethodPost, "/admin/anything", nil)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(&http.Cookie{Name: "s", Value: "1"})
		rec := httptest.NewRecorder()
		seen = ""
		g.AdminMiddleware(next).ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("role %q: %d, want %d", role, rec.Code, want)
		}
		if want == 200 && seen != "admin:a@b.c" {
			t.Errorf("identity %q", seen)
		}
	}
}

// Standalone: the key and the cutoffs persist in the data dir.
func TestAdminSessionStoreStandalone(t *testing.T) {
	dir := t.TempDir()
	g := New(WithoutMetrics(), WithAdminDataDir(dir))
	t.Cleanup(g.Close)
	ctx := context.Background()
	k1, err := g.AdminSessionKey(ctx)
	if err != nil || len(k1) != 32 {
		t.Fatalf("key: %x %v", k1, err)
	}
	if nb, err := g.AdminSessionNotBefore(ctx, "a@b.c"); err != nil || nb != 0 {
		t.Fatalf("nb before: %d %v", nb, err)
	}
	at, err := g.RevokeAdminSessions(ctx, "A@B.c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RevokeAdminSessions(ctx, " "); err == nil {
		t.Error("empty subject accepted")
	}

	g2 := New(WithoutMetrics(), WithAdminDataDir(dir))
	t.Cleanup(g2.Close)
	k2, _ := g2.AdminSessionKey(ctx)
	if string(k1) != string(k2) {
		t.Error("key not persisted")
	}
	if nb, _ := g2.AdminSessionNotBefore(ctx, "a@b.c"); nb != at {
		t.Errorf("cutoff not persisted: %d, want %d", nb, at)
	}
	if nb, _ := g2.AdminSessionNotBefore(ctx, "other@b.c"); nb != 0 {
		t.Errorf("other subject: %d", nb)
	}
	time.Sleep(2 * time.Millisecond)
	all, _ := g2.RevokeAdminSessions(ctx, AdminSessionsAll)
	if nb, _ := g2.AdminSessionNotBefore(ctx, "other@b.c"); nb != all {
		t.Errorf("revoke all: %d, want %d", nb, all)
	}
}

// Below levelAdminSessions a cluster refuses sessions; the finalize
// opens the bucket and they work.
func TestAdminSessionsFeatureLevel(t *testing.T) {
	cl := newLevelCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers := seedLegacyPeer(t, cl, "PREVNODE", "prev")
	if _, err := peers.Put(ctx, "PREVNODE", []byte(`{"node_id":"PREVNODE","name":"prev","joined_unix_ms":1,"version":"v2.12.0","level_max":1}`)); err != nil {
		t.Fatal(err)
	}
	g := levelGateway(t, cl)
	if _, err := g.startClusterTracking(context.Background()); err != nil {
		t.Fatalf("startClusterTracking: %v", err)
	}
	if got := g.ClusterLevel(); got != levelAdminSessions-1 {
		t.Fatalf("level %d, want %d", got, levelAdminSessions-1)
	}
	if _, err := g.AdminSessionKey(ctx); !errors.Is(err, errAdminSessionsLevel) {
		t.Fatalf("key below the level: %v", err)
	}
	if _, err := g.RevokeAdminSessions(ctx, "a@b.c"); !errors.Is(err, errAdminSessionsLevel) {
		t.Fatalf("revoke below the level: %v", err)
	}
	// The admin op says so.
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/sessions/revoke", strings.NewReader(`{"subject":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adminMux.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Errorf("revoke op below the level = %d: %s", rec.Code, rec.Body)
	}
	if _, err := cl.JS.KeyValue(ctx, adminSessionsBucketName); err == nil {
		t.Error("bucket exists below the level")
	}

	if err := peers.Delete(ctx, "PREVNODE"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.finalizeClusterLevel(ctx, 0, false); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	key, err := g.AdminSessionKey(ctx)
	if err != nil || len(key) == 0 {
		t.Fatalf("key after finalize: %v", err)
	}
	at, err := g.RevokeAdminSessions(ctx, "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	if nb, err := g.AdminSessionNotBefore(ctx, "A@b.c"); err != nil || nb != at {
		t.Fatalf("cutoff %d %v, want %d", nb, err, at)
	}
}

func TestAdminLoginCode(t *testing.T) {
	g := New(WithoutMetrics())
	t.Cleanup(g.Close)
	ctx := context.Background()
	if err := g.PutAdminLoginCode(ctx, "c1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := g.PutAdminLoginCode(ctx, "c1", []byte("v2"), time.Minute); err == nil {
		t.Error("duplicate id accepted")
	}
	if v, ok, err := g.TakeAdminLoginCode(ctx, "c1"); err != nil || !ok || string(v) != "v1" {
		t.Fatalf("take = %q %v %v", v, ok, err)
	}
	if _, ok, _ := g.TakeAdminLoginCode(ctx, "c1"); ok {
		t.Error("code taken twice")
	}
	_ = g.PutAdminLoginCode(ctx, "c2", []byte("v"), 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if _, ok, _ := g.TakeAdminLoginCode(ctx, "c2"); ok {
		t.Error("expired code taken")
	}
}

// A session bearer goes to the bearer authenticator only; without one
// it is 401, never the token check.
func TestAdminSessionBearerPrefix(t *testing.T) {
	g := New(WithoutMetrics(), WithAdminSession(fixedSession{AdminSession{Subject: "a@b.c", Role: AdminSessionAdmin}}))
	t.Cleanup(g.Close)
	role, _, outcome, status := g.authenticateAdminSubject(context.Background(), AdminSessionBearerPrefix+"x", "GET", "/admin/x", "", true)
	if status != http.StatusUnauthorized || role != adminRoleAnonymous || outcome != "denied_session_bearer" {
		t.Errorf("got %v %q %d", role, outcome, status)
	}
}
