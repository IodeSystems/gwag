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
	snap.P99 = histPercentile(hist[:], 0.99)
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

// HistoryBucket is one ring-bucket: a fixed-width slice of time with
// its dispatch count, ok-count, and percentile estimates. The public
// status page uses these to render a strip of dots per service —
// width = window-bucket-duration, color = error ratio. Buckets are
// emitted oldest-first; gaps in the ring (no observations yet) are
// surfaced as zero-count entries pinned to their wall-clock start so
// the UI can render a continuous timeline.
type HistoryBucket struct {
	StartUnixSec int64
	DurationSec  int64
	Count        uint64
	OkCount      uint64
	P50          time.Duration
	P95          time.Duration
	P99          time.Duration
}

// historyBuckets returns the ring's buckets covering the window
// ending at nowUnix, oldest-first. Stale buckets (whose start fell
// outside the window) are zeroed and re-pinned to the wall-clock slot
// they would belong to, so the caller always gets a fixed-length
// timeline regardless of how recently the ring saw traffic.
func (w *statsWindow) historyBuckets(nowUnix int64) []HistoryBucket {
	out := make([]HistoryBucket, len(w.buckets))
	bucketStart := (nowUnix / w.bucketDurSec) * w.bucketDurSec
	// The current bucket is the youngest; walk backward in wall time
	// so out[0] is the oldest.
	for i := 0; i < len(w.buckets); i++ {
		// Time slot i counts back from the head: head - (n-1-i) buckets.
		offset := int64(len(w.buckets)-1-i) * w.bucketDurSec
		slotStart := bucketStart - offset
		idx := int((slotStart / w.bucketDurSec)) % len(w.buckets)
		if idx < 0 {
			idx += len(w.buckets)
		}
		b := &w.buckets[idx]
		hb := HistoryBucket{
			StartUnixSec: slotStart,
			DurationSec:  w.bucketDurSec,
		}
		// Stale → bucket belongs to an older ring wrap; emit zero.
		if b.bucketStartUnix == slotStart {
			hb.Count = b.count
			hb.OkCount = b.okCount
			hb.P50 = histPercentile(b.latency[:], 0.50)
			hb.P95 = histPercentile(b.latency[:], 0.95)
			hb.P99 = histPercentile(b.latency[:], 0.99)
		}
		out[i] = hb
	}
	return out
}

// History returns one row per (namespace, version) over the chosen
// window, with bucket-level granularity. method+caller dimensions
// collapse into the (ns, ver) total — the public status page is
// service-level, not method-level. window must be one of:
// time.Minute, time.Hour, 24*time.Hour.
//
// Bucket widths track the underlying ring (1s / 1m / 10m
// respectively); the UI aggregates dots if it wants a different slice
// width.
func (g *Gateway) History(window time.Duration, now time.Time) []ServiceHistory {
	if g.stats == nil {
		return nil
	}
	g.stats.mu.RLock()
	keys := make([]statsKey, 0, len(g.stats.methods))
	type ring struct{ buckets []HistoryBucket }
	merged := map[serviceKey]*ring{}
	for k, m := range g.stats.methods {
		keys = append(keys, k)
		sk := serviceKey{namespace: k.namespace, version: k.version}
		m.mu.Lock()
		w := m.windowFor(window)
		if w == nil {
			m.mu.Unlock()
			continue
		}
		row := w.historyBuckets(now.Unix())
		m.mu.Unlock()
		dst, ok := merged[sk]
		if !ok {
			dst = &ring{buckets: make([]HistoryBucket, len(row))}
			for i, b := range row {
				dst.buckets[i] = HistoryBucket{
					StartUnixSec: b.StartUnixSec,
					DurationSec:  b.DurationSec,
				}
			}
			merged[sk] = dst
		}
		for i, b := range row {
			dst.buckets[i].Count += b.Count
			dst.buckets[i].OkCount += b.OkCount
			// Bucket-level percentiles roll up by max-across-callers —
			// matches the servicesStats aggregate posture (worst case
			// is the right summary when the question is "did any
			// caller see slow?").
			if b.P50 > dst.buckets[i].P50 {
				dst.buckets[i].P50 = b.P50
			}
			if b.P95 > dst.buckets[i].P95 {
				dst.buckets[i].P95 = b.P95
			}
			if b.P99 > dst.buckets[i].P99 {
				dst.buckets[i].P99 = b.P99
			}
		}
	}
	g.stats.mu.RUnlock()
	out := make([]ServiceHistory, 0, len(merged))
	for sk, r := range merged {
		out = append(out, ServiceHistory{
			Namespace: sk.namespace,
			Version:   sk.version,
			Buckets:   r.buckets,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// serviceKey is the rollup key for History — drops method + caller so
// the public status surface is one row per registered (ns, ver).
type serviceKey struct{ namespace, version string }

// ServiceHistory is one row of the public status page: a time-series
// of HistoryBuckets covering the window. The dot-strip UI renders one
// dot per bucket, colored by error ratio (Count - OkCount) / Count.
type ServiceHistory struct {
	Namespace string
	Version   string
	Buckets   []HistoryBucket
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
	P99        time.Duration
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
