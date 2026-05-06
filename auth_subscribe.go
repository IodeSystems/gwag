package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

const defaultSkewWindow = 5 * time.Minute

// subscribeAuthError is returned to graphql-go when verification fails;
// the WebSocket layer formats it into a graphql-ws `error` frame whose
// extensions carry the SubscribeAuthCode for operator/client triage.
type subscribeAuthError struct {
	Code   cpv1.SubscribeAuthCode
	Reason string // optional; surfaced to clients only when --debug-auth (TODO)
}

func (e *subscribeAuthError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", e.Code.String(), e.Reason)
	}
	return e.Code.String()
}

func (e *subscribeAuthError) Extensions() map[string]any {
	return map[string]any{
		"subscribeAuthCode": e.Code.String(),
	}
}

// verifySubscribe checks the (channel, hmac, timestamp) tuple against
// the gateway's SubscriptionAuthOptions. Returns OK or the appropriate
// failure code. The metric counter is incremented by the caller.
func (g *Gateway) verifySubscribe(channel string, args map[string]any) cpv1.SubscribeAuthCode {
	if g.cfg.subAuth.Insecure {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_OK
	}
	if len(g.cfg.subAuth.Secret) == 0 {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_NOT_CONFIGURED
	}

	hmacRaw, ok := args["hmac"]
	if !ok {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_MALFORMED
	}
	hmacStr, ok := hmacRaw.(string)
	if !ok || hmacStr == "" {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_MALFORMED
	}
	tsRaw, ok := args["timestamp"]
	if !ok {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_MALFORMED
	}
	ts, ok := asInt64(tsRaw)
	if !ok {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_MALFORMED
	}

	skew := g.cfg.subAuth.SkewWindow
	if skew == 0 {
		skew = defaultSkewWindow
	}
	now := time.Now().Unix()
	if ts < now-int64(skew.Seconds()) {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_TOO_OLD
	}
	if ts > now+int64(skew.Seconds()) {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_TOO_NEW
	}

	expected := computeSubscribeHMAC(g.cfg.subAuth.Secret, channel, ts)
	provided, err := base64.StdEncoding.DecodeString(hmacStr)
	if err != nil || !hmac.Equal(expected, provided) {
		return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_SIGNATURE_MISMATCH
	}
	return cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_OK
}

// computeSubscribeHMAC is the canonical signing function. Surface in
// the public API as SignSubscribeToken so external services using the
// same secret can mint compatible tokens locally.
func computeSubscribeHMAC(secret []byte, channel string, timestampUnix int64) []byte {
	h := hmac.New(sha256.New, secret)
	fmt.Fprintf(h, "%s\n%d", channel, timestampUnix)
	return h.Sum(nil)
}

// SignSubscribeToken returns the (base64-hmac, timestamp) pair for a
// channel using the given secret. Intended for callers that hold the
// shared secret and mint tokens locally — pair with the gateway's
// verifySubscribe on the read side.
//
// ttlSeconds is currently informational; the gateway's SkewWindow
// bounds replay regardless. A future version may pin tokens to an
// explicit expiry rather than a wall-clock window.
func SignSubscribeToken(secret []byte, channel string, ttlSeconds int64) (hmacB64 string, timestampUnix int64) {
	timestampUnix = time.Now().Unix()
	mac := computeSubscribeHMAC(secret, channel, timestampUnix)
	return base64.StdEncoding.EncodeToString(mac), timestampUnix
}

// asInt64 coerces a GraphQL Int (which graphql-go decodes as int) to
// int64 so we can compare timestamps. Some clients send numbers as
// float64 (JSON default) — handle that too.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
