package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/gw/internal/adminsession"
)

// Sign-in codes: gwag login --web and gwag service login --web sign
// the admin UI in without pasting a token.
//
//  1. The CLI calls POST /admin/sessions/code with a bearer: the admin
//     or dev token, or a gwo_ SSO bearer. A session cookie cannot mint
//     a code (a browser session must not turn itself into a code
//     another browser redeems), nor can a credential the AdminAuthorizer
//     delegate checks: the delegate decides per request, and a cookie
//     would freeze its answer. A token code carries the token's role
//     and subject; a gwo_ code carries the email and groups and
//     redeems as an SSO session, whose role the SSO rules give per
//     request. The code lives adminSessionCodeTTL, and is stored as a
//     hash in the one-time code table (PutAdminLoginCode: memory
//     standalone, gwag-admin-sessions in a cluster), so a code minted
//     on one node redeems on another.
//  2. The CLI prints http://HOST/#code=… (the fragment never reaches
//     the server or a proxy log) and opens it.
//  3. The UI drops the fragment and POSTs the code to /auth/session.
//     The gateway takes the code (compare-and-set: one redeem, cluster
//     wide) and sets the session cookie: the same cookie, key and
//     checks as SSO (gw/internal/adminsession), signed for
//     PurposeToken with the role inside (token code) or PurposeSSO
//     (gwo_ code). Logout, the denylist and
//     POST /admin/sessions/revoke (subject "token:admin", "token:dev",
//     or the caller's subject) apply to it.

const (
	adminSessionCodeTTL = 60 * time.Second
	// maxAdminSessionCodes caps the codes waiting to be redeemed.
	maxAdminSessionCodes = 64
	// maxAdminSessionCodesPerSubject caps them per subject, so one
	// token cannot fill the table for everyone else.
	maxAdminSessionCodesPerSubject = 4
	// adminSessionCodePrefix starts the store id of a web code; the
	// SSO CLI codes use "cli.", so neither redeems as the other.
	adminSessionCodePrefix = "web."
)

// adminSessionCode is the value stored under a code.
type adminSessionCode struct {
	Subject string `json:"s"`
	// Role is the role of a token session; for an SSO session, the
	// caller's role at mint time (reported, not signed into the
	// cookie).
	Role string `json:"r"`
	// SSO: redeem as an SSO session (Subject is the email, Groups the
	// rule groups) instead of a token session.
	SSO    bool     `json:"o,omitempty"`
	Groups []string `json:"g,omitempty"`
}

// errAdminSessionCodesFull: maxAdminSessionCodes are waiting.
var errAdminSessionCodesFull = fmt.Errorf("too many sign-in codes waiting (%d); retry in a minute", maxAdminSessionCodes)

// errAdminSessionCodesSubjectFull: the subject has
// maxAdminSessionCodesPerSubject waiting.
var errAdminSessionCodesSubjectFull = fmt.Errorf("too many sign-in codes waiting for this credential (%d); redeem one or retry in a minute", maxAdminSessionCodesPerSubject)

func adminSessionCodeID(code string) string {
	sum := sha256.Sum256([]byte("gwag-web-code\x00" + code))
	return adminSessionCodePrefix + hex.EncodeToString(sum[:])
}

