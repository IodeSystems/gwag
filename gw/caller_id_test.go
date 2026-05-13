package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	ctx := withHTTPRequest(context.Background(), r)
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
	ctx := withHTTPRequest(context.Background(), r)
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
	ctx := withHTTPRequest(context.Background(), r)
	got := resolveCallerID(ctx, publicCallerIDExtractor, []string{"X-Caller-Service"})
	if got != "seam-billing" {
		t.Errorf("got %q, want seam-billing", got)
	}
}

func TestResolveCallerID_FallsBackToHeaders(t *testing.T) {
	// No extractor → legacy header allowlist still applies.
	r, _ := newRequestWithHeader("X-Caller-Service", "legacy-users")
	ctx := withHTTPRequest(context.Background(), r)
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
	ctxA := withHTTPRequest(context.Background(), rA)
	ctxB := withHTTPRequest(context.Background(), rB)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxB, "greeter", "v1", "Hello", 9*time.Millisecond, nil)

	rows := g.snapshot(time.Minute, now)
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

// WithCallerIDMetricsTopK cap — pure-unit table on the limiter, plus
// end-to-end through Snapshot rows so the stats registry honours the
// same admission set as the Prometheus label.

func TestCallerLimiter_DisabledIsPassthrough(t *testing.T) {
	// k <= 0 returns a nil limiter; Apply on nil receiver passes every
	// caller through unchanged (default pre-option behaviour).
	l := newCallerLimiter(0)
	if l != nil {
		t.Fatalf("k=0 should return nil limiter, got %+v", l)
	}
	if got := l.Apply("alice"); got != "alice" {
		t.Errorf("nil limiter: got %q, want alice", got)
	}
}

func TestCallerLimiter_AdmitsUnderCap(t *testing.T) {
	l := newCallerLimiter(2)
	if got := l.Apply("alice"); got != "alice" {
		t.Errorf("first admit: %q", got)
	}
	if got := l.Apply("bob"); got != "bob" {
		t.Errorf("second admit: %q", got)
	}
	// Re-seeing an admitted caller is a hit; it bumps LRU, doesn't
	// evict anyone.
	if got := l.Apply("alice"); got != "alice" {
		t.Errorf("hit: %q", got)
	}
}

func TestCallerLimiter_OverflowFoldsToOther(t *testing.T) {
	l := newCallerLimiter(1)
	if got := l.Apply("alice"); got != "alice" {
		t.Fatalf("first admit: %q", got)
	}
	// Second caller is over cap → __other__.
	if got := l.Apply("bob"); got != OtherCallerID {
		t.Errorf("overflow: got %q, want %q", got, OtherCallerID)
	}
	// Admitted caller still passes; cap doesn't drift on hit.
	if got := l.Apply("alice"); got != "alice" {
		t.Errorf("admitted-still-passes: %q", got)
	}
	// Bob remains over the cap until alice falls off — repeated hits
	// stay folded to __other__ rather than displacing alice.
	if got := l.Apply("bob"); got != OtherCallerID {
		t.Errorf("overflow-repeat: got %q, want %q", got, OtherCallerID)
	}
}

func TestCallerLimiter_LRUEviction(t *testing.T) {
	// Cap of 2; admit a, then b; hit on a bumps it to MRU; admit c
	// must evict b (the LRU), not a.
	l := newCallerLimiter(2)
	l.Apply("a")
	l.Apply("b")
	l.Apply("a") // bump a to MRU
	// c is a new caller; cap is full. We want c folded to __other__
	// (admission is admit-on-room, not evict-on-overflow). The LRU
	// position only matters when an *existing* admitted entry would
	// be re-ordered.
	if got := l.Apply("c"); got != OtherCallerID {
		t.Errorf("c over cap: got %q, want %q", got, OtherCallerID)
	}
	// snapshot MRU→LRU = [a, b]. b is LRU because a was bumped.
	snap := l.snapshot()
	if len(snap) != 2 || snap[0] != "a" || snap[1] != "b" {
		t.Errorf("snapshot: got %v, want [a b]", snap)
	}
}

