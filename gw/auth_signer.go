package gateway

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// signAuthOutcome categorizes the result of the SignSubscriptionToken
// bearer check. Used as the `code` label on
// go_api_gateway_sign_auth_total — mirrors the subscribe-auth metric's
// code-string shape so dashboards compose the same way.
const (
	signAuthInProcess        = "in_process"
	signAuthOKSigner         = "ok_signer"
	signAuthOKBearer         = "ok_bearer"
	signAuthMissingBearer    = "missing_bearer"
	signAuthDeniedBearer     = "denied_bearer"
	signAuthNoTokenConfigure = "no_token_configured"
)

// checkSignAuth gates the gRPC SignSubscriptionToken RPC. Returns
// (outcome, allow). Outcome is the metric label; `allow` says whether
// to admit the call.
//
//   - In-process call (no gRPC peer in ctx): bypass — the trust
//     boundary is the embedder, not the wire. Library users who call
//     cp.SignSubscriptionToken directly (huma /admin/sign handler,
//     in-process tests) all land here.
//   - Wire call: require Authorization metadata. The presented bearer
//     must constant-time-match WithSignerSecret (if set) OR the admin
//     boot token. Either is OK; mismatch / missing → deny.
func (g *Gateway) checkSignAuth(ctx context.Context) (outcome string, allow bool) {
	if _, ok := peer.FromContext(ctx); !ok {
		return signAuthInProcess, true
	}
	md, _ := metadata.FromIncomingContext(ctx)
	bearer := bearerFromMetadata(md)
	if bearer == "" {
		return signAuthMissingBearer, false
	}
	got, err := decodeAdminTokenString(bearer)
	if err != nil {
		return signAuthDeniedBearer, false
	}
	signer := g.cfg.signerSecret
	admin := g.cfg.adminToken
	if len(signer) == 0 && len(admin) == 0 {
		return signAuthNoTokenConfigure, false
	}
	if len(signer) > 0 && constantTimeEqualBytes(got, signer) {
		return signAuthOKSigner, true
	}
	if len(admin) > 0 && constantTimeEqualBytes(got, admin) {
		return signAuthOKBearer, true
	}
	return signAuthDeniedBearer, false
}

// bearerFromMetadata pulls the trimmed bearer from the
// `authorization` metadata key, normalizing the "Bearer " prefix.
// gRPC metadata keys are lowercase per spec.
func bearerFromMetadata(md metadata.MD) string {
	v := md.Get("authorization")
	if len(v) == 0 {
		return ""
	}
	const prefix = "Bearer "
	s := v[0]
	if !strings.HasPrefix(s, prefix) {
		return ""
	}
	return strings.TrimSpace(s[len(prefix):])
}

func constantTimeEqualBytes(a, b []byte) bool {
	if subtle.ConstantTimeEq(int32(len(a)), int32(len(b))) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