// mintAdminSessionCode stores a one-time code for c and returns it.
func (g *Gateway) mintAdminSessionCode(ctx context.Context, c0 adminSessionCode) (string, error) {
	if _, ok := adminSessionRole(AdminSessionRole(c0.Role)); !ok || c0.Subject == "" {
		return "", errors.New("sign-in code: role must be admin or dev")
	}
	code := adminsession.RandomString()
	v, _ := json.Marshal(c0)
	id, exp := adminSessionCodeID(code), time.Now().Add(adminSessionCodeTTL).UnixMilli()
	err := g.editAdminLoginCodes(ctx, func(c adminExpiring) (bool, error) {
		n, mine := 0, 0
		for k, e := range c {
			if !strings.HasPrefix(k, adminSessionCodePrefix) {
				continue
			}
			n++
			var o adminSessionCode
			if json.Unmarshal(e.Value, &o) == nil && o.Subject == c0.Subject {
				mine++
			}
		}
		if mine >= maxAdminSessionCodesPerSubject {
			return false, errAdminSessionCodesSubjectFull
		}
		if n >= maxAdminSessionCodes {
			return false, errAdminSessionCodesFull
		}
		c[id] = adminExpiringEntry{Value: v, ExpUnixMs: exp}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// takeAdminSessionCode spends code. ok is false when it is unknown,
// used or expired.
func (g *Gateway) takeAdminSessionCode(ctx context.Context, code string) (adminSessionCode, bool, error) {
	raw, ok, err := g.TakeAdminLoginCode(ctx, adminSessionCodeID(code))
	if err != nil || !ok {
		return adminSessionCode{}, false, err
	}
	var c adminSessionCode
	if json.Unmarshal(raw, &c) != nil || c.Subject == "" {
		return adminSessionCode{}, false, nil
	}
	return c, true, nil
}

// AdminSessionTTLer is an AdminSessionAuthenticator that names how long
// a session lasts (gw/oidc: --sso-session-ttl). Sessions redeemed from
// a sign-in code then last as long; otherwise
// DefaultAdminSessionTTL.
//
// Stability: experimental
type AdminSessionTTLer interface {
	AdminSessionTTL() time.Duration
}

// DefaultAdminSessionTTL is how long a session redeemed from a
// sign-in code lasts when the AdminSessionAuthenticator names no TTL.
//
// Stability: experimental
const DefaultAdminSessionTTL = adminsession.DefaultTTL

func (g *Gateway) adminSessionTTL() time.Duration {
	if t, ok := g.cfg.adminSession.(AdminSessionTTLer); ok {
		if d := t.AdminSessionTTL(); d > 0 {
			return d
		}
	}
	return DefaultAdminSessionTTL
}

// tokenSessionOf is the session redeemed from a sign-in code that r
// carries, if any.
func (g *Gateway) tokenSessionOf(r *http.Request) (AdminSession, bool) {
	p, ok := adminsession.TokenSession(r, g, time.Now())
	if !ok {
		return AdminSession{}, false
	}
	return AdminSession{Subject: p.Email, Role: AdminSessionRole(p.Role)}, true
}

// MountAdminSessions registers POST /auth/session on mux: the browser
// trades a sign-in code (gwag login --web) for a session cookie.
// Without WithAdminSession it also registers GET /auth/whoami and POST
// /auth/logout for those sessions; with it, the authenticator serves
// them (gw/oidc's Mount does, and reads these sessions too), so call
// this after or before oidc.SSO.Mount, not instead of it.
//
// Stability: experimental
func (g *Gateway) MountAdminSessions(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/session", g.serveAdminSessionRedeem)
	if g.cfg.adminSession == nil {
		mux.HandleFunc("GET /auth/whoami", g.serveAdminSessionWhoami)
		mux.HandleFunc("POST /auth/logout", g.serveAdminSessionLogout)
	}
}

func sessionJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// serveAdminSessionRedeem is POST /auth/session {"code"}. Same-origin
// only: a page elsewhere must not plant a session in this browser. The
// code is never logged.
func (g *Gateway) serveAdminSessionRedeem(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) { sessionJSON(w, status, map[string]string{"error": msg}) }
	if why := adminSessionAllowed(r); why != "" {
		g.recordAdminAuth(r.Method, "denied_session_csrf", adminRoleAnonymous)
		fail(http.StatusForbidden, "sign-in must come from the gateway's own pages ("+why+")")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Code == "" || len(in.Code) > 128 {
		fail(http.StatusBadRequest, "body must be JSON {code}")
		return
	}
	ctx := r.Context()
	key, err := g.AdminSessionKey(ctx)
	if err != nil {
		g.sessionStoreFail(w, err)
		return
	}
	c, ok, err := g.takeAdminSessionCode(ctx, in.Code)
	if err != nil {
		g.sessionStoreFail(w, err)
		return
	}
	role, roleOK := adminSessionRole(AdminSessionRole(c.Role))
	if !ok || !roleOK {
		g.recordAdminAuth(r.Method, "denied_session_code", adminRoleAnonymous)
		fail(http.StatusUnauthorized, "sign-in code unknown, used or expired; run gwag login --web again")
		return
	}
	now := time.Now()
	if nb, err := g.AdminSessionNotBefore(ctx, c.Subject); err != nil {
		g.sessionStoreFail(w, err)
		return
	} else if nb >= now.UnixMilli() {
		fail(http.StatusForbidden, fmt.Sprintf("the sessions of %s were ended moments ago; sign in again after %s UTC", c.Subject, time.UnixMilli(nb).UTC().Format(time.TimeOnly)))
		return
	}
	ttl := g.adminSessionTTL()
	exp := now.Add(ttl)
	purpose, p := adminsession.PurposeToken, adminsession.Payload{
		Email: c.Subject, Role: c.Role, Exp: exp.Unix(), Issued: now.UnixMilli(), ID: adminsession.RandomString(),
	}
	if c.SSO {
		// The SSO rules give the role per request; it is not signed in.
		purpose, p.Role, p.Groups = adminsession.PurposeSSO, "", c.Groups
	}
	adminsession.SetCookie(w, adminsession.Sign(key, purpose, p), ttl, adminsession.RequestSecure(r))
	g.recordAdminAuth(r.Method, "ok_session_code", role)
	slog.Info("admin session: signed in with a code", "subject", c.Subject, "role", c.Role, "sso", c.SSO)
	sessionJSON(w, http.StatusOK, map[string]string{
		"subject": sanitizeSubject(c.Subject), "role": c.Role, "expires_at": exp.UTC().Format(time.RFC3339),
	})
}

func (g *Gateway) sessionStoreFail(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, errAdminSessionsLevel) {
		status = http.StatusConflict
	}
	slog.Warn("admin session: store unavailable", "err", err)
	sessionJSON(w, status, map[string]string{"error": err.Error()})
}

// serveAdminSessionWhoami is GET /auth/whoami without SSO: the same
// answer gw/oidc gives, with sso false.
func (g *Gateway) serveAdminSessionWhoami(w http.ResponseWriter, r *http.Request) {
	out := struct {
		SSO           bool   `json:"sso"`
		Authenticated bool   `json:"authenticated"`
		Email         string `json:"email,omitempty"`
		Role          string `json:"role,omitempty"`
		Via           string `json:"via,omitempty"`
	}{}
	if s, ok := g.tokenSessionOf(r); ok {
		out.Authenticated, out.Email, out.Role, out.Via = true, sanitizeSubject(s.Subject), string(s.Role), "token"
	}
	sessionJSON(w, http.StatusOK, out)
}

// serveAdminSessionLogout is POST /auth/logout without SSO: clears the
// cookie and puts the session on the denylist, on every node.
func (g *Gateway) serveAdminSessionLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		http.Error(w, "forbidden: logout must come from the gateway's own pages (Sec-Fetch-Site: same-origin)", http.StatusForbidden)
		return
	}
	secure := adminsession.RequestSecure(r)
	adminsession.ClearCookie(w, adminsession.CookieName, "/", secure)
	c, err := r.Cookie(adminsession.CookieName)
	if err != nil || c.Value == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if p, found, err := adminsession.End(r.Context(), g, c.Value, time.Now(), adminsession.PurposeToken); err != nil {
		slog.Warn("admin session: logout could not end the session on the server", "subject", p.Email, "err", err)
		http.Error(w, "the session could not be ended on the gateway; retry, or ask an admin to revoke it", http.StatusServiceUnavailable)
		return
	} else if found {
		slog.Info("admin session: signed out", "subject", p.Email)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- admin operation ----------------------------------------------

// adminCallerVia is how AdminMiddleware authenticated a request.
type adminCallerVia int

const (
	adminViaOther         adminCallerVia = iota
	adminViaCookie                       // a session cookie
	adminViaToken                        // the boot admin or dev token
	adminViaDelegate                     // the AdminAuthorizer delegate
	adminViaSessionBearer                // a gwo_ session bearer
)

// adminCaller is how the caller of an admin request was
// authenticated; the sign-in code route reads it.
type adminCaller struct {
	via adminCallerVia
	// tokenRole is the role of the boot or dev token (via token).
	tokenRole adminRole
	// sessionBearer is the gwo_ bearer (via session bearer).
	sessionBearer string
}

type adminCallerCtxKey struct{}

func adminCallerFrom(ctx context.Context) adminCaller {
	c, _ := ctx.Value(adminCallerCtxKey{}).(adminCaller)
	return c
}

// adminCallerOf names how AdminMiddleware authenticated a request from
// its outcome. A delegate that accepted the boot or dev token itself
// counts as that token.
func (g *Gateway) adminCallerOf(outcome string, viaSession bool, bearer string) adminCaller {
	switch {
	case viaSession:
		return adminCaller{via: adminViaCookie}
	case outcome == "ok_session_bearer":
		return adminCaller{via: adminViaSessionBearer, sessionBearer: bearer}
	case outcome == "ok_bearer" || outcome == "ok_delegate":
		if r, _, st := g.authenticateAdminToken(bearer, true); st == 0 && (r == adminRoleAdmin || r == adminRoleDev) {
			return adminCaller{via: adminViaToken, tokenRole: r}
		}
		if outcome == "ok_delegate" {
			return adminCaller{via: adminViaDelegate}
		}
	}
	return adminCaller{}
}

type createAdminSessionCodeOut struct {
	Body struct {
		Code             string `json:"code" doc:"One-time code. Open http://HOST/#code=CODE in the browser; the admin UI redeems it at POST /auth/session."`
		Role             string `json:"role" enum:"dev,admin" doc:"Role the browser session gets: the caller's."`
		Subject          string `json:"subject" doc:"Subject the session is recorded under: token:admin, token:dev, or the caller's subject."`
		ExpiresInSeconds int    `json:"expiresInSeconds"`
	}
}

func registerAdminSessionCodeRoutes(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "createAdminSessionCode",
		Method:      http.MethodPost,
		Path:        "/admin/sessions/code",
		Summary:     "Mint a one-time code (60 s, single use) that signs a browser in to the admin UI (gwag login --web). Needs the admin or dev token, or an SSO bearer (gwo_); a session cookie or a credential the AdminAuthorizer delegate checks is refused. Dev or admin.",
	}, func(ctx context.Context, _ *struct{}) (*createAdminSessionCodeOut, error) {
		role, ok := adminRoleFromContext(ctx)
		caller := adminCallerFrom(ctx)
		if !ok || caller.via == adminViaCookie {
			return nil, huma.Error403Forbidden("a sign-in code needs a bearer (admin or dev token); a session cookie cannot mint one")
		}
		if role != adminRoleAdmin && role != adminRoleDev {
			return nil, huma.Error403Forbidden("a sign-in code needs the dev or admin role")
		}
		var c adminSessionCode
		switch caller.via {
		case adminViaToken:
			// The token's own role, fixed like the token.
			role = caller.tokenRole
			c = adminSessionCode{Subject: "token:" + role.String(), Role: role.String()}
		case adminViaSessionBearer:
			sc, err := g.ssoSessionCode(ctx, caller.sessionBearer, role)
			if err != nil {
				return nil, err
			}
			c = sc
		default:
			return nil, huma.Error403Forbidden("sign-in codes need the admin or dev token or an SSO login; this credential is checked per request by the delegate")
		}
		code, err := g.mintAdminSessionCode(ctx, c)
		if err != nil {
			switch {
			case errors.Is(err, errAdminSessionsLevel):
				return nil, huma.Error409Conflict(err.Error())
			case errors.Is(err, errAdminSessionsStarting), errors.Is(err, errAdminLoginCodeConflict):
				return nil, huma.Error503ServiceUnavailable(err.Error())
			case errors.Is(err, errAdminSessionCodesFull), errors.Is(err, errAdminSessionCodesSubjectFull):
				return nil, huma.Error429TooManyRequests(err.Error())
			}
			return nil, err
		}
		out := &createAdminSessionCodeOut{}
		out.Body.Code, out.Body.Role, out.Body.Subject = code, role.String(), sanitizeSubject(c.Subject)
		out.Body.ExpiresInSeconds = int(adminSessionCodeTTL.Seconds())
		return out, nil
	})
}

// ssoSessionCode is the code for a gwo_ caller: it redeems as an SSO
// session (email and groups, no role), so the SSO rules give the
// browser its role per request, as for a browser SSO sign-in. A bearer
// the gateway cannot read as gw/oidc's (another authenticator) is
// refused.
func (g *Gateway) ssoSessionCode(ctx context.Context, bearer string, role adminRole) (adminSessionCode, error) {
	v := strings.TrimPrefix(bearer, AdminSessionBearerPrefix)
	p, ok := adminsession.Check(ctx, g, adminsession.PurposeCLIBearer, v, time.Now())
	if !ok {
		return adminSessionCode{}, huma.Error403Forbidden("this SSO bearer cannot be turned into a browser session; sign the browser in at /auth/login")
	}
	return adminSessionCode{Subject: p.Email, Role: role.String(), SSO: true, Groups: p.Groups}, nil
}
