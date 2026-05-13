package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func newTestRequest(remote string) *http.Request {
	r, _ := http.NewRequest("GET", "http://x/graphql", nil)
	r.RemoteAddr = remote
	return r
}

func TestWSLimiter_DisabledWhenAllZero(t *testing.T) {
	l := newWSLimiter(WSLimitOptions{})
	if l != nil {
		t.Fatalf("expected nil limiter when all caps zero, got %+v", l)
	}
	// nil-safe acquire on the hot path.
	rel, _, ok := l.acquire(newTestRequest("1.2.3.4:9999"))
	if !ok {
		t.Fatalf("nil limiter must admit")
	}
	if rel == nil {
		t.Fatalf("nil limiter must return non-nil release")
	}
	rel()
}

func TestWSLimiter_PerIPCap(t *testing.T) {
	l := newWSLimiter(WSLimitOptions{MaxPerIP: 2})
	r := newTestRequest("1.2.3.4:1111")

	rel1, _, ok := l.acquire(r)
	if !ok {
		t.Fatalf("acquire #1: want ok")
	}
	rel2, _, ok := l.acquire(r)
	if !ok {
		t.Fatalf("acquire #2: want ok")
	}
	_, reason, ok := l.acquire(r)
	if ok || reason != "max_per_ip" {
		t.Fatalf("acquire #3: want !ok max_per_ip, got ok=%v reason=%q", ok, reason)
	}

	// Different IP isolated.
	r2 := newTestRequest("5.6.7.8:1111")
	rel3, _, ok := l.acquire(r2)
	if !ok {
		t.Fatalf("acquire other-IP: want ok")
	}
	rel3()

	rel1()
	// After release one slot frees up.
	rel4, _, ok := l.acquire(r)
	if !ok {
		t.Fatalf("acquire after release: want ok")
	}
	rel4()
	rel2()
}

func TestWSLimiter_TokenBucket(t *testing.T) {
	l := newWSLimiter(WSLimitOptions{RatePerSec: 2, Burst: 2})
	base := time.Unix(1700000000, 0)
	l.now = func() time.Time { return base }
	r := newTestRequest("1.2.3.4:1111")

	// Burst of 2 — two go through then we're empty.
	for i := 0; i < 2; i++ {
		if _, _, ok := l.acquire(r); !ok {
			t.Fatalf("burst slot %d: want ok", i)
		}
	}
	if _, reason, ok := l.acquire(r); ok || reason != "rate_limit" {
		t.Fatalf("third burst slot: want rate_limit, got ok=%v reason=%q", ok, reason)
	}

	// Advance ~0.6s — at 2/sec we recover ~1.2 tokens, so one more
	// acquire admits.
	l.now = func() time.Time { return base.Add(600 * time.Millisecond) }
	if _, _, ok := l.acquire(r); !ok {
		t.Fatalf("after refill: want ok")
	}
	// Bucket back below 1.
	if _, reason, ok := l.acquire(r); ok || reason != "rate_limit" {
		t.Fatalf("post-refill 2nd: want rate_limit, got ok=%v reason=%q", ok, reason)
	}

	// Different IP unaffected.
	r2 := newTestRequest("5.6.7.8:1111")
	if _, _, ok := l.acquire(r2); !ok {
		t.Fatalf("other-IP under its own bucket: want ok")
	}
}

func TestWSLimiter_TokenRefundOnMaxPerIPReject(t *testing.T) {
	// When MaxPerIP wins, the rate-bucket token must be refunded so
	// the caller isn't double-docked.
	l := newWSLimiter(WSLimitOptions{MaxPerIP: 1, RatePerSec: 10, Burst: 10})
	base := time.Unix(1700000000, 0)
	l.now = func() time.Time { return base }
	r := newTestRequest("1.2.3.4:1111")

	rel, _, ok := l.acquire(r)
	if !ok {
		t.Fatalf("acquire #1: want ok")
	}
	// Many rejected by max_per_ip — each should refund the token.
	// Without refund, after burst+1 attempts we'd hit rate_limit;
	// with refund, every reject stays "max_per_ip".
	for i := 0; i < 50; i++ {
		_, reason, ok := l.acquire(r)
		if ok {
			t.Fatalf("reject #%d: want !ok", i)
		}
		if reason != "max_per_ip" {
			t.Fatalf("reject #%d: want max_per_ip, got %q (refund broken — token bucket exhausted)", i, reason)
		}
	}
	rel()
}

