// Package oidctest is a minimal OpenID provider for tests of package
// oidc and of gateways that wire it: discovery, JWKS, userinfo and a
// token endpoint that checks the PKCE verifier and returns an RS256 ID
// token. There is no authorize page: a test reads code_challenge and
// nonce from the /auth/login redirect, hands them to Expect, and calls
// the callback with Code.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Code is the authorization code the token endpoint accepts.
const Code = "good-code"

// Provider is the fake provider.
type Provider struct {
	// URL is the issuer.
	URL      string
	ClientID string

	t         testing.TB
	mu        sync.Mutex
	key       *rsa.PrivateKey
	challenge string
	nonce     string
	claims    func(issuer, nonce string) map[string]any
	userinfo  map[string]any
}

// New starts a provider for clientID, closed at test end. Its ID
// tokens carry GoodClaims until SetClaims.
func New(t testing.TB, clientID string) *Provider {
	p := &Provider{t: t, ClientID: clientID}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.key = k
	p.claims = p.GoodClaims
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"userinfo_endpoint":                     p.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := p.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		ui := p.userinfo
		p.mu.Unlock()
		if ui == nil || r.Header.Get("Authorization") != "Bearer at" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ui)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		challenge, nonce, claims := p.challenge, p.nonce, p.claims
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("code") != Code || b64(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.Mint(claims(p.URL, nonce)),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// GoodClaims is a valid ID token for ops@example.com, verified.
func (p *Provider) GoodClaims(iss, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss, "aud": p.ClientID, "sub": "u1", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": "ops@example.com", "email_verified": true,
	}
}

// SetClaims sets the ID token claims.
func (p *Provider) SetClaims(f func(issuer, nonce string) map[string]any) {
	p.mu.Lock()
	p.claims = f
	p.mu.Unlock()
}

// SetUserinfo sets what /userinfo answers; nil = 404.
func (p *Provider) SetUserinfo(m map[string]any) {
	p.mu.Lock()
	p.userinfo = m
	p.mu.Unlock()
}

// Expect records the code_challenge and nonce of an authorize URL, as
// the provider's login page would.
func (p *Provider) Expect(authorizeURL string) error {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.challenge, p.nonce = u.Query().Get("code_challenge"), u.Query().Get("nonce")
	p.mu.Unlock()
	return nil
}

// Mint signs claims as an ID token.
func (p *Provider) Mint(claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
