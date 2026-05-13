package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc/metadata"
)

// CallerIDHMACOptions configures the HMAC caller-id extractor.
//
// Verified payload mirrors the subscribe HMAC scheme
// (gw/auth_subscribe.go::computeSubscribeHMAC): empty kid signs
// "<caller_id>\n<timestamp_unix>"; non-empty kid signs
// "<kid>\n<caller_id>\n<timestamp_unix>" so swapping kid can't replay
// a token across keys.
//
// Stability: stable
type CallerIDHMACOptions struct {
	// Secret is the legacy single shared HMAC key. Tokens minted with
	// no kid (or kid=="") verify against this. Either Secret, Secrets,
	// or both may be set; if both supply the empty kid, Secret wins.
	Secret []byte

	// Secrets is the keyed-secret set used for token rotation. Tokens
	// minted with a non-empty kid look it up here; an unknown kid is
	// an extraction error. To rotate: add a new (kid, secret) entry,
	// switch signers to it, drop the old entry once outstanding tokens
	// age out.
	Secrets map[string][]byte

	// SkewWindow caps acceptable clock drift between the signer and
	// the gateway. 0 → defaultSkewWindow (5 minutes).
	SkewWindow time.Duration
}

// HMAC caller-id HTTP / gRPC field names. X-Caller-Id / caller-id is
// reused from the Public flavor so dashboards and labels stay stable
// when an operator upgrades Public → HMAC.
//
// Stability: stable
const (
	HMACCallerIDTimestampHeader = "X-Caller-Timestamp"
	HMACCallerIDKidHeader       = "X-Caller-Kid"
	HMACCallerIDSignatureHeader = "X-Caller-Signature"

	HMACCallerIDTimestampMetadata = "caller-timestamp"
	HMACCallerIDKidMetadata       = "caller-kid"
	HMACCallerIDSignatureMetadata = "caller-signature"
)

// Verification errors. The seam's "err → unknown" rule collapses these
// to the "unknown" caller label today; once WithCallerIDEnforce ships
// (plan §Caller-ID enforce-mode), the same errors short-circuit the
// request with 401 / Unauthenticated.
var (
	errCallerIDHMACNotConfigured     = errors.New("caller-id hmac: no secret configured")
	errCallerIDHMACMalformed         = errors.New("caller-id hmac: malformed token")
	errCallerIDHMACUnknownKid        = errors.New("caller-id hmac: unknown kid")
	errCallerIDHMACTimestampTooOld   = errors.New("caller-id hmac: timestamp too old")
	errCallerIDHMACTimestampTooNew   = errors.New("caller-id hmac: timestamp too new")
	errCallerIDHMACSignatureMismatch = errors.New("caller-id hmac: signature mismatch")
)

// WithCallerIDHMAC installs the HMAC caller-id extractor. The gateway
// verifies (caller_id, timestamp, kid, sig) on every request — unsigned
// (no signature field) is treated as anonymous and resolves to the
// "unknown" caller label; signed-but-invalid returns an error so
// WithCallerIDEnforce can short-circuit on it once shipped.
//
// Production default. Public mode (WithCallerIDPublic) is forgeable
// and intended for dev / mTLS-via-proxy deployments; this option is
// the production answer for untrusted-network ingress.
//
// Wire format (HTTP / gRPC):
//   - X-Caller-Id          / caller-id          : caller identity string
//   - X-Caller-Timestamp   / caller-timestamp   : unix seconds
//   - X-Caller-Kid         / caller-kid         : optional rotation key id
//   - X-Caller-Signature   / caller-signature   : base64 HMAC-SHA256
//
// Pair with SignCallerIDToken / SignCallerIDTokenWithKid on the signer
// side so callers holding the shared secret mint compatible tokens
// locally. Plan §Caller-ID.
//
// Stability: stable
func WithCallerIDHMAC(o CallerIDHMACOptions) Option {
	return WithCallerIDExtractor(o.extractor())
}

func (o CallerIDHMACOptions) extractor() CallerIDExtractor {
	return func(ctx context.Context) (string, error) {
		return o.resolve(ctx, time.Now())
	}
}

