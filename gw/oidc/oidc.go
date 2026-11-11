// Package oidc signs people in to the gwag admin UI and API with an
// OpenID Connect provider.
//
// The flow is the authorization code flow with PKCE (S256) and a
// nonce. The flow state rides in a signed cookie for 10 minutes. A
// successful sign-in sets a session cookie: HMAC-signed {email,
// groups, issued, expiry}, HttpOnly, SameSite=Lax. The role is not in
// the cookie: the rules are evaluated on every request against the
// email and groups it holds, so a changed rule applies at once.
//
// The gateway stores the signing key and the per-subject revocation
// cutoffs (gateway.AdminSessionKey, AdminSessionNotBefore), so a
// cookie minted on one cluster node passes on every other, and a
// revocation reaches all of them. The gateway also applies the CSRF
// rule to cookie-authenticated requests (gateway.WithAdminSession).
//
// gwag login --sso signs the CLI in through the same flow (cli.go):
// the browser finishes at a loopback listener of the CLI, which trades
// a one-time code for a session bearer (gateway.AdminSessionBearerPrefix).
//
// Wiring:
//
//	sso, err := oidc.New(cfg)
//	g := gateway.New(gateway.WithAdminSession(sso), ...)
//	sso.Mount(mux, g)                       // /auth/login, /auth/callback, /auth/logout, /auth/whoami, /auth/cli, /auth/cli/token
//	g.AddOpenAPIBytes(adminSpec, gateway.As("admin"), gateway.To(...),
//		gateway.ForwardHeaders(gateway.AdminForwardHeaders...))
//
// The gateway core does not import this package, so go-oidc and
// x/oauth2 are linked only into binaries that use it.
package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

const (
	// SessionCookie carries the signed session.
	SessionCookie = "gwag_session"
	// stateCookie carries state, nonce and the PKCE verifier between
	// /auth/login and /auth/callback.
	stateCookie = "gwag_oidc"

	// DefaultSessionTTL is how long a session lasts when
	// Config.SessionTTL is 0.
	DefaultSessionTTL = 12 * time.Hour
	stateTTL          = 10 * time.Minute
	// clockSkew is how far a provider's clock may run ahead of ours.
	clockSkew = time.Minute
)

// oauthErrorCode is the form of an error code the callback page may
// show.
var oauthErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// DefaultScopes are requested when Config.Scopes is empty.
var DefaultScopes = []string{"openid", "email", "profile"}

// Store holds what every gateway node shares. *gateway.Gateway is one.
type Store interface {
	// AdminSessionKey is the key that signs session and state cookies.
	AdminSessionKey(ctx context.Context) ([]byte, error)
	// AdminSessionNotBefore is subject's revocation cutoff, unix ms.
	AdminSessionNotBefore(ctx context.Context, subject string) (int64, error)
	// PutAdminLoginCode and TakeAdminLoginCode hold the one-time
	// codes of gwag login --sso; a code put on one node is taken at
	// most once, on any node.
	PutAdminLoginCode(ctx context.Context, id string, value []byte, ttl time.Duration) error
	TakeAdminLoginCode(ctx context.Context, id string) ([]byte, bool, error)
}

// Config is the provider and the role rules.
type Config struct {
	Issuer   string
	ClientID string
	// ClientSecret is empty for a public client (PKCE only).
	ClientSecret string
	// RedirectURL is the callback registered at the provider. Empty:
	// /auth/callback on the address the browser used (RequestOrigin).
	RedirectURL string
	// Scopes default to DefaultScopes.
	Scopes []string
	// AllowUnverifiedEmail admits an email the provider has not marked
	// email_verified. Off by default: on a provider that lets users
	// type any address, an unverified email names nobody.
	AllowUnverifiedEmail bool
	// SessionTTL defaults to DefaultSessionTTL.
	SessionTTL time.Duration
	// CLITokenTTL is how long a gwag login --sso bearer lasts;
	// default DefaultCLITokenTTL, never longer than SessionTTL.
	CLITokenTTL time.Duration
	// GroupsClaim is the claim that lists the groups; default "groups".
	GroupsClaim string
	// Rules decide the role, first match wins. No match: no access.
	Rules []Rule

	// HTTPClient is used for discovery, JWKS, token and userinfo.
	// Nil: a client with a 15 s timeout.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// SSO serves /auth/* and reads session cookies. It implements
// gateway.AdminSessionAuthenticator.
type SSO struct {
	cfg   Config
	store atomic.Pointer[storeBox]

	mu       sync.Mutex
	provider *oidc.Provider
}

type storeBox struct{ s Store }

// New validates cfg and returns the SSO. Mount must be called before
// it accepts sessions.
func New(cfg Config) (*SSO, error) {
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is empty")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oidc: client id is empty")
	}
	if len(cfg.Rules) == 0 {
		return nil, errors.New("oidc: no role rules; every sign-in would be refused")
	}
	for i, r := range cfg.Rules {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("oidc: rule %d: %w", i+1, err)
		}
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultScopes
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.CLITokenTTL <= 0 {
		cfg.CLITokenTTL = DefaultCLITokenTTL
	}
	cfg.CLITokenTTL = min(cfg.CLITokenTTL, cfg.SessionTTL)
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	return &SSO{cfg: cfg}, nil
}

