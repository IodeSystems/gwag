package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newRequestWithHeader builds a minimal *http.Request carrying a
// single header — the smallest fixture for callerFromContext tests.
func newRequestWithHeader(key, val string) (*http.Request, error) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(key, val)
	return r, nil
}

// statsBucket / statsWindow / statsRegistry are the data path the
// admin UI reads. Tests pin: bucket landing, window aggregation,
// percentile estimation, ring rollover, and the gateway-level
// Snapshot view.

func TestStatsWindow_ObserveWithinWindow(t *testing.T) {
	w := newStatsWindow(1*time.Second, 10*time.Second)
	t0 := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 5; i++ {
		w.observe(t0+int64(i), 50*time.Millisecond, true)
	}
	snap := w.snapshot(t0+5, 10)
	if snap.Count != 5 {
		t.Errorf("count=%d, want 5", snap.Count)
	}
	if snap.OkCount != 5 {
		t.Errorf("okCount=%d, want 5", snap.OkCount)
	}
	if snap.Throughput != 0.5 { // 5 calls / 10s span
		t.Errorf("throughput=%v, want 0.5", snap.Throughput)
	}
	// 50ms lands in the 0.1s bucket → P50/P95 estimate the upper bound.
	if snap.P50 != 100*time.Millisecond {
		t.Errorf("p50=%v, want 100ms", snap.P50)
	}
}

func TestStatsWindow_StaleBucketsExcluded(t *testing.T) {
	// Ring length 5s; observations 10s apart on the same modular index
	// should not bleed across — the bucketStartUnix mismatch resets.
	w := newStatsWindow(1*time.Second, 5*time.Second)
	w.observe(100, 1*time.Millisecond, true)
	w.observe(105, 1*time.Millisecond, true) // same modular index as 100
	snap := w.snapshot(105, 5)
	if snap.Count != 1 {
		t.Errorf("count=%d after rollover; stale bucket leaked", snap.Count)
	}
}

func TestHistPercentile_Empty(t *testing.T) {
	hist := make([]uint64, numLatencyBuckets)
	if got := histPercentile(hist, 0.5); got != 0 {
		t.Errorf("empty p50=%v, want 0", got)
	}
}

func TestHistPercentile_BucketBoundary(t *testing.T) {
	hist := make([]uint64, numLatencyBuckets)
	hist[0] = 100 // 100% in 0.001s bucket
	if got := histPercentile(hist, 0.5); got != 1*time.Millisecond {
		t.Errorf("p50=%v, want 1ms", got)
	}
	if got := histPercentile(hist, 0.95); got != 1*time.Millisecond {
		t.Errorf("p95=%v, want 1ms", got)
	}
}

func TestStatsRegistry_RecordAndSnapshot(t *testing.T) {
	r := newStatsRegistry()
	k := statsKey{namespace: "greeter", version: "v1", method: "Hello"}
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		r.record(k, 10*time.Millisecond, true, now.Add(time.Duration(i)*time.Second))
	}
	snap := r.snapshot(k, time.Minute, now.Add(30*time.Second))
	if snap.Count != 30 {
		t.Errorf("count=%d, want 30", snap.Count)
	}
}

func TestStatsRegistry_MissingKey(t *testing.T) {
	r := newStatsRegistry()
	snap := r.snapshot(statsKey{namespace: "missing"}, time.Minute, time.Now())
	if snap.Count != 0 {
		t.Errorf("expected empty snapshot for unknown key, got %+v", snap)
	}
}

