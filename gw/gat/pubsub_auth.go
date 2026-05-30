package gat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// subscribeTokenSkew bounds how far a subscribe token's timestamp may
// be from the verifying gateway's clock — a token is accepted within
// ±skew of its embedded timestamp.
const subscribeTokenSkew = 5 * time.Minute

// EnableSubscribeAuth gates the WebSocket subscribe endpoint behind
// HMAC tokens. After this call, a subscribe request must carry valid
// `token` and `ts` query parameters minted by SignSubscribeToken
// with the same secret. Without it the endpoint is open — acceptable
// only on a trusted network.
//
// The secret is independent of the peer-mesh Auth key: client-trust
// (gateway↔client) and peer-trust (gateway↔gateway) are separate
// domains. Call once, before serving traffic.
//
// Stability: experimental
func (g *Gateway) EnableSubscribeAuth(secret []byte) {
	g.subAuth = secret
}

// SignSubscribeToken mints an HMAC token authorising a subscription
// to `channel`. The client presents the returned token and timestamp
// as the `token` and `ts` query parameters on the WebSocket subscribe
// URL; `channel` must match exactly. The token is accepted within
// ±5 minutes of `ts`.
//
// `secret` must be the same key passed to EnableSubscribeAuth. The
// HMAC covers `channel` + timestamp, so a token is bound to one
// channel pattern. Mirrors gwag's SignSubscribeToken shape.
//
// Stability: experimental
func SignSubscribeToken(secret []byte, channel string) (token string, ts int64) {
	ts = time.Now().Unix()
	return computeSubscribeToken(secret, channel, ts), ts
}

// SignSubscribeToken is the import-boundary counterpart of the package
// function: it mints a subscribe token for `channel` using the secret
// configured via EnableSubscribeAuth, so anything holding the *Gateway
// can sign without threading the secret around and without an admin
// token — the Go import is the auth boundary.
//
// Returns an error if EnableSubscribeAuth has not been called: an open
// endpoint has nothing to sign against, so signing is a usage mistake
// worth surfacing rather than minting a token under a nil key.
//
// Stability: experimental
func (g *Gateway) SignSubscribeToken(channel string) (token string, ts int64, err error) {
	if g.subAuth == nil {
		return "", 0, fmt.Errorf("gat: subscribe auth not enabled (call EnableSubscribeAuth first)")
	}
	ts = time.Now().Unix()
	return computeSubscribeToken(g.subAuth, channel, ts), ts, nil
}

// computeSubscribeToken is the HMAC-SHA256 of `channel\nts`,
// URL-safe-base64 encoded (no padding) so the token drops straight
// into the WebSocket subscribe URL's query string without escaping.
func computeSubscribeToken(secret []byte, channel string, ts int64) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s\n%d", channel, ts)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifySubscribeRequest checks the subscribe-auth token on a WS
// upgrade request. It returns nil when subscribe auth is disabled
// (EnableSubscribeAuth not called — the endpoint is open) or when the
// token, timestamp, and channel all check out; an error otherwise.
func (g *Gateway) verifySubscribeRequest(r *http.Request) error {
	if g.subAuth == nil {
		return nil // open — trusted-network posture
	}
	q := r.URL.Query()
	channel, token, tsStr := q.Get("channel"), q.Get("token"), q.Get("ts")
	if token == "" || tsStr == "" {
		return fmt.Errorf("subscribe auth: missing token or ts")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("subscribe auth: invalid ts")
	}
	if d := time.Since(time.Unix(ts, 0)); d > subscribeTokenSkew || d < -subscribeTokenSkew {
		return fmt.Errorf("subscribe auth: token expired")
	}
	want := computeSubscribeToken(g.subAuth, channel, ts)
	if !hmac.Equal([]byte(want), []byte(token)) {
		return fmt.Errorf("subscribe auth: bad token")
	}
	return nil
}