// Mount registers /auth/login, /auth/callback, /auth/logout,
// /auth/whoami, /auth/cli and /auth/cli/token on mux and sets the
// store the sessions are checked against.
func (s *SSO) Mount(mux *http.ServeMux, store Store) {
	s.store.Store(&storeBox{store})
	mux.HandleFunc("GET /auth/login", s.Login)
	mux.HandleFunc("GET /auth/callback", s.Callback)
	mux.HandleFunc("POST /auth/logout", s.Logout)
	mux.HandleFunc("GET /auth/whoami", s.WhoAmI)
	mux.HandleFunc("GET /auth/cli", s.CLILogin)
	mux.HandleFunc("POST /auth/cli/token", s.CLIToken)
}

func (s *SSO) storeOf() Store {
	if b := s.store.Load(); b != nil {
		return b.s
	}
	return nil
}

var errNoStore = errors.New("oidc: not mounted")

func (s *SSO) key(ctx context.Context) ([]byte, error) {
	st := s.storeOf()
	if st == nil {
		return nil, errNoStore
	}
	return st.AdminSessionKey(ctx)
}

func (s *SSO) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

var defaultClient = &http.Client{Timeout: 15 * time.Second}

// ctx attaches the HTTP client the way go-oidc and oauth2 expect.
func (s *SSO) ctx(parent context.Context) context.Context {
	c := s.cfg.HTTPClient
	if c == nil {
		c = defaultClient
	}
	return oidc.ClientContext(parent, c)
}

// discover returns the provider, running discovery until it works
// once. A failure is not cached. Background context: the provider
// keeps it for JWKS fetches after the request ends.
//
// go-oidc wants the configured issuer to equal the provider's byte
// for byte; a trailing slash is a common difference (Zitadel names
// itself without one, Auth0 with one), so the other form is tried
// before giving up.
func (s *SSO) discover() (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		return s.provider, nil
	}
	issuer := s.cfg.Issuer
	p, err := oidc.NewProvider(s.ctx(context.Background()), issuer)
	if err != nil {
		alt := strings.TrimSuffix(issuer, "/")
		if alt == issuer {
			alt += "/"
		}
		p2, err2 := oidc.NewProvider(s.ctx(context.Background()), alt)
		if err2 != nil {
			return nil, err
		}
		p = p2
	}
	s.provider = p
	return p, nil
}

// RequestOrigin is scheme://host as the browser addressed the
// gateway: the proxy's X-Forwarded-Proto and X-Forwarded-Host when
// sent, else the connection's. A client can set those headers; the
// result is used only for the callback URL, which the provider must
// already have registered, so a forged one fails at the provider.
func RequestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

func (s *SSO) redirectFor(r *http.Request) string {
	if s.cfg.RedirectURL != "" {
		return s.cfg.RedirectURL
	}
	return RequestOrigin(r) + "/auth/callback"
}

func (s *SSO) oauthConfig(p *oidc.Provider, redirect string) *oauth2.Config {
	ep := p.Endpoint()
	if s.cfg.ClientSecret == "" {
		// Public client: PKCE is the proof, client_id in the body.
		// Auto-detect would send Basic "id:" first, which some
		// providers refuse for a PKCE-only client.
		ep.AuthStyle = oauth2.AuthStyleInParams
	}
	return &oauth2.Config{
		ClientID:     s.cfg.ClientID,
		ClientSecret: s.cfg.ClientSecret,
		RedirectURL:  redirect,
		Endpoint:     ep,
		Scopes:       s.cfg.Scopes,
	}
}

type statePayload struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Exp      int64  `json:"x"`
	// CLI is set when gwag login --sso started the flow (/auth/cli).
	CLI *cliFlow `json:"c,omitempty"`
}

type sessionPayload struct {
	Email string `json:"e"`
	// Groups are the groups the rules name that the person was in at
	// sign-in. Other groups are dropped: a provider can send hundreds,
	// and a cookie holds 4 KB.
	Groups []string `json:"g,omitempty"`
	Exp    int64    `json:"x"`
	// Issued is unix ms, checked against the NotBefore cutoff.
	Issued int64 `json:"i"`
}

