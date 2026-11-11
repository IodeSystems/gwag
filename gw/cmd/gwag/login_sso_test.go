package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/oidc"
	"github.com/iodesystems/gwag/v2/gw/oidc/oidctest"
)

// ssoGateway is a standalone gateway with SSO, as gwag up wires it.
func ssoGateway(t *testing.T) (*httptest.Server, *oidctest.Provider) {
	t.Helper()
	p := oidctest.New(t, "gwag-cli")
	sso, err := oidc.New(oidc.Config{
		Issuer: p.URL, ClientID: "gwag-cli",
		Rules: []oidc.Rule{{Kind: oidc.RuleDomain, Value: "example.com", Role: gateway.AdminSessionDev}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.WithAdminSession(sso), gateway.WithoutMetrics(), gateway.WithAdminDataDir(t.TempDir()))
	t.Cleanup(gw.Close)
	mux := http.NewServeMux()
	sso.Mount(mux, gw)
	adminMux, _, err := gw.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, p
}

// fakeBrowser follows the sign-in like a browser would: the gateway's
// redirect to the provider, the provider's (here: the test's) redirect
// back to the callback, and the callback's redirect to the CLI.
func fakeBrowser(t *testing.T, p *oidctest.Provider, gatewayURL string) func(string) error {
	return func(signIn string) error {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
			if strings.HasPrefix(r.URL.String(), p.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		}}
		go func() {
			resp, err := c.Get(signIn)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			resp.Body.Close()
			authorize := resp.Header.Get("Location")
			if err := p.Expect(authorize); err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			u, _ := url.Parse(authorize)
			cb := gatewayURL + "/auth/callback?code=" + oidctest.Code + "&state=" + url.QueryEscape(u.Query().Get("state"))
			resp, err = c.Get(cb)
			if err != nil {
				t.Errorf("browser callback: %v", err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Request.URL.String(), "http://127.0.0.1:") {
				t.Errorf("browser ended at %s with %d", resp.Request.URL, resp.StatusCode)
			}
		}()
		return nil
	}
}

func TestLoginSSO(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, p := ssoGateway(t)
	prev := openBrowser
	t.Cleanup(func() { openBrowser = prev })
	openBrowser = fakeBrowser(t, p, srv.URL)

	if rc := loginCmd([]string{"--name", "corp", "--gateway", "gw.example:50090", "--endpoint", srv.URL, "--sso"}); rc != 0 {
		t.Fatalf("login --sso rc=%d", rc)
	}
	c, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.resolve("corp")
	if !ok || !strings.HasPrefix(e.Bearer, gateway.AdminSessionBearerPrefix) || e.Endpoint != srv.URL {
		t.Fatalf("saved login = %+v", e)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+e.Bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who struct{ Role string }
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if who.Role != "dev" {
		t.Errorf("saved bearer: role %q, want dev", who.Role)
	}

	// Again, on the current login: no --gateway needed.
	if rc := loginCmd([]string{"--sso"}); rc != 0 {
		t.Fatalf("login --sso on current login rc=%d", rc)
	}
	c, _ = loadCredentials()
	if len(c.Logins) != 1 || c.Logins[0].Bearer == e.Bearer || c.Logins[0].Name != "corp" {
		t.Errorf("re-login = %+v", c.Logins)
	}
}

func TestLoginSSONoBrowserPrintsURL(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, p := ssoGateway(t)
	prev := openBrowser
	t.Cleanup(func() { openBrowser = prev })
	openBrowser = func(string) error { return errors.New("must not be called") }
	var printed strings.Builder
	sim := fakeBrowser(t, p, srv.URL)
	w := writerFunc(func(b []byte) (int, error) {
		printed.Write(b)
		for _, line := range strings.Split(string(b), "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, srv.URL+"/auth/cli?") {
				_ = sim(l)
			}
		}
		return len(b), nil
	})
	res, err := ssoLogin(t.Context(), srv.URL, false, w)
	if err != nil || res.Role != "dev" || res.Email != "ops@example.com" {
		t.Fatalf("ssoLogin = %+v, %v; printed:\n%s", res, err, printed.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestLoginSSOOff(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	err := checkSSO(t.Context(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "SSO is off") {
		t.Fatalf("checkSSO = %v", err)
	}
	if rc := loginCmd([]string{"--gateway", "x:1", "--endpoint", srv.URL, "--sso"}); rc != 1 {
		t.Errorf("rc=%d, want 1", rc)
	}
	if _, err := loadCredentialsAt(credentialsFile); err != nil {
		t.Fatal(err)
	}
}