func TestWSLimiter_TrustedIPBypass(t *testing.T) {
	l := newWSLimiter(WSLimitOptions{
		MaxPerIP:   1,
		RatePerSec: 10,
		Burst:      10,
		TrustedIPs: []string{"127.0.0.1"},
	})
	r := newTestRequest("127.0.0.1:1111")
	// Should admit unlimited times.
	for i := 0; i < 50; i++ {
		if _, _, ok := l.acquire(r); !ok {
			t.Fatalf("trusted IP rejected at i=%d", i)
		}
	}
	// Untrusted IP still capped by MaxPerIP.
	r2 := newTestRequest("8.8.8.8:1111")
	if _, _, ok := l.acquire(r2); !ok {
		t.Fatalf("untrusted #1: want ok")
	}
	_, reason, ok := l.acquire(r2)
	if ok {
		t.Fatalf("untrusted #2: want reject")
	}
	if reason != "max_per_ip" {
		t.Fatalf("untrusted #2: want max_per_ip, got %q", reason)
	}
}

// wsLimitMetrics records RecordWSRejected calls. Embeds noopMetrics
// for everything else.
type wsLimitMetrics struct {
	noopMetrics
	mu      sync.Mutex
	reasons map[string]int
}

func (m *wsLimitMetrics) RecordWSRejected(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reasons == nil {
		m.reasons = map[string]int{}
	}
	m.reasons[reason]++
}

func (m *wsLimitMetrics) snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.reasons))
	for k, v := range m.reasons {
		out[k] = v
	}
	return out
}

// TestWSLimit_HTTPRejects429 wires a real Gateway with MaxPerIP=1,
// confirms the second concurrent upgrade gets 429 + metric.
func TestWSLimit_HTTPRejects429(t *testing.T) {
	rec := &wsLimitMetrics{}
	f := newSubFixture(t, WithMetrics(rec), WithWSLimit(WSLimitOptions{MaxPerIP: 1}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn1 := dialWS(t, ctx, f.wsURL)
	defer conn1.Close(websocket.StatusNormalClosure, "")

	// Second upgrade from the same loopback peer should get 429.
	resp, err := http.Get(strings.Replace(f.wsURL, "ws://", "http://", 1))
	if err == nil {
		resp.Body.Close()
	}
	// Plain GET doesn't trigger the WS branch — re-issue with
	// an actual Upgrade. nhooyr's websocket.Dial returns the *http.Response
	// in the second return value on failure.
	_, httpResp, err := websocket.Dial(ctx, f.wsURL, &websocket.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err == nil {
		t.Fatalf("expected dial to fail with 429, got success")
	}
	if httpResp == nil {
		t.Fatalf("expected non-nil http.Response on rejected dial: %v", err)
	}
	if httpResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status: want 429, got %d (%v)", httpResp.StatusCode, err)
	}

	snap := rec.snapshot()
	if snap["max_per_ip"] < 1 {
		t.Fatalf("expected at least one max_per_ip reject, got %v", snap)
	}

	// Closing conn1 frees the slot so a new dial succeeds.
	conn1.Close(websocket.StatusNormalClosure, "")
	// Give the deferred release a moment to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn3, _, err := websocket.Dial(ctx, f.wsURL, &websocket.DialOptions{
			Subprotocols: []string{"graphql-transport-ws"},
		})
		if err == nil {
			conn3.Close(websocket.StatusNormalClosure, "")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("post-release dial never admitted")
}

// TestWSLimit_TrustedBypassThroughHTTP confirms the trusted-IP
// allowlist works through the real Handler path.
func TestWSLimit_TrustedBypassThroughHTTP(t *testing.T) {
	rec := &wsLimitMetrics{}
	f := newSubFixture(t, WithMetrics(rec), WithWSLimit(WSLimitOptions{
		MaxPerIP:   1,
		TrustedIPs: []string{"127.0.0.1", "::1"},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn1 := dialWS(t, ctx, f.wsURL)
	defer conn1.Close(websocket.StatusNormalClosure, "")
	conn2 := dialWS(t, ctx, f.wsURL)
	defer conn2.Close(websocket.StatusNormalClosure, "")

	if reason := rec.snapshot()["max_per_ip"]; reason != 0 {
		t.Fatalf("trusted-IP path emitted %d rejects", reason)
	}
}

// TestWSLimit_DisabledByDefault confirms an unconfigured gateway
// places no caps.
func TestWSLimit_DisabledByDefault(t *testing.T) {
	rec := &wsLimitMetrics{}
	f := newSubFixture(t, WithMetrics(rec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var conns []*websocket.Conn
	for i := 0; i < 5; i++ {
		c := dialWS(t, ctx, f.wsURL)
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}
	if r := rec.snapshot(); len(r) != 0 {
		t.Fatalf("unexpected rejects: %v", r)
	}
}

var _ = httptest.NewServer // keep import; subFixture already uses it
var _ atomic.Int32         // keep
