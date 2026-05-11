package gateway

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// CallerIDExtractor resolves the caller-id for an in-flight request.
// It is consulted by the dispatch metric / stats recording site on
// every dispatch and is the single seam covering HTTP, WebSocket, and
// gRPC ingress — the three transports the gateway exposes.
//
// Return values:
//   - ("alice", nil): caller identified as "alice".
//   - ("", nil):       anonymous caller. Recorded as "unknown".
//   - ("", err):       extraction failed (e.g. HMAC didn't verify).
//     Recorded as "unknown" today; once
//     WithCallerIDEnforce ships (plan §Caller-ID),
//     err short-circuits the request with 401/16.
//
// Plan §Caller-ID — one seam, four implementations: Public, HMAC,
// Delegated, mTLS-via-proxy. v1 ships Public via WithCallerIDPublic;
// the rest layer on without touching the seam.
type CallerIDExtractor func(ctx context.Context) (string, error)

// PublicCallerIDHeader is the HTTP / WebSocket header consulted by
// the WithCallerIDPublic extractor.
const PublicCallerIDHeader = "X-Caller-Id"

// PublicCallerIDMetadata is the gRPC metadata key consulted by the
// WithCallerIDPublic extractor. gRPC normalises keys to lower-case
// per the spec.
const PublicCallerIDMetadata = "caller-id"

// WithCallerIDExtractor installs a custom caller-id extraction
// function. The extractor runs at dispatch metric / stats recording
// time on every call.
//
// Built-in flavors: WithCallerIDPublic. If both WithCallerIDExtractor
// and WithCallerHeaders are set, the extractor wins; the older
// header-allowlist option stays as the no-extractor fallback so
// pre-seam adopters keep working unchanged.
func WithCallerIDExtractor(ex CallerIDExtractor) Option {
	return func(cfg *config) { cfg.callerIDExtractor = ex }
}

// WithCallerIDPublic installs the "public" extractor: trust the
// inbound X-Caller-Id HTTP header (also covers WebSocket via the
// upgrade request) and the caller-id gRPC metadata key on the
// incoming context. Forgeable by design — suitable for dev, behind
// an authenticated reverse proxy, or with mTLS-via-proxy terminating
// in front of the gateway. For untrusted-network production, use the
// HMAC flavor (plan §Caller-ID HMAC mode — next todo).
func WithCallerIDPublic() Option {
	return WithCallerIDExtractor(publicCallerIDExtractor)
}

// publicCallerIDExtractor is the implementation backing
// WithCallerIDPublic. HTTP wins over gRPC when both happen to be
// present (the gateway's GraphQL / huma surfaces are HTTP; the
// gRPC-incoming case is the gRPC ingress entry point).
func publicCallerIDExtractor(ctx context.Context) (string, error) {
	if r := HTTPRequestFromContext(ctx); r != nil {
		if v := r.Header.Get(PublicCallerIDHeader); v != "" {
			return v, nil
		}
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(PublicCallerIDMetadata); len(v) > 0 && v[0] != "" {
			return v[0], nil
		}
	}
	return "", nil
}

// resolveCallerID derives the caller string a metric or stats
// observation should carry. Prefers the configured extractor (the
// seam); falls back to the legacy WithCallerHeaders allowlist when no
// extractor is installed. Always returns a non-empty string —
// "unknown" stands in for anonymous or failed extraction.
func resolveCallerID(ctx context.Context, ex CallerIDExtractor, headers []string) string {
	if ex != nil {
		v, err := ex(ctx)
		if err != nil || v == "" {
			return "unknown"
		}
		return v
	}
	return callerFromContext(ctx, headers)
}
