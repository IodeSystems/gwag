package gateway

import (
	"context"
	"sort"
	"sync"
	"time"
)

// In-process rolling stats for the admin UI: 1m / 1h / 24h windowed
// call counts, throughput, p50/p95 latency. Backed by fixed-size ring
// buffers so the UI works without a Prometheus scraper. Prometheus
// stays canonical for long-window history and label cardinality;
// this is the live operator panel side of the story (plan §5 first
// commit).
//
// Memory budget per (ns, ver, method):
//
//	  1m window:  60 buckets × 1s
//	  1h window:  60 buckets × 1m
//	  24h window: 144 buckets × 10m
//	  total:      264 buckets × ~88B = ~23KB
//
// 100 namespaces × 20 methods × 23KB ≈ 46MB. Per-replica variants
// (plan §5 followup) multiply by replica count and were left out of
// this core to keep the footprint predictable; the Snapshot API is
// keyed-generic so adding replica is one more call site, not a new
// shape.

// statsKey identifies a single stats series. (caller) is "unknown"
// when the inbound request didn't match any header in WithCallerHeaders;
// per-replica dimensions come later via the same shape pattern.
type statsKey struct {
	namespace, version, method, caller string
}

// numLatencyBuckets sized so a single bucket fits in two cache lines
// alongside count + sum + ok-count (statsBucket below). Eight buckets
// cover sub-ms to multi-second with a final overflow bin.
const numLatencyBuckets = 8

// latencyBucketUpperBoundsSec is the inclusive upper bound of each
// histogram bucket in seconds. Logarithmic-ish coverage from 1ms to
// ~12.5s plus an overflow bin so any latency lands somewhere.
var latencyBucketUpperBoundsSec = [numLatencyBuckets]float64{
	0.001, 0.005, 0.025, 0.1, 0.5, 2.5, 12.5, 1e9,
}

// statsBucket accumulates observations falling within one bucket
// boundary. bucketStartUnix is the wall-clock second the bucket
// represents (truncated to bucketDur); when an observation arrives
// against a stale bucket (the ring has wrapped past it), the bucket
// is reset before the increment lands.
type statsBucket struct {
	bucketStartUnix int64
	count           uint64
	okCount         uint64
	sumNanos        uint64
	latency         [numLatencyBuckets]uint64
}

// statsWindow is a fixed-size ring of buckets. The bucket index for
// a given timestamp is `(t / bucketDurSec) % len(buckets)`; identity
// is checked via bucketStartUnix so callers naturally evict stale
// data without needing a janitor goroutine.
type statsWindow struct {
	bucketDurSec int64
	buckets      []statsBucket
}

func newStatsWindow(bucketDur, span time.Duration) *statsWindow {
	n := int(span / bucketDur)
	return &statsWindow{
		bucketDurSec: int64(bucketDur / time.Second),
		buckets:      make([]statsBucket, n),
	}
}

// observe records one dispatch outcome at `nowUnix` against this
// window. Returns immediately on success; nothing in the call path
// touches the global registry, so the cost is one bucket find + a
// handful of integer increments per dispatch.
func (w *statsWindow) observe(nowUnix int64, latency time.Duration, ok bool) {
	bucketStart := (nowUnix / w.bucketDurSec) * w.bucketDurSec
	idx := int((bucketStart / w.bucketDurSec)) % len(w.buckets)
	if idx < 0 {
		idx += len(w.buckets)
	}
	b := &w.buckets[idx]
	if b.bucketStartUnix != bucketStart {
		*b = statsBucket{bucketStartUnix: bucketStart}
	}
	b.count++
	if ok {
		b.okCount++
	}
	b.sumNanos += uint64(latency.Nanoseconds())
	secs := latency.Seconds()
	for i, ub := range latencyBucketUpperBoundsSec {
		if secs <= ub {
			b.latency[i]++
			break
		}
	}
}

// snapshot aggregates buckets covering [nowUnix-spanSec, nowUnix].
// Stale buckets (whose start fell outside the window) are skipped —
// they belong to an earlier wrap of the ring. Percentiles are derived
// from the aggregate latency histogram via histPercentile.
func (w *statsWindow) snapshot(nowUnix, spanSec int64) StatsSnapshot {
	var snap StatsSnapshot
	floor := nowUnix - spanSec
	var hist [numLatencyBuckets]uint64
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.bucketStartUnix < floor || b.bucketStartUnix > nowUnix {
			continue
		}
		snap.Count += b.count
		snap.OkCount += b.okCount
		for j := 0; j < numLatencyBuckets; j++ {
			hist[j] += b.latency[j]
		}
	}
	if spanSec > 0 {
		snap.Throughput = float64(snap.Count) / float64(spanSec)
	}
	snap.P50 = histPercentile(hist[:], 0.50)
	snap.P95 = histPercentile(hist[:], 0.95)
	return snap
}

