package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

// Plan §Caller-ID seam — these pin (1) the Public extractor reads
// both the HTTP header and the gRPC metadata key, (2) the seam wins
// over the legacy WithCallerHeaders allowlist when both are set, and
// (3) the no-extractor path still honours WithCallerHeaders.

func TestPublicCallerIDExtractor_HTTPHeader(t *testing.T) {
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "billing")
	ctx := WithHTTPRequest(context.Background(), r)
	got, err := publicCallerIDExtractor(ctx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "billing" {
		t.Errorf("got %q, want billing", got)
	}
}

func TestPublicCallerIDExtractor_GRPCMetadata(t *testing.T) {
	md := metadata.Pairs(PublicCallerIDMetadata, "users")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	got, err := publicCallerIDExtractor(ctx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "users" {
		t.Errorf("got %q, want users", got)
	}
}

func TestPublicCallerIDExtractor_Anonymous(t *testing.T) {
	got, err := publicCallerIDExtractor(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (anonymous)", got)
	}
}

func TestPublicCallerIDExtractor_HTTPHeaderTakesPrecedenceOverGRPC(t *testing.T) {
	// Mixed context (gRPC ingress shouldn't have HTTP, but the
	// resolver order is observable contract — HTTP wins so future
	// hybrids stay deterministic).
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "from-http")
	ctx := WithHTTPRequest(context.Background(), r)
	md := metadata.Pairs(PublicCallerIDMetadata, "from-grpc")
	ctx = metadata.NewIncomingContext(ctx, md)
	got, _ := publicCallerIDExtractor(ctx)
	if got != "from-http" {
		t.Errorf("got %q, want from-http", got)
	}
}

func TestResolveCallerID_ExtractorWins(t *testing.T) {
	// When both the extractor and the legacy header allowlist are
	// configured, the extractor result takes precedence.
	r, _ := newRequestWithHeader("X-Caller-Service", "legacy-billing")
	r.Header.Set(PublicCallerIDHeader, "seam-billing")
	ctx := WithHTTPRequest(context.Background(), r)
	got := resolveCallerID(ctx, publicCallerIDExtractor, []string{"X-Caller-Service"})
	if got != "seam-billing" {
		t.Errorf("got %q, want seam-billing", got)
	}
}

func TestResolveCallerID_FallsBackToHeaders(t *testing.T) {
	// No extractor → legacy header allowlist still applies.
	r, _ := newRequestWithHeader("X-Caller-Service", "legacy-users")
	ctx := WithHTTPRequest(context.Background(), r)
	got := resolveCallerID(ctx, nil, []string{"X-Caller-Service"})
	if got != "legacy-users" {
		t.Errorf("got %q, want legacy-users", got)
	}
}

func TestResolveCallerID_ExtractorErrorCollapsesToUnknown(t *testing.T) {
	ex := func(ctx context.Context) (string, error) {
		return "would-be-alice", errors.New("forged")
	}
	got := resolveCallerID(context.Background(), ex, nil)
	if got != "unknown" {
		t.Errorf("got %q, want unknown", got)
	}
}

func TestResolveCallerID_AnonymousCollapsesToUnknown(t *testing.T) {
	got := resolveCallerID(context.Background(), publicCallerIDExtractor, nil)
	if got != "unknown" {
		t.Errorf("got %q, want unknown", got)
	}
}

// End-to-end: WithCallerIDPublic plumbs through to the dispatch
// recording site so Snapshot rows carry the resolved caller. Mirrors
// TestSnapshot_CallerDimension (stats_test.go) but exercises the seam
// instead of WithCallerHeaders.
func TestSnapshot_CallerDimension_PublicExtractor(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerIDPublic(),
	)
	t.Cleanup(g.Close)

	rA, _ := newRequestWithHeader(PublicCallerIDHeader, "billing")
	rB, _ := newRequestWithHeader(PublicCallerIDHeader, "users")
	ctxA := WithHTTPRequest(context.Background(), rA)
	ctxB := WithHTTPRequest(context.Background(), rB)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxB, "greeter", "v1", "Hello", 9*time.Millisecond, nil)

	rows := g.Snapshot(time.Minute, now)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per caller), got %d: %+v", len(rows), rows)
	}
	byCaller := map[string]uint64{}
	for _, r := range rows {
		byCaller[r.Caller] = r.Count
	}
	if byCaller["billing"] != 2 {
		t.Errorf("billing count: got %d, want 2", byCaller["billing"])
	}
	if byCaller["users"] != 1 {
		t.Errorf("users count: got %d, want 1", byCaller["users"])
	}
}

// gRPC ingress flavor: caller-id arriving as gRPC metadata also lands
// on Snapshot rows when WithCallerIDPublic is configured.
func TestSnapshot_CallerDimension_PublicExtractor_GRPC(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerIDPublic(),
	)
	t.Cleanup(g.Close)

	md := metadata.Pairs(PublicCallerIDMetadata, "payments")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	g.cfg.metrics.RecordDispatch(ctx, "greeter", "v1", "Hello", 3*time.Millisecond, nil)

	rows := g.Snapshot(time.Minute, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Caller != "payments" {
		t.Errorf("caller: got %q, want payments", rows[0].Caller)
	}
}
