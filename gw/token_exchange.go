package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/metadata"
)

// exchangeSkew is how far before a token's stated expiry the cache
// treats it as stale, so a request never goes out with a token about to
// expire mid-flight.
const exchangeSkew = 30 * time.Second

// defaultMaxCachedTokens bounds the exchanged-token cache when
// TokenExchangeConfig.MaxCachedTokens is unset, so a high-cardinality
// caller population can't grow it without limit.
const defaultMaxCachedTokens = 4096

// tokenExchangeGrant is the RFC 8693 grant type.
const tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"

// TokenExchangeConfig configures an outbound RoundTripper that performs
// RFC 8693 OAuth 2.0 token exchange: it reads the inbound caller's
// bearer token off the originating request and swaps it at TokenURL for
// a token scoped to the upstream, attached as `Authorization: Bearer`.
// This preserves caller identity across the hop while re-minting the
// token for the upstream's audience — distinct from ForwardHeaders
// (verbatim passthrough) and ServiceAccountTransport (the gateway's own
// identity). Install via WithOpenAPIClient / OpenAPIClient.
//
// Stability: experimental
type TokenExchangeConfig struct {
	// TokenURL is the OAuth 2.0 token-exchange endpoint. Required.
	TokenURL string
	// Audience / Resource scope the exchanged token to the upstream
	// (the RFC 8693 `audience` / `resource` parameters). Optional.
	Audience string
	Resource string
	// Scope is the space-separated scopes requested. Optional.
	Scope string
	// ClientID / ClientSecret authenticate the gateway to the token
	// endpoint via HTTP Basic. Optional.
	ClientID     string
	ClientSecret string
	// SubjectTokenType is the RFC 8693 `subject_token_type`. Default
	// "urn:ietf:params:oauth:token-type:access_token".
	SubjectTokenType string
	// RequestedTokenType is the optional `requested_token_type`.
	RequestedTokenType string
	// HTTPClient calls the token endpoint. Default http.DefaultClient.
	HTTPClient *http.Client
	// Base is the transport for the upstream call. Default
	// http.DefaultTransport.
	Base http.RoundTripper

	// MaxCachedTokens caps the exchanged-token cache. 0 →
	// defaultMaxCachedTokens. When full, expired entries are purged
	// first, then the soonest-to-expire entry is evicted.
	MaxCachedTokens int

	// now overrides the clock for tests.
	now func() time.Time
}

// TokenExchangeTransport is the http.RoundTripper built from a
// TokenExchangeConfig. It caches exchanged tokens by (subject, audience,
// scope, resource) until shortly before expiry, bounded by
// MaxCachedTokens. Concurrent misses for one key coalesce into a single
// exchange (singleflight) — and the network call does NOT hold the cache
// lock, so a slow token endpoint can't stall dispatches for other keys.
//
// Stability: experimental
type TokenExchangeTransport struct {
	cfg   TokenExchangeConfig
	group singleflight.Group // coalesces concurrent misses for one key
	mu    sync.Mutex
	cache map[string]cachedExchange
}

type cachedExchange struct {
	token   string
	expires time.Time
}

