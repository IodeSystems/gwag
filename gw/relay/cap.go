// Package relay is gwag's gateway-free, embeddable live-subscription relay and
// its typed-client generator — the "sprinkle into your own app, bring your own
// NATS + authorizers" counterpart to gw/gat (gat is NATS-free embedded
// translation; this is NATS-native embedded fan-out).
//
// The model: the adopter's own authorizer mints a signed Cap (channel + exp)
// after its access check; an identity-agnostic multiplexing WebSocket relay
// verifies the signature and fans matching bus events to clients — it never
// re-derives who the client is, so the gate is the cap, not the connection. The
// same code runs embedded in the adopter's server or as a detached pool behind a
// load balancer. GenClient spits out the typed TS client (payload types from the
// adopter's Go event structs + the static runtime), so a relay adopter gets a
// typed multiplex client for free. No gateway, schema, or broker.
package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Cap is a subscription capability: it authorizes a client to subscribe to one
// channel until Exp. The adopter mints it (after its own access check) and the
// relay verifies it; the relay trusts the signature and nothing else. Keep the
// fields stable — the JSON is the signed wire form.
type Cap struct {
	Channel string `json:"c"`
	Exp     int64  `json:"e"` // unix seconds; 0 = no expiry (discouraged)
}

// Cap verification errors. The relay reports these as the error text of an
// `err` frame; none re-derive identity.
var (
	ErrMalformed = errors.New("relay: malformed token")
	ErrSignature = errors.New("relay: bad signature")
	ErrExpired   = errors.New("relay: capability expired")
)

// Sign returns an opaque token binding c under secret:
// base64url(payload) "." base64url(HMAC-SHA256(payload)). The signature covers
// the exact transmitted payload bytes, so Verify re-derives nothing. secret is
// shared between the mint side (the adopter's authorizer) and the relay —
// symmetric is sufficient because one trust domain owns both.
func Sign(secret []byte, c Cap) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return enc(payload) + "." + enc(mac(secret, payload)), nil
}

// Verify checks token's signature against secret and its expiry against now,
// returning the carried Cap. Signature comparison is constant-time.
func Verify(secret []byte, token string, now time.Time) (Cap, error) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot >= len(token)-1 {
		return Cap{}, ErrMalformed
	}
	payload, err := dec(token[:dot])
	if err != nil {
		return Cap{}, ErrMalformed
	}
	sig, err := dec(token[dot+1:])
	if err != nil {
		return Cap{}, ErrMalformed
	}
	if !hmac.Equal(sig, mac(secret, payload)) {
		return Cap{}, ErrSignature
	}
	var c Cap
	if err := json.Unmarshal(payload, &c); err != nil {
		return Cap{}, ErrMalformed
	}
	if c.Exp != 0 && now.Unix() > c.Exp {
		return Cap{}, ErrExpired
	}
	return c, nil
}

func mac(secret, payload []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(payload)
	return h.Sum(nil)
}

func enc(b []byte) string          { return base64.RawURLEncoding.EncodeToString(b) }
func dec(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