// histPercentile estimates `p` over an aggregate latency histogram.
// Returns the bucket upper bound where the cumulative count crosses
// `p × total`. Treats the overflow bin (1e9 sentinel) as Inf-styled —
// surfaced as the bound itself, which the UI / serializer can clamp.
func histPercentile(hist []uint64, p float64) time.Duration {
	var total uint64
	for _, c := range hist {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := uint64(float64(total) * p)
	if target == 0 {
		target = 1
	}
	var cumul uint64
	for i, c := range hist {
		cumul += c
		if cumul >= target {
			return time.Duration(latencyBucketUpperBoundsSec[i] * float64(time.Second))
		}
	}
	return time.Duration(latencyBucketUpperBoundsSec[numLatencyBuckets-1] * float64(time.Second))
}

// methodStats wraps the three windows for one (ns, ver, method).
// One mutex protects all three; observations are quick enough that
// the scope is fine.
type methodStats struct {
	mu     sync.Mutex
	win1m  *statsWindow
	win1h  *statsWindow
	win24h *statsWindow
}

func newMethodStats() *methodStats {
	return &methodStats{
		win1m:  newStatsWindow(1*time.Second, 1*time.Minute),
		win1h:  newStatsWindow(1*time.Minute, 1*time.Hour),
		win24h: newStatsWindow(10*time.Minute, 24*time.Hour),
	}
}

// statsRecordingMetrics composes Metrics so every RecordDispatch
// observation also feeds the in-process stats registry. Embedding
// keeps the rest of the Metrics surface (auth, dwell, gauges) on the
// underlying impl with zero extra cost. Installed once at New() —
// every dispatcher receives the composed Metrics, so wiring is a
// single seam.
type statsRecordingMetrics struct {
	Metrics
	stats         *statsRegistry
	callerHeaders []string
}

// callerFromContext returns the first non-empty inbound header value
// matching `headers`, or "unknown" when none match (or no HTTP request
// is on the context, e.g. in-process registrations / cluster
// reconcilers). Plan §5: operators control header allowlist via
// WithCallerHeaders; default empty list always returns "unknown".
func callerFromContext(ctx context.Context, headers []string) string {
	if len(headers) == 0 {
		return "unknown"
	}
	r := HTTPRequestFromContext(ctx)
	if r == nil {
		return "unknown"
	}
	for _, h := range headers {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return "unknown"
}

// nowFunc is overridable for tests; production passes time.Now.
var nowFunc = time.Now

func (s *statsRecordingMetrics) RecordDispatch(ctx context.Context, namespace, version, method string, d time.Duration, err error) {
	s.Metrics.RecordDispatch(ctx, namespace, version, method, d, err)
	if s.stats != nil {
		caller := callerFromContext(ctx, s.callerHeaders)
		s.stats.record(statsKey{namespace: namespace, version: version, method: method, caller: caller}, d, err == nil, nowFunc())
	}
}

// statsRegistry is the gateway-wide map of (ns, ver, method) →
// methodStats. The map mutex covers create-on-record; per-method
// observations take the methodStats mutex to keep contention local.
type statsRegistry struct {
	mu      sync.RWMutex
	methods map[statsKey]*methodStats
}

func newStatsRegistry() *statsRegistry {
	return &statsRegistry{methods: map[statsKey]*methodStats{}}
}

// record observes one dispatch. now is injected so tests can drive
// deterministic boundaries; production calls pass time.Now().
func (s *statsRegistry) record(k statsKey, latency time.Duration, ok bool, now time.Time) {
	s.mu.RLock()
	m, exists := s.methods[k]
	s.mu.RUnlock()
	if !exists {
		s.mu.Lock()
		m, exists = s.methods[k]
		if !exists {
			m = newMethodStats()
			s.methods[k] = m
		}
		s.mu.Unlock()
	}
	nowUnix := now.Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.win1m.observe(nowUnix, latency, ok)
	m.win1h.observe(nowUnix, latency, ok)
	m.win24h.observe(nowUnix, latency, ok)
}

// snapshot reads one (ns, ver, method) at the given window and
// wall-clock. Returns the zero StatsSnapshot when the key has no
// recorded observations.
func (s *statsRegistry) snapshot(k statsKey, window time.Duration, now time.Time) StatsSnapshot {
	s.mu.RLock()
	m, ok := s.methods[k]
	s.mu.RUnlock()
	if !ok {
		return StatsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.windowFor(window)
	if w == nil {
		return StatsSnapshot{}
	}
	return w.snapshot(now.Unix(), int64(window/time.Second))
}

func (m *methodStats) windowFor(window time.Duration) *statsWindow {
	switch window {
	case 1 * time.Minute:
		return m.win1m
	case 1 * time.Hour:
		return m.win1h
	case 24 * time.Hour:
		return m.win24h
	}
	return nil
}

// MethodStatsSnapshot is one row of the operator panel: a single
// (namespace, version, method, caller) measured over a named window.
// Caller is "unknown" when no caller-header allowlist is configured
// or no allowed header was present on the inbound request. Public so
// admin endpoints / UI codegen can consume it directly.
type MethodStatsSnapshot struct {
	Namespace string
	Version   string
	Method    string
	Caller    string
	StatsSnapshot
}

// StatsSnapshot is the per-window aggregate. Percentiles are bucket
// upper-bound estimates — sufficient for "is anything pathologically
// slow" without dragging in t-digest. Throughput is calls/second
// across the window span (count / span_seconds).
type StatsSnapshot struct {
	Count      uint64
	OkCount    uint64
	Throughput float64
	P50        time.Duration
	P95        time.Duration
}

// Snapshot returns one row per (namespace, version, method) that has
// recorded observations within the window. window must be one of:
// time.Minute, time.Hour, 24*time.Hour. Result is sorted by
// namespace/version/method for stable UI output.
func (g *Gateway) Snapshot(window time.Duration, now time.Time) []MethodStatsSnapshot {
	if g.stats == nil {
		return nil
	}
	g.stats.mu.RLock()
	keys := make([]statsKey, 0, len(g.stats.methods))
	for k := range g.stats.methods {
		keys = append(keys, k)
	}
	g.stats.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		if a.version != b.version {
			return a.version < b.version
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.caller < b.caller
	})
	out := make([]MethodStatsSnapshot, 0, len(keys))
	for _, k := range keys {
		snap := g.stats.snapshot(k, window, now)
		if snap.Count == 0 {
			continue
		}
		out = append(out, MethodStatsSnapshot{
			Namespace:     k.namespace,
			Version:       k.version,
			Method:        k.method,
			Caller:        k.caller,
			StatsSnapshot: snap,
		})
	}
	return out
}