// End-to-end: with WithCallerIDMetricsTopK(1), only the first
// admitted caller keeps its label; every subsequent distinct caller
// collapses into the __other__ row on the in-process stats registry.
func TestSnapshot_CallerLimiter_OtherRollup(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerIDPublic(),
		WithCallerIDMetricsTopK(1),
	)
	t.Cleanup(g.Close)

	for _, name := range []string{"billing", "billing", "users", "payments", "users"} {
		r, _ := newRequestWithHeader(PublicCallerIDHeader, name)
		ctx := withHTTPRequest(context.Background(), r)
		g.cfg.metrics.RecordDispatch(ctx, "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	}

	rows := g.snapshot(time.Minute, now)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (billing + __other__), got %d: %+v", len(rows), rows)
	}
	byCaller := map[string]uint64{}
	for _, r := range rows {
		byCaller[r.Caller] = r.Count
	}
	if byCaller["billing"] != 2 {
		t.Errorf("billing count: got %d, want 2", byCaller["billing"])
	}
	if byCaller[OtherCallerID] != 3 {
		t.Errorf("%s count: got %d, want 3 (users×2 + payments×1)", OtherCallerID, byCaller[OtherCallerID])
	}
}

// Prometheus scrape carries the same __other__ label the stats
// registry uses — both sides share one limiter so operators see the
// same overflow rollup in /metrics and in the admin UI.
func TestPrometheusScrape_CallerLimiter_OtherLabel(t *testing.T) {
	g := New(
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerIDPublic(),
		WithCallerIDMetricsTopK(1),
	)
	t.Cleanup(g.Close)

	for _, name := range []string{"billing", "users", "payments"} {
		r, _ := newRequestWithHeader(PublicCallerIDHeader, name)
		ctx := withHTTPRequest(context.Background(), r)
		g.cfg.metrics.RecordDispatch(ctx, "greeter", "v1", "Hello", 3*time.Millisecond, nil)
	}

	w := httptest.NewRecorder()
	scrape := httptest.NewRequest("GET", "/metrics", nil)
	g.MetricsHandler().ServeHTTP(w, scrape)
	body := w.Body.String()
	if !strings.Contains(body, `caller="billing"`) {
		t.Errorf("scrape missing caller=\"billing\": %s", body)
	}
	if !strings.Contains(body, `caller="`+OtherCallerID+`"`) {
		t.Errorf("scrape missing caller=%q: %s", OtherCallerID, body)
	}
	if strings.Contains(body, `caller="users"`) || strings.Contains(body, `caller="payments"`) {
		t.Errorf("scrape leaked over-cap callers: %s", body)
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

	rows := g.snapshot(time.Minute, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Caller != "payments" {
		t.Errorf("caller: got %q, want payments", rows[0].Caller)
	}
}

// Plan §Caller-ID + quota ladder — pins the enforce-mode switch.
// Without WithCallerIDEnforce, anonymous and extraction-error
// requests collapse to caller="unknown" and proceed normally; with
// WithCallerIDEnforce, those same requests short-circuit with
// CodeUnauthenticated.

func TestEnforceCallerID_ExtractorErrorRejects(t *testing.T) {
	ex := func(_ context.Context) (string, error) {
		return "ignored", errors.New("hmac mismatch")
	}
	err := enforceCallerID(context.Background(), ex, nil)
	if err == nil {
		t.Fatalf("expected rejection on extractor error")
	}
	var rej *rejection
	if !errors.As(err, &rej) {
		t.Fatalf("err type = %T, want *rejection", err)
	}
	if rej.Code != CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", rej.Code)
	}
	if !strings.Contains(rej.Msg, "hmac mismatch") {
		t.Errorf("msg = %q, want it to surface the extractor error", rej.Msg)
	}
}

func TestEnforceCallerID_AnonymousRejects(t *testing.T) {
	// Empty string from the extractor is the canonical "anonymous"
	// signal. Enforce rejects.
	err := enforceCallerID(context.Background(), publicCallerIDExtractor, nil)
	if err == nil {
		t.Fatalf("expected rejection on anonymous")
	}
	var rej *rejection
	if !errors.As(err, &rej) {
		t.Fatalf("err type = %T, want *rejection", err)
	}
	if rej.Code != CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", rej.Code)
	}
}

func TestEnforceCallerID_NoExtractorNoHeadersRejects(t *testing.T) {
	// Operator turned on enforce without wiring an extractor or a
	// header allowlist. We treat that as a misconfiguration loud
	// rather than silent: every dispatch rejects.
	err := enforceCallerID(context.Background(), nil, nil)
	if err == nil {
		t.Fatalf("expected rejection when no source is wired")
	}
	var rej *rejection
	if !errors.As(err, &rej) {
		t.Fatalf("err type = %T, want *rejection", err)
	}
	if rej.Code != CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", rej.Code)
	}
}