// resolve is the testable core; `now` is parameterised so tests can
// pin skew-window edges without monkey-patching time.Now.
func (o CallerIDHMACOptions) resolve(ctx context.Context, now time.Time) (string, error) {
	if !o.hasAnySecret() {
		return "", errCallerIDHMACNotConfigured
	}
	callerID, ts, kid, sigB64, present := readCallerHMACFields(ctx)
	if !present {
		// No signature at all → anonymous, not malformed. Lets a
		// service running unauthenticated traffic through to "unknown"
		// without tripping the err path; WithCallerIDEnforce
		// promotes anonymous to a 401 separately.
		return "", nil
	}
	if callerID == "" || ts == "" || sigB64 == "" {
		return "", errCallerIDHMACMalformed
	}
	tsUnix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errCallerIDHMACMalformed
	}
	secret, ok := o.lookupSecret(kid)
	if !ok {
		return "", errCallerIDHMACUnknownKid
	}
	skew := o.SkewWindow
	if skew == 0 {
		skew = defaultSkewWindow
	}
	nowUnix := now.Unix()
	if tsUnix < nowUnix-int64(skew.Seconds()) {
		return "", errCallerIDHMACTimestampTooOld
	}
	if tsUnix > nowUnix+int64(skew.Seconds()) {
		return "", errCallerIDHMACTimestampTooNew
	}
	expected := computeCallerIDHMAC(secret, kid, callerID, tsUnix)
	provided, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(expected, provided) {
		return "", errCallerIDHMACSignatureMismatch
	}
	return callerID, nil
}

func (o CallerIDHMACOptions) hasAnySecret() bool {
	if len(o.Secret) > 0 {
		return true
	}
	for _, v := range o.Secrets {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

func (o CallerIDHMACOptions) lookupSecret(kid string) ([]byte, bool) {
	if kid == "" && len(o.Secret) > 0 {
		return o.Secret, true
	}
	if v, ok := o.Secrets[kid]; ok && len(v) > 0 {
		return v, true
	}
	return nil, false
}

// readCallerHMACFields pulls the four HMAC fields from HTTP headers or
// gRPC metadata. HTTP wins when both happen to be present (mirrors the
// Public extractor's resolution order). The signature field is the
// presence indicator — absent → caller is anonymous, not malformed.
func readCallerHMACFields(ctx context.Context) (callerID, ts, kid, sig string, present bool) {
	if r := HTTPRequestFromContext(ctx); r != nil {
		if v := r.Header.Get(HMACCallerIDSignatureHeader); v != "" {
			return r.Header.Get(PublicCallerIDHeader),
				r.Header.Get(HMACCallerIDTimestampHeader),
				r.Header.Get(HMACCallerIDKidHeader),
				v, true
		}
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := firstMetadataValue(md, HMACCallerIDSignatureMetadata); v != "" {
			return firstMetadataValue(md, PublicCallerIDMetadata),
				firstMetadataValue(md, HMACCallerIDTimestampMetadata),
				firstMetadataValue(md, HMACCallerIDKidMetadata),
				v, true
		}
	}
	return "", "", "", "", false
}

func firstMetadataValue(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// computeCallerIDHMAC is the canonical signing function. Empty kid →
// payload "<caller_id>\n<timestamp>"; non-empty kid →
// "<kid>\n<caller_id>\n<timestamp>". Mirrors
// computeSubscribeHMAC in gw/auth_subscribe.go so the verification
// primitive is one design across subscriptions and caller identity.
func computeCallerIDHMAC(secret []byte, kid, callerID string, timestampUnix int64) []byte {
	h := hmac.New(sha256.New, secret)
	if kid == "" {
		fmt.Fprintf(h, "%s\n%d", callerID, timestampUnix)
	} else {
		fmt.Fprintf(h, "%s\n%s\n%d", kid, callerID, timestampUnix)
	}
	return h.Sum(nil)
}

// SignCallerIDToken returns (base64-sig, timestamp) for a caller_id
// using the given secret. Sibling of SignSubscribeToken — same shape,
// scoped to caller identity instead of subscription channel.
//
// ttlSeconds is currently informational; the gateway's SkewWindow
// bounds replay regardless. A future version may pin tokens to an
// explicit expiry rather than a wall-clock window.
//
// Stability: stable
func SignCallerIDToken(secret []byte, callerID string, ttlSeconds int64) (sigB64 string, timestampUnix int64) {
	sigB64, _, timestampUnix = SignCallerIDTokenWithKid(secret, "", callerID, ttlSeconds)
	return sigB64, timestampUnix
}

// SignCallerIDTokenWithKid is the rotation-aware signer. Pair with
// `CallerIDHMACOptions.Secrets[kid]` on the verifier side and send
// `kid` alongside `caller_id` / `timestamp` / `signature` in the
// request headers (or gRPC metadata). Empty kid produces a legacy
// token that verifies against `Secret`.
//
// Stability: stable
func SignCallerIDTokenWithKid(secret []byte, kid, callerID string, ttlSeconds int64) (sigB64, kidOut string, timestampUnix int64) {
	_ = ttlSeconds
	timestampUnix = time.Now().Unix()
	mac := computeCallerIDHMAC(secret, kid, callerID, timestampUnix)
	return base64.StdEncoding.EncodeToString(mac), kid, timestampUnix
}
