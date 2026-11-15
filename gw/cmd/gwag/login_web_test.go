package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

// webGateway is a standalone gateway without SSO, as gwag up wires it.
func webGateway(t *testing.T) (*httptest.Server, *gateway.Gateway) {
	t.Helper()
	gw := gateway.New(gateway.WithoutMetrics(), gateway.WithAdminDataDir(t.TempDir()))
	t.Cleanup(gw.Close)
	mux := http.NewServeMux()
	gw.MountAdminSessions(mux)
	adminMux, _, err := gw.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, gw
}

var codeURL = regexp.MustCompile(`(https?://\S+)/#code=(\S+)`)

func TestWebLogin(t *testing.T) {
	srv, gw := webGateway(t)
	prev := openBrowser
	t.Cleanup(func() { openBrowser = prev })
	var opened string
	openBrowser = func(u string) error { opened = u; return nil }

	var out bytes.Buffer
	if err := webLogin(t.Context(), srv.URL, gw.DevTokenHex(), "http://192.168.1.76:8080/ignored?x", true, &out); err != nil {
		t.Fatal(err)
	}
	m := codeURL.FindStringSubmatch(out.String())
	if m == nil || m[1] != "http://192.168.1.76:8080" || opened != m[0] || !strings.Contains(out.String(), "role dev") {
		t.Fatalf("printed %q, opened %q", out.String(), opened)
	}
	code, _ := url.PathUnescape(m[2])
	redeem := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/session", strings.NewReader(`{"code":"`+code+`"}`))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := redeem(); s != http.StatusOK {
		t.Fatalf("redeem: %d", s)
	}
	if s := redeem(); s != http.StatusUnauthorized {
		t.Errorf("second redeem: %d", s)
	}

	// --no-browser prints only; a wrong bearer fails clearly.
	openBrowser = func(string) error { return errors.New("must not be called") }
	out.Reset()
	if err := webLogin(t.Context(), srv.URL, gw.AdminTokenHex(), "", false, &out); err != nil || !strings.Contains(out.String(), srv.URL+"/#code=") {
		t.Fatalf("no-browser: %v %q", err, out.String())
	}
	if err := webLogin(t.Context(), srv.URL, "00ff", "", false, io.Discard); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong bearer: %v", err)
	}
}

func TestWebSignInURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:8080":        "http://127.0.0.1:8080/#code=c",
		"http://127.0.0.1:8080/":       "http://127.0.0.1:8080/#code=c",
		"https://gw.example.com/ui/#x": "https://gw.example.com/#code=c",
		" http://192.168.1.76:18680 ":  "http://192.168.1.76:18680/#code=c",
		"192.168.1.76:8080":            "",
		"ftp://gw.example.com":         "",
		"http://":                      "",
	} {
		got, err := webSignInURL(base, "c")
		if want == "" {
			if err == nil {
				t.Errorf("%q: accepted as %q", base, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", base, got, err, want)
		}
	}
}

func TestLoginWebFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	for name, args := range map[string][]string{
		"with --token":    {"--web", "--token", "ab"},
		"with --sso":      {"--web", "--sso"},
		"with --gateway":  {"--web", "--gateway", "x:1"},
		"with --primary":  {"--web", "--primary"},
		"--web-url alone": {"--web-url", "http://x:1", "--gateway", "x:1"},
		"--context alone": {"--context", "a", "--gateway", "x:1"},
		"no logins":       {"--web"},
		"unknown context": {"--web", "--context", "nope"},
		"bad flag":        {"--web", "--bogus"},
	} {
		if rc := loginCmd(args); rc != 2 {
			t.Errorf("%s: rc=%d, want 2", name, rc)
		}
	}
	if _, err := os.Stat(credentialsFile); !os.IsNotExist(err) {
		t.Errorf("flag errors wrote credentials: %v", err)
	}
	// A login without a bearer cannot sign a browser in.
	if rc := loginCmd([]string{"--name", "nb", "--gateway", "x:1"}); rc != 0 {
		t.Fatal(rc)
	}
	if rc := loginCmd([]string{"--web", "--context", "nb"}); rc != 2 {
		t.Errorf("no bearer: rc=%d, want 2", rc)
	}
}

// --web uses the named login and changes no credentials.
func TestLoginWebUsesContext(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, gw := webGateway(t)
	prev := openBrowser
	t.Cleanup(func() { openBrowser = prev })
	openBrowser = func(string) error { return errors.New("must not be called") }
	if rc := loginCmd([]string{"--name", "a", "--gateway", "x:1", "--endpoint", srv.URL, "--token", gw.AdminTokenHex()}); rc != 0 {
		t.Fatal(rc)
	}
	if rc := loginCmd([]string{"--name", "b", "--gateway", "x:1", "--endpoint", "http://127.0.0.1:1", "--token", "00"}); rc != 0 {
		t.Fatal(rc)
	}
	before, _ := os.ReadFile(credentialsFile)
	if rc := loginCmd([]string{"--web", "--no-browser", "--context", "a"}); rc != 0 {
		t.Errorf("--web --context a: rc=%d", rc)
	}
	if rc := loginCmd([]string{"--web", "--no-browser", "--name", "b"}); rc != 1 {
		t.Errorf("--web --name b (unreachable): rc=%d, want 1", rc)
	}
	after, _ := os.ReadFile(credentialsFile)
	if !bytes.Equal(before, after) {
		t.Error("--web changed the credentials file")
	}
}

func TestServiceLoginWeb(t *testing.T) {
	env, _, out := newTestEnv(t, "linux")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(env.home, "cfg"))
	if err := env.install([]string{"--name", "lw", "--", "--addr", "0.0.0.0:18780"}); err != nil {
		t.Fatal(err)
	}
	const tok = "deadbeefdeadbeefdeadbeefdeadbeef"
	if err := os.WriteFile(filepath.Join(env.installedDataDir("lw"), "admin-token"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got struct {
		endpoint, bearer, base string
		browser                bool
	}
	env.webLogin = func(_ context.Context, endpoint, bearer, base string, browser bool, _ io.Writer) error {
		got.endpoint, got.bearer, got.base, got.browser = endpoint, bearer, base, browser
		return nil
	}
	if err := env.login([]string{"--name", "lw", "--web", "--no-browser", "--web-url", "http://192.168.1.76:18780"}); err != nil {
		t.Fatal(err)
	}
	if got.endpoint != "http://localhost:18780" || got.bearer != tok || got.base != "http://192.168.1.76:18780" || got.browser {
		t.Errorf("got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(env.home, "cfg", "gwag", "credentials.json")); !os.IsNotExist(err) {
		t.Errorf("--web saved a context: %v", err)
	}
	for _, args := range [][]string{
		{"--name", "lw", "--web-url", "http://x:1"},
		{"--name", "lw", "--no-browser"},
		{"--name", "lw", "--web", "--primary"},
	} {
		out.Reset()
		if err := env.login(args); !errors.Is(err, errUsage) {
			t.Errorf("%v: err %v, want usage", args, err)
		}
	}
}