// Login starts the flow: GET /auth/login.
func (s *SSO) Login(w http.ResponseWriter, r *http.Request) {
	s.login(w, r, nil)
}

// login starts the flow; cli is set for gwag login --sso.
func (s *SSO) login(w http.ResponseWriter, r *http.Request, cli *cliFlow) {
	key, err := s.key(r.Context())
	if err != nil {
		slog.Warn("sso: session store unavailable", "err", err)
		errorPage(w, http.StatusServiceUnavailable, "Single sign-on is not available", err.Error()+". Sign in with a token meanwhile.")
		return
	}
	p, err := s.discover()
	if err != nil {
		slog.Warn("sso: provider discovery failed", "issuer", s.cfg.Issuer, "err", err)
		errorPage(w, http.StatusBadGateway, "Sign-in provider unreachable",
			"The gateway could not read the provider's configuration at "+s.cfg.Issuer+": "+err.Error()+". Sign in with a token meanwhile.")
		return
	}
	sp := statePayload{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Exp:      s.now().Add(stateTTL).Unix(),
		CLI:      cli,
	}
	http.SetCookie(w, &http.Cookie{
		Name:  stateCookie,
		Value: sign(key, "oidc-state", sp),
		Path:  "/auth/",
		// Lax, not Strict: the callback is a top-level navigation from
		// the provider; a Strict cookie would not be sent on it.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   s.secure(r),
		MaxAge:   int(stateTTL.Seconds()),
	})
	u := s.oauthConfig(p, s.redirectFor(r)).AuthCodeURL(sp.State, oidc.Nonce(sp.Nonce), oauth2.S256ChallengeOption(sp.Verifier))
	http.Redirect(w, r, u, http.StatusFound)
}

// Callback finishes the flow: GET /auth/callback.
func (s *SSO) Callback(w http.ResponseWriter, r *http.Request) {
	key, err := s.key(r.Context())
	if err != nil {
		errorPage(w, http.StatusServiceUnavailable, "Single sign-on is not available", err.Error()+".")
		return
	}
	q := r.URL.Query()
	var sp statePayload
	c, err := r.Cookie(stateCookie)
	flowOK := err == nil && verify(key, "oidc-state", c.Value, &sp) && sp.Exp >= s.now().Unix()
	stateOK := flowOK && strEqual(q.Get("state"), sp.State)
	if stateOK {
		// One use only, whatever happens next. A request without the
		// flow's state leaves the flow alone: a link cannot cancel a
		// sign-in in progress.
		clearCookie(w, stateCookie, "/auth/", s.secure(r))
	}
	if e := q.Get("error"); e != "" {
		// Anyone can put text in these parameters: the page shows a
		// fixed message and the error code only when it looks like one
		// (RFC 6749 codes are lower case and underscores); the
		// description goes to the log.
		slog.Warn("sso: provider returned an error", "error", e, "description", q.Get("error_description"))
		msg := "The provider did not complete the sign-in."
		if oauthErrorCode.MatchString(e) {
			msg = "The provider did not complete the sign-in (" + e + ")."
		}
		errorPage(w, http.StatusUnauthorized, "Sign-in was not completed", msg+" Start again, or ask the operator to check the gateway log.")
		return
	}
	if !flowOK {
		errorPage(w, http.StatusBadRequest, "Sign-in expired",
			"The sign-in attempt was missing, expired or came from another browser. Start again.")
		return
	}
	if !stateOK {
		slog.Warn("sso: state mismatch")
		errorPage(w, http.StatusBadRequest, "Sign-in rejected", "The response did not match this sign-in attempt. Start again.")
		return
	}
	p, err := s.discover()
	if err != nil {
		slog.Warn("sso: provider discovery failed", "issuer", s.cfg.Issuer, "err", err)
		errorPage(w, http.StatusBadGateway, "Sign-in provider unreachable", "Try again, or sign in with a token.")
		return
	}
	ctx := s.ctx(r.Context())
	tok, err := s.oauthConfig(p, s.redirectFor(r)).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(sp.Verifier))
	if err != nil {
		slog.Warn("sso: code exchange failed", "err", exchangeErr(err))
		errorPage(w, http.StatusBadGateway, "Sign-in failed", "The provider did not accept the sign-in. Start again.")
		return
	}
	id, reason, refused := s.verifyIDToken(ctx, p, tok, sp.Nonce)
	if reason != "" {
		slog.Warn("sso: sign-in refused", "reason", reason, "email", id.Email)
		if refused {
			msg := fmt.Sprintf("%s has no role on this gateway: no role rule matches it. Ask the operator to add one, or sign in with a token.", displayEmail(id.Email))
			switch {
			case strings.HasPrefix(reason, "email not verified"):
				msg = fmt.Sprintf("The provider has not verified %s, so it cannot be used to sign in here.", id.Email)
			case strings.HasPrefix(reason, "no email claim"):
				msg = "The provider sent no email for this account. The gateway requests the email scope; check the client's claims at the provider."
			}
			errorPage(w, http.StatusForbidden, "Not allowed", msg)
			return
		}
		errorPage(w, http.StatusUnauthorized, "Sign-in rejected", "The provider's identity token did not verify. Start again.")
		return
	}
	if sp.CLI != nil {
		s.finishCLI(w, r, sp.CLI, id)
		return
	}
	now := s.now()
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie,
		Value: sign(key, "session", sessionPayload{
			Email: id.Email, Groups: id.Groups,
			Exp: now.Add(s.cfg.SessionTTL).Unix(), Issued: now.UnixMilli(),
		}),
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   s.secure(r),
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	slog.Info("sso: signed in", "email", id.Email, "role", id.Role)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// identity is what a verified ID token says about the person.
type identity struct {
	Email  string
	Groups []string
	Role   gateway.AdminSessionRole
}

