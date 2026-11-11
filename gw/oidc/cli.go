package oidc

// gwag login --sso: the CLI signs in through the browser and gets a
// session bearer (RFC 8252 loopback redirect).
//
//  1. The CLI listens on 127.0.0.1:P and opens
//     /auth/cli?port=P&state=S&challenge=C, C = base64url(SHA-256(V))
//     for a random verifier V it keeps.
//  2. /auth/cli puts {P, S, C} in the signed flow cookie and starts the
//     normal sign-in.
//  3. The callback stores a one-time code (codeTTL, single use,
//     cluster-wide through the Store) bound to C, and redirects to
//     http://127.0.0.1:P/callback?code=…&state=S. The host is fixed:
//     nothing from the request names it.
//  4. The CLI posts {code, verifier: V} to /auth/cli/token and gets
//     gwo_<signed session payload>, verified by AdminSessionBearer. The
//     role is evaluated per request and the per-subject cutoff
//     revokes it, as for the cookie.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

const (
	// DefaultCLITokenTTL is how long a gwag login --sso bearer lasts
	// when Config.CLITokenTTL is 0.
	DefaultCLITokenTTL = 12 * time.Hour
	// codeTTL is how long the CLI has to trade the code for a bearer.
	codeTTL = 60 * time.Second
	// maxCLIState bounds the state the CLI picks.
	maxCLIState = 128
)

// cliFlow is what /auth/cli puts in the flow cookie.
type cliFlow struct {
	Port      int    `json:"p"`
	State     string `json:"s"`
	Challenge string `json:"c"`
}

// cliCode is the value stored under a one-time code.
type cliCode struct {
	Email     string   `json:"e"`
	Groups    []string `json:"g,omitempty"`
	Challenge string   `json:"c"`
}

// CLITokenResponse is the body of POST /auth/cli/token.
type CLITokenResponse struct {
	Token string `json:"token"`
	Email string `json:"email"`
	Role  string `json:"role"`
	// ExpiresAt is RFC 3339.
	ExpiresAt string `json:"expires_at"`
}

// CLILogin starts gwag login --sso: GET /auth/cli?port&state&challenge.
func (s *SSO) CLILogin(w http.ResponseWriter, r *http.Request) {
	cli, why := parseCLIFlow(r.URL.Query())
	if why != "" {
		errorPage(w, http.StatusBadRequest, "Bad CLI sign-in request", why+". Run gwag login --sso again.")
		return
	}
	s.login(w, r, cli)
}

// parseCLIFlow validates the /auth/cli parameters; why is "" when
// they are good.
func parseCLIFlow(q url.Values) (*cliFlow, string) {
	port, err := strconv.Atoi(q.Get("port"))
	if err != nil || port < 1024 || port > 65535 {
		return nil, "port must be a number from 1024 to 65535"
	}
	state := q.Get("state")
	if state == "" || len(state) > maxCLIState || !isBase64URL(state) {
		return nil, fmt.Sprintf("state must be 1 to %d characters of A-Z a-z 0-9 - _", maxCLIState)
	}
	ch := q.Get("challenge")
	if len(ch) != 43 || !isBase64URL(ch) {
		return nil, "challenge must be base64url(SHA-256(verifier)), 43 characters"
	}
	return &cliFlow{Port: port, State: state, Challenge: ch}, ""
}

