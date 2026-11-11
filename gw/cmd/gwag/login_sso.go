package main

// gwag login --sso: browser sign-in with a loopback redirect (RFC 8252).
// The CLI listens on 127.0.0.1, the browser signs in at the gateway,
// the gateway redirects to the listener with a one-time code, and the
// CLI trades code + PKCE verifier for a session bearer. Gateway side:
// gw/oidc/cli.go.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/oidc"
)

// ssoLoginTimeout is how long the CLI waits for the browser.
var ssoLoginTimeout = 5 * time.Minute

// openBrowser opens u in the desktop browser. Tests replace it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

type ssoLoginResult = oidc.CLITokenResponse

var ssoHTTP = &http.Client{Timeout: 30 * time.Second}

// ssoLogin signs in at endpoint and returns the bearer. Progress goes
// to out.
func ssoLogin(ctx context.Context, endpoint string, browser bool, out io.Writer) (ssoLoginResult, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if err := checkSSO(ctx, endpoint); err != nil {
		return ssoLoginResult{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ssoLoginResult{}, fmt.Errorf("loopback listener: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	state, verifier := randomB64(16), randomB64(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	type result struct {
		code string
		err  error
	}
	got := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			// Not our sign-in; keep waiting for the real one.
			http.Error(w, "This does not match the gwag login in progress.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "gwag: signed in. You can close this tab.\n")
		select {
		case got <- result{code: q.Get("code")}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case got <- result{err: err}:
			default:
			}
		}
	}()
	defer srv.Close()

	signIn := endpoint + "/auth/cli?" + url.Values{
		"port": {strconv.Itoa(port)}, "state": {state}, "challenge": {challenge},
	}.Encode()
	fmt.Fprintf(out, "Sign in at:\n  %s\n", signIn)
	if browser {
		if err := openBrowser(signIn); err != nil {
			fmt.Fprintf(out, "could not open a browser (%v); open the URL above\n", err)
		}
	}
	fmt.Fprintf(out, "Waiting for the browser on 127.0.0.1:%d (%s)...\n", port, ssoLoginTimeout)

	ctx, cancel := context.WithTimeout(ctx, ssoLoginTimeout)
	defer cancel()
	var code string
	select {
	case r := <-got:
		if r.err != nil {
			return ssoLoginResult{}, r.err
		}
		code = r.code
	case <-ctx.Done():
		return ssoLoginResult{}, errors.New("no sign-in within " + ssoLoginTimeout.String())
	}
	return exchangeSSOCode(ctx, endpoint, code, verifier)
}

// checkSSO fails clearly when the gateway has no SSO.
func checkSSO(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/auth/whoami", nil)
	if err != nil {
		return err
	}
	resp, err := ssoHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	var who struct {
		SSO   bool   `json:"sso"`
		Error string `json:"error"`
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("SSO is off on %s (no /auth/whoami); start the gateway with --sso-issuer, or use --token", endpoint)
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&who) != nil {
		return fmt.Errorf("%s/auth/whoami: unexpected answer (HTTP %d)", endpoint, resp.StatusCode)
	}
	if !who.SSO {
		return fmt.Errorf("SSO is off on %s; use --token", endpoint)
	}
	if who.Error != "" {
		return fmt.Errorf("SSO on %s is not available: %s", endpoint, who.Error)
	}
	return nil
}

func exchangeSSOCode(ctx context.Context, endpoint, code, verifier string) (ssoLoginResult, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/auth/cli/token", bytes.NewReader(body))
	if err != nil {
		return ssoLoginResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ssoHTTP.Do(req)
	if err != nil {
		return ssoLoginResult{}, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return ssoLoginResult{}, fmt.Errorf("token exchange: %s", e.Error)
		}
		return ssoLoginResult{}, fmt.Errorf("token exchange: HTTP %d", resp.StatusCode)
	}
	var out ssoLoginResult
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return ssoLoginResult{}, errors.New("token exchange: no token in the answer")
	}
	return out, nil
}

// endSSOBearer ends a gwag login --sso bearer on its gateway (POST
// /auth/logout). Best effort: the login is removed locally either way.
func endSSOBearer(e loginEntry) {
	if !strings.HasPrefix(e.Bearer, gateway.AdminSessionBearerPrefix) || e.Endpoint == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.Endpoint, "/")+"/auth/logout", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+e.Bearer)
	resp, err := ssoHTTP.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not end the SSO session of %q on %s: %v\n", e.Name, e.Endpoint, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "warn: could not end the SSO session of %q on %s: HTTP %d\n", e.Name, e.Endpoint, resp.StatusCode)
		return
	}
	fmt.Printf("ended SSO session of %q on %s\n", e.Name, e.Endpoint)
}

func randomB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