func TestEnforceCallerID_LegacyHeadersFallback(t *testing.T) {
	// Without an extractor, the legacy WithCallerHeaders allowlist
	// still satisfies enforce when a header matches.
	r, _ := newRequestWithHeader("X-Caller-Service", "billing")
	ctx := withHTTPRequest(context.Background(), r)
	if err := enforceCallerID(ctx, nil, []string{"X-Caller-Service"}); err != nil {
		t.Errorf("expected no rejection with legacy header present, got %v", err)
	}
}

func TestEnforceCallerID_LegacyHeadersMissingRejects(t *testing.T) {
	// Header allowlist configured but no matching header → "unknown"
	// from callerFromContext → enforce rejects.
	r, _ := newRequestWithHeader("X-Other", "irrelevant")
	ctx := withHTTPRequest(context.Background(), r)
	if err := enforceCallerID(ctx, nil, []string{"X-Caller-Service"}); err == nil {
		t.Errorf("expected rejection when configured header is missing")
	}
}

func TestEnforceCallerID_ExtractorAccepts(t *testing.T) {
	r, _ := newRequestWithHeader(PublicCallerIDHeader, "alice")
	ctx := withHTTPRequest(context.Background(), r)
	if err := enforceCallerID(ctx, publicCallerIDExtractor, nil); err != nil {
		t.Errorf("expected accept with extractor returning a value, got %v", err)
	}
}

func TestCallerIDEnforceMiddleware_DefaultIsIdentity(t *testing.T) {
	// Without WithCallerIDEnforce the middleware factory is identity
	// (zero overhead on the hot path).
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	mw := gw.callerIDEnforceMiddleware()
	called := false
	core := dispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		called = true
		return "ok", nil
	})
	got, err := mw(core).Dispatch(context.Background(), nil)
	if err != nil || got != "ok" || !called {
		t.Fatalf("identity passthrough failed: got=%v err=%v called=%v", got, err, called)
	}
}

func TestCallerIDEnforceMiddleware_RejectsAnonymous(t *testing.T) {
	gw := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithCallerIDPublic(),
		WithCallerIDEnforce(),
	)
	t.Cleanup(gw.Close)
	mw := gw.callerIDEnforceMiddleware()
	called := false
	core := dispatcherFunc(func(_ context.Context, _ map[string]any) (any, error) {
		called = true
		return "should not reach", nil
	})
	_, err := mw(core).Dispatch(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected rejection on anonymous request")
	}
	if called {
		t.Errorf("core dispatcher must not run when enforce rejects")
	}
	var rej *rejection
	if !errors.As(err, &rej) {
		t.Fatalf("err type = %T, want *rejection", err)
	}
	if rej.Code != CodeUnauthenticated {
		t.Errorf("code = %v, want CodeUnauthenticated", rej.Code)
	}
}

// End-to-end: WithCallerIDEnforce wired through to /graphql →
// dispatch on an OpenAPI op without a caller-id returns
// extensions.code=UNAUTHENTICATED on the GraphQL error envelope.
func TestEnforceCallerID_E2E_AnonymousSurfaces401Code(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(be.Close)

	gw := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithCallerIDPublic(),
		WithCallerIDEnforce(),
	)
	t.Cleanup(gw.Close)

	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	// No X-Caller-Id header → enforce should reject before any
	// upstream contact.
	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { getThing(id:\"1\") { id } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)

	var body struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rr.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("expected errors[] in response, got %s", rr.Body.String())
	}
	if body.Errors[0].Extensions.Code != "UNAUTHENTICATED" {
		t.Errorf("error code=%q, want UNAUTHENTICATED (body=%s)", body.Errors[0].Extensions.Code, rr.Body.String())
	}
}

// And the other side: WithCallerIDEnforce + a valid X-Caller-Id
// header proceeds normally (the dispatch reaches the upstream).
func TestEnforceCallerID_E2E_PresentCallerProceeds(t *testing.T) {
	var hit int
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(be.Close)

	gw := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithCallerIDPublic(),
		WithCallerIDEnforce(),
	)
	t.Cleanup(gw.Close)

	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { getThing(id:\"1\") { id } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PublicCallerIDHeader, "alice")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)

	if hit != 1 {
		t.Errorf("upstream hits = %d, want 1 (enforce should let signed traffic through)", hit)
	}
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err == nil && len(body.Errors) > 0 {
		t.Errorf("unexpected errors with caller-id present: %s", rr.Body.String())
	}
}