// NewTokenExchangeTransport validates cfg and returns the transport.
//
// Stability: experimental
func NewTokenExchangeTransport(cfg TokenExchangeConfig) (*TokenExchangeTransport, error) {
	if cfg.TokenURL == "" {
		return nil, fmt.Errorf("gateway: TokenExchangeConfig.TokenURL is required")
	}
	if cfg.SubjectTokenType == "" {
		cfg.SubjectTokenType = "urn:ietf:params:oauth:token-type:access_token"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Base == nil {
		cfg.Base = http.DefaultTransport
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.MaxCachedTokens <= 0 {
		cfg.MaxCachedTokens = defaultMaxCachedTokens
	}
	return &TokenExchangeTransport{cfg: cfg, cache: map[string]cachedExchange{}}, nil
}

// TokenExchangeClient is sugar for an *http.Client whose Transport is a
// TokenExchangeTransport. Pass it to WithOpenAPIClient / OpenAPIClient.
//
// Stability: experimental
func TokenExchangeClient(cfg TokenExchangeConfig) (*http.Client, error) {
	rt, err := NewTokenExchangeTransport(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: rt}, nil
}

// RoundTrip implements http.RoundTripper.
func (t *TokenExchangeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	subject := inboundBearer(req.Context())
	if subject == "" {
		return nil, fmt.Errorf("gateway: token exchange: no inbound bearer token to exchange")
	}
	tok, err := t.tokenFor(req.Context(), subject)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok)
	return t.cfg.Base.RoundTrip(clone)
}

func (t *TokenExchangeTransport) tokenFor(ctx context.Context, subject string) (string, error) {
	key := exchangeKey(subject, t.cfg.Audience, t.cfg.Scope, t.cfg.Resource)
	if tok, ok := t.cached(key); ok {
		return tok, nil
	}
	// Coalesce concurrent misses for the same key into one exchange — the
	// network call runs WITHOUT the cache lock held, so a slow IdP can't
	// block dispatches for other keys.
	v, err, _ := t.group.Do(key, func() (any, error) {
		// Another goroutine may have filled the cache while we queued.
		if tok, ok := t.cached(key); ok {
			return tok, nil
		}
		tok, ttl, err := t.exchange(ctx, subject)
		if err != nil {
			return "", err
		}
		t.store(key, tok, ttl)
		return tok, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// cached returns a live cached token for key, if present and unexpired.
func (t *TokenExchangeTransport) cached(key string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.cache[key]; ok && t.cfg.now().Before(c.expires) {
		return c.token, true
	}
	return "", false
}

// store caches a freshly exchanged token, bounding the cache: when at
// capacity it purges expired entries first, then evicts the soonest-to-
// expire one. Tokens whose remaining life is under the skew aren't
// cached.
func (t *TokenExchangeTransport) store(key, tok string, ttl time.Duration) {
	life := ttl - exchangeSkew
	if life <= 0 {
		return
	}
	now := t.cfg.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, fresh := t.cache[key]; !fresh && len(t.cache) >= t.cfg.MaxCachedTokens {
		t.evictLocked(now)
	}
	t.cache[key] = cachedExchange{token: tok, expires: now.Add(life)}
}

// evictLocked frees room in a full cache: drop expired entries, then if
// still full, the soonest-to-expire one. Caller holds t.mu.
func (t *TokenExchangeTransport) evictLocked(now time.Time) {
	for k, c := range t.cache {
		if !now.Before(c.expires) {
			delete(t.cache, k)
		}
	}
	if len(t.cache) < t.cfg.MaxCachedTokens {
		return
	}
	var evictKey string
	var soonest time.Time
	for k, c := range t.cache {
		if evictKey == "" || c.expires.Before(soonest) {
			evictKey, soonest = k, c.expires
		}
	}
	delete(t.cache, evictKey)
}

func (t *TokenExchangeTransport) exchange(ctx context.Context, subject string) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", tokenExchangeGrant)
	form.Set("subject_token", subject)
	form.Set("subject_token_type", t.cfg.SubjectTokenType)
	if t.cfg.Audience != "" {
		form.Set("audience", t.cfg.Audience)
	}
	if t.cfg.Resource != "" {
		form.Set("resource", t.cfg.Resource)
	}
	if t.cfg.Scope != "" {
		form.Set("scope", t.cfg.Scope)
	}
	if t.cfg.RequestedTokenType != "" {
		form.Set("requested_token_type", t.cfg.RequestedTokenType)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("gateway: token exchange: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if t.cfg.ClientID != "" {
		req.SetBasicAuth(t.cfg.ClientID, t.cfg.ClientSecret)
	}
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("gateway: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("gateway: token exchange: %s: %s", resp.Status, bodySnippet(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("gateway: token exchange: decode response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("gateway: token exchange: response had empty access_token")
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// inboundBearer extracts the caller's bearer token from the originating
// request — an HTTP request (GraphQL / REST ingress) or, for the gRPC
// ingress, the incoming gRPC metadata. Returns "" when absent.
func inboundBearer(ctx context.Context) string {
	if in := HTTPRequestFromContext(ctx); in != nil {
		return bearerFromHeader(in.Header.Get("Authorization"))
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			return bearerFromHeader(v[0])
		}
	}
	return ""
}

// bearerFromHeader strips a case-insensitive "Bearer " scheme; a value
// without the scheme is returned trimmed (tolerating raw-token callers).
func bearerFromHeader(h string) string {
	h = strings.TrimSpace(h)
	const scheme = "bearer "
	if len(h) >= len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		return strings.TrimSpace(h[len(scheme):])
	}
	return h
}

// exchangeKey hashes the cache-key parts so the raw subject token isn't
// retained as a map key any longer than the cached exchanged token.
func exchangeKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return string(h.Sum(nil))
}

func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