// verifyIDToken checks the ID token and admission. reason is empty on
// success. refused means the token verified but the person has no
// role; Email is set then, for the refusal page.
func (s *SSO) verifyIDToken(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, nonce string) (id identity, reason string, refused bool) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return id, "no id_token in token response", false
	}
	// Signature (JWKS, refetched on an unknown kid), iss, aud and exp
	// are the library's. Its clock is set back by clockSkew so a token
	// that expired seconds ago on a fast provider clock passes.
	v := p.Verifier(&oidc.Config{ClientID: s.cfg.ClientID, Now: func() time.Time { return s.now().Add(-clockSkew) }})
	idt, err := v.Verify(ctx, raw)
	if err != nil {
		return id, "id token: " + err.Error(), false
	}
	if !strEqual(idt.Nonce, nonce) {
		return id, "id token: nonce mismatch", false
	}
	if !idt.IssuedAt.IsZero() && idt.IssuedAt.After(s.now().Add(clockSkew)) {
		return id, "id token: issued in the future", false
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return id, "id token claims: " + err.Error(), false
	}
	// Some providers (Zitadel by default) leave profile claims out of
	// the ID token and serve them at userinfo; groups often come only
	// from there too. Ask when one is missing, and accept only an
	// answer about the same subject.
	_, hasGroups := claims[s.cfg.GroupsClaim]
	needEmail := strings.TrimSpace(claimString(claims["email"])) == ""
	if needEmail || (!hasGroups && s.hasGroupRules()) {
		ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		switch {
		case err != nil && needEmail:
			return id, "userinfo: " + err.Error(), false
		case err == nil && ui.Subject != idt.Subject:
			return id, "userinfo: subject does not match the id token", false
		case err == nil:
			extra := map[string]any{}
			if err := ui.Claims(&extra); err != nil {
				return id, "userinfo claims: " + err.Error(), false
			}
			for k, v := range extra {
				switch k {
				case "email", "email_verified":
					// Together, and only when the email comes from
					// userinfo: a verified flag there may be about
					// another address than the ID token's.
					if needEmail {
						claims[k] = v
					}
				default:
					if _, ok := claims[k]; !ok {
						claims[k] = v
					}
				}
			}
		}
	}
	id.Email = strings.ToLower(strings.TrimSpace(claimString(claims["email"])))
	if id.Email == "" {
		return id, "no email claim (is the email scope requested?)", true
	}
	if !s.cfg.AllowUnverifiedEmail && !truthy(claims["email_verified"]) {
		return id, "email not verified by the provider", true
	}
	id.Groups = s.ruleGroups(claimStrings(claims[s.cfg.GroupsClaim]))
	role, ok := s.roleOf(id.Email, id.Groups)
	if !ok {
		return id, "no role rule matches", true
	}
	id.Role = role
	return id, "", false
}

// AdminSession implements gateway.AdminSessionAuthenticator: the
// session on r, when the signature is good, it has not expired, it was
// issued after the subject's cutoff, and a rule gives it a role now.
func (s *SSO) AdminSession(r *http.Request) (gateway.AdminSession, bool) {
	sp, ok := s.session(r)
	if !ok {
		return gateway.AdminSession{}, false
	}
	role, ok := s.roleOf(sp.Email, sp.Groups)
	if !ok {
		return gateway.AdminSession{}, false
	}
	return gateway.AdminSession{Subject: sp.Email, Role: role}, true
}