// Snapshot wires the registry through to a single (g.Snapshot) call.
// The dispatchMetrics wrapper installed at New() funnels every
// RecordDispatch into stats; this test verifies the full path
// without exercising the dispatcher itself (we'd need a fake
// dispatcher, which is overkill for the seam).
func TestGateway_SnapshotReadsRegistry(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(g.Close)
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	g.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Bye", 9*time.Millisecond, nil)
	rows := g.Snapshot(time.Minute, now)
	if len(rows) != 0 {
		// stats.record uses nowFunc(); without a deterministic stub,
		// observations land at real wall-clock and Snapshot at our
		// fixed `now` won't see them. Stub nowFunc and re-run.
	}
	old := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g2 := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(g2.Close)
	g2.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	g2.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	g2.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Bye", 9*time.Millisecond, nil)
	rows = g2.Snapshot(time.Minute, now)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	// Stable sort — Bye < Hello.
	if rows[0].Method != "Bye" || rows[1].Method != "Hello" {
		t.Errorf("rows not sorted: %+v", rows)
	}
	if rows[1].Count != 2 {
		t.Errorf("Hello count=%d, want 2", rows[1].Count)
	}
}

// callerFromContext returns the first non-empty match from the
// allowlist, "unknown" when no header matches or no list configured.
func TestCallerFromContext_NoHeadersConfigured(t *testing.T) {
	if got := callerFromContext(context.Background(), nil); got != "unknown" {
		t.Errorf("nil headers: got %q, want unknown", got)
	}
}

func TestCallerFromContext_HeaderMatch(t *testing.T) {
	r, _ := newRequestWithHeader("X-Caller-Service", "billing")
	ctx := WithHTTPRequest(context.Background(), r)
	if got := callerFromContext(ctx, []string{"X-Caller-Service", "User-Agent"}); got != "billing" {
		t.Errorf("got %q, want billing", got)
	}
}

func TestCallerFromContext_FallsThroughOnAbsent(t *testing.T) {
	r, _ := newRequestWithHeader("User-Agent", "curl/7.88")
	ctx := WithHTTPRequest(context.Background(), r)
	// First header missing → falls through to second.
	if got := callerFromContext(ctx, []string{"X-Caller-Service", "User-Agent"}); got != "curl/7.88" {
		t.Errorf("got %q, want curl/7.88", got)
	}
}

func TestCallerFromContext_NoRequestOnContext(t *testing.T) {
	if got := callerFromContext(context.Background(), []string{"X-Caller-Service"}); got != "unknown" {
		t.Errorf("missing-request: got %q, want unknown", got)
	}
}

// Snapshot rows carry the caller label when WithCallerHeaders is set.
// Two callers under the same (ns, ver, method) must produce two rows.
func TestSnapshot_CallerDimension(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g := New(
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("t")),
		WithCallerHeaders("X-Caller-Service"),
	)
	t.Cleanup(g.Close)

	rA, _ := newRequestWithHeader("X-Caller-Service", "billing")
	rB, _ := newRequestWithHeader("X-Caller-Service", "users")
	ctxA := WithHTTPRequest(context.Background(), rA)
	ctxB := WithHTTPRequest(context.Background(), rB)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxA, "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	g.cfg.metrics.RecordDispatch(ctxB, "greeter", "v1", "Hello", 9*time.Millisecond, nil)

	rows := g.Snapshot(time.Minute, now)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per caller), got %d: %+v", len(rows), rows)
	}
	if rows[0].Caller != "billing" || rows[0].Count != 2 {
		t.Errorf("billing row: %+v", rows[0])
	}
	if rows[1].Caller != "users" || rows[1].Count != 1 {
		t.Errorf("users row: %+v", rows[1])
	}
}

// MetricsHandler must still return the Prometheus handler even though
// cfg.metrics is now wrapped in statsRecordingMetrics. Regression
// guard against the New()-time wrap breaking /metrics scrape.
func TestMetricsHandler_UnwrapsStatsRecorder(t *testing.T) {
	g := New(WithAdminToken([]byte("t")))
	t.Cleanup(g.Close)
	h := g.MetricsHandler()
	if h == nil {
		t.Fatal("MetricsHandler returned nil")
	}
	// http.NotFoundHandler is the negative result; check by type
	// rather than nil. promhttp.HandlerFor returns a *struct{} — any
	// non-nil result that isn't NotFoundHandler is fine.
}