func isBase64URL(v string) bool {
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// cliCodeID is the store key of a code: its hash, so the store does
// not hold the code itself.
func cliCodeID(code string) string {
	sum := sha256.Sum256([]byte("gwag-cli-code\x00" + code))
	return "cli." + hex.EncodeToString(sum[:])
}

// cliRedirect is where the browser goes with the code. The host is
// always 127.0.0.1.
func cliRedirect(cli *cliFlow, code string) string {
	q := url.Values{"code": {code}, "state": {cli.State}}
	return "http://127.0.0.1:" + strconv.Itoa(cli.Port) + "/callback?" + q.Encode()
}

// finishCLI ends a CLI sign-in in the callback: no cookie, a one-time
// code for the loopback listener.
func (s *SSO) finishCLI(w http.ResponseWriter, r *http.Request, cli *cliFlow, id identity) {
	st := s.storeOf()
	if st == nil {
		errorPage(w, http.StatusServiceUnavailable, "Single sign-on is not available", errNoStore.Error()+".")
		return
	}
	b, _ := json.Marshal(cliCode{Email: id.Email, Groups: id.Groups, Challenge: cli.Challenge})
	code := randomString()
	if err := st.PutAdminLoginCode(r.Context(), cliCodeID(code), b, codeTTL); err != nil {
		slog.Warn("sso: storing CLI code failed", "err", err)
		errorPage(w, http.StatusServiceUnavailable, "CLI sign-in failed", "The gateway could not store the sign-in code. Run gwag login --sso again.")
		return
	}
	slog.Info("sso: CLI sign-in", "email", id.Email, "role", id.Role)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, cliRedirect(cli, code), http.StatusSeeOther)
}

// CLIToken trades a one-time code and its verifier for a bearer:
// POST /auth/cli/token {"code", "verifier"}. The code is spent by the
// first attempt, right or wrong.
func (s *SSO) CLIToken(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	var in struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Code == "" || in.Verifier == "" {
		fail(http.StatusBadRequest, "body must be JSON {code, verifier}")
		return
	}
	st := s.storeOf()
	if st == nil {
		fail(http.StatusServiceUnavailable, errNoStore.Error())
		return
	}
	key, err := st.AdminSessionKey(r.Context())
	if err != nil {
		slog.Warn("sso: session store unavailable", "err", err)
		fail(http.StatusServiceUnavailable, "single sign-on is not available")
		return
	}
	raw, ok, err := st.TakeAdminLoginCode(r.Context(), cliCodeID(in.Code))
	if err != nil {
		slog.Warn("sso: reading CLI code failed", "err", err)
		fail(http.StatusServiceUnavailable, "the gateway could not read the sign-in code; run gwag login --sso again")
		return
	}
	var c cliCode
	if !ok || json.Unmarshal(raw, &c) != nil {
		fail(http.StatusBadRequest, "code unknown, used or expired; run gwag login --sso again")
		return
	}
	sum := sha256.Sum256([]byte(in.Verifier))
	if !strEqual(base64.RawURLEncoding.EncodeToString(sum[:]), c.Challenge) {
		slog.Warn("sso: CLI verifier mismatch", "email", c.Email)
		fail(http.StatusBadRequest, "verifier does not match; run gwag login --sso again")
		return
	}
	role, ok := s.roleOf(c.Email, c.Groups)
	if !ok {
		fail(http.StatusForbidden, c.Email+" has no role on this gateway")
		return
	}
	now := s.now()
	exp := now.Add(s.cfg.CLITokenTTL)
	tok := gateway.AdminSessionBearerPrefix + sign(key, "cli-bearer", sessionPayload{
		Email: c.Email, Groups: c.Groups, Exp: exp.Unix(), Issued: now.UnixMilli(),
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(CLITokenResponse{
		Token: tok, Email: c.Email, Role: string(role), ExpiresAt: exp.UTC().Format(time.RFC3339),
	})
}

// AdminSessionBearer implements gateway.AdminSessionBearerAuthenticator:
// a gwag login --sso bearer, checked like the cookie (signature,
// expiry, cutoff, a rule gives it a role now).
func (s *SSO) AdminSessionBearer(ctx context.Context, bearer string) (gateway.AdminSession, bool) {
	v, ok := strings.CutPrefix(bearer, gateway.AdminSessionBearerPrefix)
	if !ok || v == "" {
		return gateway.AdminSession{}, false
	}
	sp, ok := s.verifySession(ctx, "cli-bearer", v)
	if !ok {
		return gateway.AdminSession{}, false
	}
	role, ok := s.roleOf(sp.Email, sp.Groups)
	if !ok {
		return gateway.AdminSession{}, false
	}
	return gateway.AdminSession{Subject: sp.Email, Role: role}, true
}