func (s *SSO) session(r *http.Request) (sessionPayload, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return sessionPayload{}, false
	}
	return s.verifySession(r.Context(), "session", c.Value)
}

// verifySession checks a signed session payload: signature under
// purpose ("session" for the cookie, "cli-bearer" for the bearer, so
// neither passes as the other), expiry, and the revocation cutoff.
func (s *SSO) verifySession(ctx context.Context, purpose, value string) (sessionPayload, bool) {
	st := s.storeOf()
	if st == nil {
		return sessionPayload{}, false
	}
	key, err := st.AdminSessionKey(ctx)
	if err != nil {
		slog.Warn("sso: session key unavailable; refusing the session", "err", err)
		return sessionPayload{}, false
	}
	var sp sessionPayload
	if !verify(key, purpose, value, &sp) || sp.Exp < s.now().Unix() || sp.Email == "" {
		return sessionPayload{}, false
	}
	nb, err := st.AdminSessionNotBefore(ctx, sp.Email)
	if err != nil {
		slog.Warn("sso: revocation lookup failed; refusing the session", "email", sp.Email, "err", err)
		return sessionPayload{}, false
	}
	if nb > 0 && sp.Issued <= nb {
		return sessionPayload{}, false
	}
	return sp, true
}

// Logout clears the session cookie: POST /auth/logout. Same-origin
// only, like every other cookie-authenticated write.
func (s *SSO) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		http.Error(w, "forbidden: logout must come from the gateway's own pages (Sec-Fetch-Site: same-origin)", http.StatusForbidden)
		return
	}
	clearCookie(w, SessionCookie, "/", s.secure(r))
	w.WriteHeader(http.StatusNoContent)
}

// WhoAmI is GET /auth/whoami, public: whether SSO is on, and whose
// session this browser holds. The login screen asks it before it has
// any credential. Bearer tokens are not reported here; GET
// /api/admin/whoami resolves those.
func (s *SSO) WhoAmI(w http.ResponseWriter, r *http.Request) {
	out := struct {
		SSO           bool   `json:"sso"`
		Authenticated bool   `json:"authenticated"`
		Email         string `json:"email,omitempty"`
		Role          string `json:"role,omitempty"`
		Via           string `json:"via,omitempty"`
		// Error is "sso unavailable" when SSO cannot be used right
		// now (cluster level below the one sessions need, store
		// unavailable); the reason is logged, not sent.
		Error string `json:"error,omitempty"`
	}{SSO: true}
	if _, err := s.key(r.Context()); err != nil {
		// The page is public: the detail goes to the log only.
		slog.Warn("sso: session store unavailable", "err", err)
		out.Error = "sso unavailable"
	} else if sess, ok := s.AdminSession(r); ok {
		out.Authenticated, out.Email, out.Role, out.Via = true, sess.Subject, string(sess.Role), "sso"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// claimString reads a string claim; anything else is "".
func claimString(v any) string {
	s, _ := v.(string)
	return s
}

// claimStrings reads a list-of-strings claim; a single string counts
// as a list of one.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// truthy reads email_verified, which a few providers send as "true".
func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

// sign encodes v as base64url(JSON).base64url(HMAC-SHA256). purpose is
// mixed into the MAC so a state cookie never passes as a session.
func sign(key []byte, purpose string, v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, purpose, body))
}

func verify(key []byte, purpose, value string, out any) bool {
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

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func strEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// secure: the cookie gets Secure on TLS, behind a proxy that says
// https, or with an https redirect URL.
func (s *SSO) secure(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return strings.HasPrefix(s.cfg.RedirectURL, "https://")
}

func clearCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
}

// exchangeErr keeps the provider's error code and drops the rest: a
// token endpoint's body does not belong in a log.
func exchangeErr(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return re.ErrorCode
		}
		if re.Response != nil {
			return re.Response.Status
		}
	}
	return "request failed"
}

func displayEmail(e string) string {
	if e == "" {
		return "This account"
	}
	return e
}

var errorTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · gwag</title></head>
<body style="font-family:system-ui,sans-serif;max-width:34em;margin:4em auto;padding:0 16px;line-height:1.5">
<h1 style="font-size:1.3em">{{.Title}}</h1>
<p>{{.Msg}}</p>
<p><a href="/">Back to the sign-in screen</a></p>
</body></html>
`))

// errorPage renders a plain page with a way back. Everything is
// escaped: msg can carry the provider's error_description from the URL.
func errorPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, struct{ Title, Msg string }{title, msg})
}
