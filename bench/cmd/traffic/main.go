// Traffic generator for the gateway bench stack.
//
// Hammers one or more GraphQL endpoints at a target rate. The CLI
// summary covers two views:
//
//   - Client-side: count, error categories, p50/p95/p99 latency per
//     target endpoint. Captures network + gateway + backend cost.
//
//   - Server-side: per-(namespace, version, method) RPS, p50/p95/p99,
//     and per-code counts pulled from the gateway's /api/metrics
//     before+after the run. Reflects what the gateway itself observed
//     about each upstream backend — how to read "infrastructure cost"
//     by namespace.
//
// Errors are categorized rather than rolled into one bucket so it's
// obvious whether failures came from the network, the gateway, or
// the GraphQL response envelope. The first few error messages of
// each kind are kept and printed so you can see what's actually
// breaking.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// errCategory groups errors so the summary explains *what* failed
// rather than just how many. Drops are concurrency saturation;
// transport are TCP/timeout failures; httpStatus is 4xx/5xx; graphql
// is a 200 response with non-empty `errors` array.
type errCategory string

const (
	errDrop      errCategory = "drop"
	errTransport errCategory = "transport"
	errHTTP      errCategory = "http"
	errGraphQL   errCategory = "graphql"
)

const sampleErrMessages = 3

type targetStats struct {
	count uint64 // successful 200 responses with no GraphQL errors
	errs  map[errCategory]uint64
	codes map[int]uint64 // HTTP status code distribution

	mu        sync.Mutex
	latencies []time.Duration
	samples   map[errCategory][]string // up to sampleErrMessages each
}

func newTargetStats() *targetStats {
	return &targetStats{
		errs:    map[errCategory]uint64{},
		codes:   map[int]uint64{},
		samples: map[errCategory][]string{},
	}
}

func (s *targetStats) recordErr(cat errCategory, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[cat]++
	if len(s.samples[cat]) < sampleErrMessages {
		s.samples[cat] = append(s.samples[cat], msg)
	}
}

func (s *targetStats) totalErrs() uint64 {
	var n uint64
	for _, v := range s.errs {
		n += v
	}
	return n
}

func main() {
	var (
		rps         = flag.Int("rps", 100, "requests per second per target")
		duration    = flag.Duration("duration", 30*time.Second, "test duration")
		concurrency = flag.Int("concurrency", 16, "max concurrent in-flight per target (extras are dropped)")
		query       = flag.String("query", `{ greeter { hello(name: "world") { greeting } } }`, "GraphQL query string")
		timeout     = flag.Duration("timeout", 5*time.Second, "per-request HTTP timeout")
		serverSide  = flag.Bool("server-metrics", true, "snapshot gateway /api/metrics before+after for the per-backend table")
	)
	var targetsRaw stringFlag
	flag.Var(&targetsRaw, "target", "GraphQL endpoint URL (repeat or comma-separate for multiple)")
	flag.Parse()

	targets := splitTargets(targetsRaw)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "at least one --target is required")
		os.Exit(2)
	}

	body, err := json.Marshal(map[string]any{"query": *query})
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal query: %v\n", err)
		os.Exit(2)
	}

	// Snapshot server-side metrics before the run.
	var preSnap map[string]*dto.MetricFamily
	metricsClient := &http.Client{Timeout: 5 * time.Second}
	if *serverSide {
		preSnap = collectMetrics(metricsClient, targets)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tctx, tcancel := context.WithTimeout(ctx, *duration)
	defer tcancel()

	stats := make([]*targetStats, len(targets))
	for i := range stats {
		stats[i] = newTargetStats()
	}

	fmt.Printf("running %d req/s for %s against %d target(s)\n", *rps, duration.String(), len(targets))
	runStart := time.Now()

	wg := sync.WaitGroup{}
	for ti, target := range targets {
		ti := ti
		target := target
		// Default http.Transport sets MaxIdleConnsPerHost=2 — at any
		// real load, requests beyond that pair burn fresh TCP
		// connections, pile up in TIME_WAIT, and after a few seconds
		// surface as "connect: cannot assign requested address" when
		// the local ephemeral-port range is exhausted. Size the pool
		// to match concurrency so keep-alive actually works.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = *concurrency * 4
		tr.MaxIdleConnsPerHost = *concurrency * 4
		tr.IdleConnTimeout = 90 * time.Second
		client := &http.Client{Timeout: *timeout, Transport: tr}
		sem := make(chan struct{}, *concurrency)
		ticker := time.NewTicker(time.Second / time.Duration(*rps))
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ticker.Stop()
			for {
				select {
				case <-tctx.Done():
					return
				case <-ticker.C:
					select {
					case sem <- struct{}{}:
						go func() {
							defer func() { <-sem }()
							fireOnce(tctx, client, target, body, stats[ti])
						}()
					default:
						stats[ti].recordErr(errDrop, "concurrency saturated; --concurrency too low or server too slow")
					}
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(runStart)

	// Snapshot server-side metrics after the run.
	var postSnap map[string]*dto.MetricFamily
	if *serverSide {
		postSnap = collectMetrics(metricsClient, targets)
	}

	printClientSummary(targets, stats, elapsed)
	if *serverSide {
		printServerSummary(preSnap, postSnap, elapsed)
	}
}

func fireOnce(ctx context.Context, client *http.Client, target string, body []byte, s *targetStats) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if err != nil {
		s.recordErr(errTransport, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		// Ignore the test-clock cancel: tcancel() at end-of-duration
		// will fail any in-flight request with this error, and it's
		// indistinguishable from "real" timeouts at the syscall level.
		// We simply drop the sample. Real backend timeouts manifest
		// at the (server-side) gateway metrics regardless.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		s.recordErr(errTransport, err.Error())
		return
	}
	defer resp.Body.Close()

	s.mu.Lock()
	s.codes[resp.StatusCode]++
	s.mu.Unlock()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		excerpt := truncate(string(respBody), 200)
		s.recordErr(errHTTP, fmt.Sprintf("status=%d body=%s", resp.StatusCode, excerpt))
		return
	}
	// 200 — but might still carry a GraphQL error envelope. Inspect.
	var env struct {
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &env); err == nil && len(env.Errors) > 0 {
		first := env.Errors[0]
		code := ""
		if c, ok := first.Extensions["code"].(string); ok {
			code = " code=" + c
		}
		s.recordErr(errGraphQL, fmt.Sprintf("%s%s", first.Message, code))
		return
	}
	atomic.AddUint64(&s.count, 1)
	s.mu.Lock()
	s.latencies = append(s.latencies, elapsed)
	s.mu.Unlock()
}

func printClientSummary(targets []string, stats []*targetStats, elapsed time.Duration) {
	fmt.Println()
	fmt.Printf("=== client-side (over %s) ===\n", elapsed.Round(time.Millisecond))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TARGET\tRPS\tP50\tP95\tP99\tOK\tERRS\tCODES")
	for ti, t := range targets {
		s := stats[ti]
		count := atomic.LoadUint64(&s.count)
		errs := s.totalErrs()
		s.mu.Lock()
		ls := append([]time.Duration(nil), s.latencies...)
		codeSnapshot := copyIntCounts(s.codes)
		s.mu.Unlock()
		var p50s, p95s, p99s string
		if len(ls) == 0 {
			p50s, p95s, p99s = "-", "-", "-"
		} else {
			sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
			p50s = fmtSeconds(pct(ls, 0.5))
			p95s = fmtSeconds(pct(ls, 0.95))
			p99s = fmtSeconds(pct(ls, 0.99))
		}
		rps := float64(count+errs) / elapsed.Seconds()
		fmt.Fprintf(tw, "  %s\t%.1f\t%s\t%s\t%s\t%d\t%d\t%s\n",
			t, rps, p50s, p95s, p99s, count, errs, formatCodeMap(codeSnapshot))
		_ = ti
	}
	tw.Flush()

	// Per-category errors with sample messages live below the table —
	// only the targets that actually saw errors print, so the happy
	// path stays a single line per target.
	for ti, t := range targets {
		s := stats[ti]
		errs := s.totalErrs()
		if errs == 0 {
			continue
		}
		fmt.Printf("\n  %s — error breakdown:\n", t)
		for _, cat := range []errCategory{errDrop, errTransport, errHTTP, errGraphQL} {
			n := s.errs[cat]
			if n == 0 {
				continue
			}
			fmt.Printf("    %-9s %d\n", cat, n)
			for _, msg := range s.samples[cat] {
				fmt.Printf("      sample: %s\n", msg)
			}
		}
		_ = ti
	}
}

// copyIntCounts is a defensive copy so the caller can inspect the
// codes map without keeping the stats lock held while it formats.
func copyIntCounts(src map[int]uint64) map[int]uint64 {
	out := make(map[int]uint64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// printServerSummary diffs the gateway-side dispatch metrics to show
// per-backend RPS, latency quantiles, and code mix. Latency comes
// from the histogram buckets; RPS from the count diff over elapsed
// wall time.
func printServerSummary(pre, post map[string]*dto.MetricFamily, elapsed time.Duration) {
	fmt.Println()
	fmt.Printf("=== gateway-side metrics (delta over %s) ===\n", elapsed.Round(time.Millisecond))
	if pre == nil || post == nil {
		fmt.Println("  no metric snapshots — gateway /api/metrics not reachable.")
		return
	}
	type backendKey struct{ namespace, version, method string }
	type backendAgg struct {
		count   uint64
		buckets map[float64]uint64 // upper-bound → cumulative count delta
		sum     float64
		codes   []codeAggLite
	}
	agg := map[backendKey]*backendAgg{}
	totalByCode := map[string]uint64{}

	postFam := post["go_api_gateway_dispatch_duration_seconds"]
	if postFam == nil {
		fmt.Println("  go_api_gateway_dispatch_duration_seconds not present in snapshot.")
		return
	}
	preFam := pre["go_api_gateway_dispatch_duration_seconds"]

	preIndex := map[string]*dto.Metric{}
	if preFam != nil {
		for _, m := range preFam.Metric {
			preIndex[labelKey(m)] = m
		}
	}

	for _, m := range postFam.Metric {
		var ns, ver, method, code string
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "namespace":
				ns = l.GetValue()
			case "version":
				ver = l.GetValue()
			case "method":
				method = l.GetValue()
			case "code":
				code = l.GetValue()
			}
		}
		key := backendKey{ns, ver, method}
		k := labelKey(m)
		var prev *dto.Metric
		if p, ok := preIndex[k]; ok {
			prev = p
		}
		hist := m.GetHistogram()
		if hist == nil {
			continue
		}
		dCount := hist.GetSampleCount()
		dSum := hist.GetSampleSum()
		if prev != nil && prev.GetHistogram() != nil {
			dCount -= prev.GetHistogram().GetSampleCount()
			dSum -= prev.GetHistogram().GetSampleSum()
		}
		if dCount == 0 {
			continue
		}
		a, ok := agg[key]
		if !ok {
			a = &backendAgg{buckets: map[float64]uint64{}}
			agg[key] = a
		}
		a.count += dCount
		a.sum += dSum
		a.codes = append(a.codes, codeAggLite{code: code, count: dCount})
		totalByCode[code] += dCount
		// Bucket diffs: each post bucket minus the matching pre bucket
		// (matched on upper-bound). graphql-go's histogram emits the
		// same buckets every scrape so the match is by float key.
		preBuckets := map[float64]uint64{}
		if prev != nil && prev.GetHistogram() != nil {
			for _, b := range prev.GetHistogram().GetBucket() {
				preBuckets[b.GetUpperBound()] = b.GetCumulativeCount()
			}
		}
		for _, b := range hist.GetBucket() {
			ub := b.GetUpperBound()
			delta := b.GetCumulativeCount() - preBuckets[ub]
			a.buckets[ub] += delta
		}
	}

	// Render a (namespace, version, method) table.
	keys := make([]backendKey, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		if keys[i].version != keys[j].version {
			return keys[i].version < keys[j].version
		}
		return keys[i].method < keys[j].method
	})
	if len(keys) == 0 {
		fmt.Println("  no dispatches observed during this run.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tVERSION\tMETHOD\tRPS\tP50\tP95\tP99\tCOUNT\tCODES")
	for _, k := range keys {
		a := agg[k]
		rps := float64(a.count) / elapsed.Seconds()
		p50 := histogramQuantile(0.5, a.buckets, a.count)
		p95 := histogramQuantile(0.95, a.buckets, a.count)
		p99 := histogramQuantile(0.99, a.buckets, a.count)
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%d\t%s\n",
			k.namespace, k.version, k.method, rps,
			fmtSeconds(p50), fmtSeconds(p95), fmtSeconds(p99),
			a.count, formatCodeAggs(a.codes))
	}
	tw.Flush()

	// Cross-namespace code mix — useful at a glance for "did anything
	// turn red in any backend".
	fmt.Println("  overall codes:")
	codes := make([]string, 0, len(totalByCode))
	for c := range totalByCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	var grand uint64
	for _, c := range codes {
		grand += totalByCode[c]
	}
	for _, c := range codes {
		n := totalByCode[c]
		pctv := 0.0
		if grand > 0 {
			pctv = float64(n) / float64(grand) * 100
		}
		fmt.Printf("    %-30s %d (%.2f%%)\n", c, n, pctv)
	}
}

// collectMetrics fetches /api/metrics from each unique gateway in
// the target list and merges into one map keyed by metric family
// name. Multiple gateway snapshots' metrics combine because each
// label set is unique per (namespace, version, method, code) — when
// two gateways serve the same dispatch, prom merges their counts via
// summation in queries; here we already merge by re-using the latest
// value per label set, which is wrong for cross-cluster aggregation.
// For the bench's single-host case this gives a sensible per-cluster
// view because the JetStream cluster KV makes every gateway see the
// same dispatch totals (every gateway dispatches every request to its
// pool's replicas; the counters are local).
//
// TODO when the bench grows: sum same-label families across gateway
// snapshots so multi-gateway clusters report the union, not the last.
func collectMetrics(client *http.Client, targets []string) map[string]*dto.MetricFamily {
	seen := map[string]bool{}
	out := map[string]*dto.MetricFamily{}
	for _, t := range targets {
		mu := metricsURLFromTarget(t)
		if mu == "" || seen[mu] {
			continue
		}
		seen[mu] = true
		fam, err := fetchMetrics(client, mu)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: fetching %s: %v\n", mu, err)
			continue
		}
		for name, f := range fam {
			out[name] = mergeFamilies(out[name], f)
		}
	}
	return out
}

func metricsURLFromTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	u.Path = "/api/metrics"
	u.RawQuery = ""
	return u.String()
}

func fetchMetrics(client *http.Client, u string) (map[string]*dto.MetricFamily, error) {
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// NewTextParser explicitly — the zero-value TextParser carries
	// UnsetValidation which panics inside parsing. UTF8Validation is
	// the modern default and accepts everything the gateway emits.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(resp.Body)
}

// mergeFamilies adds src's metrics to dst. Same-label series are
// summed (counters) or kept (gauges — last-wins is fine for snapshot
// diffs since we use per-snapshot deltas). For the histogram family
// we sum SampleCount, SampleSum, and per-bucket cumulative counts.
func mergeFamilies(dst, src *dto.MetricFamily) *dto.MetricFamily {
	if dst == nil {
		return src
	}
	idx := map[string]*dto.Metric{}
	for _, m := range dst.Metric {
		idx[labelKey(m)] = m
	}
	for _, sm := range src.Metric {
		k := labelKey(sm)
		if dm, ok := idx[k]; ok {
			mergeMetric(dm, sm)
		} else {
			dst.Metric = append(dst.Metric, sm)
		}
	}
	return dst
}

func mergeMetric(dst, src *dto.Metric) {
	if src.Counter != nil && dst.Counter != nil {
		v := dst.Counter.GetValue() + src.Counter.GetValue()
		dst.Counter.Value = &v
	}
	if src.Histogram != nil && dst.Histogram != nil {
		dc := dst.Histogram.GetSampleCount() + src.Histogram.GetSampleCount()
		ds := dst.Histogram.GetSampleSum() + src.Histogram.GetSampleSum()
		dst.Histogram.SampleCount = &dc
		dst.Histogram.SampleSum = &ds
		bidx := map[float64]*dto.Bucket{}
		for _, b := range dst.Histogram.Bucket {
			bidx[b.GetUpperBound()] = b
		}
		for _, sb := range src.Histogram.Bucket {
			if db, ok := bidx[sb.GetUpperBound()]; ok {
				v := db.GetCumulativeCount() + sb.GetCumulativeCount()
				db.CumulativeCount = &v
			}
		}
	}
}

func labelKey(m *dto.Metric) string {
	pairs := make([]string, 0, len(m.Label))
	for _, l := range m.Label {
		pairs = append(pairs, l.GetName()+"="+l.GetValue())
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// histogramQuantile computes the q-th quantile from a Prometheus-shape
// histogram by linear interpolation within the bucket containing the
// target rank. Mirrors graphql-go-promhttp's `histogram_quantile()`.
// `buckets` maps upper-bound → cumulative count delta (already
// snapshot-differenced); `total` is sum across all buckets at +Inf.
func histogramQuantile(q float64, buckets map[float64]uint64, total uint64) time.Duration {
	if total == 0 || len(buckets) == 0 {
		return 0
	}
	uppers := make([]float64, 0, len(buckets))
	for ub := range buckets {
		uppers = append(uppers, ub)
	}
	sort.Float64s(uppers)
	target := q * float64(total)
	var prevUB, prevCount float64
	for _, ub := range uppers {
		count := float64(buckets[ub])
		if count >= target {
			if math.IsInf(ub, 1) {
				return time.Duration(prevUB * float64(time.Second))
			}
			// Linear interpolation inside the bucket.
			if count == prevCount {
				return time.Duration(ub * float64(time.Second))
			}
			frac := (target - prevCount) / (count - prevCount)
			est := prevUB + (ub-prevUB)*frac
			return time.Duration(est * float64(time.Second))
		}
		prevUB, prevCount = ub, count
	}
	return time.Duration(prevUB * float64(time.Second))
}

func fmtSeconds(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	default:
		return d.Round(time.Millisecond).String()
	}
}

func formatCodeMap(m map[int]uint64) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func formatCodeAggs(codes []codeAggLite) string {
	if len(codes) == 0 {
		return "-"
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].count > codes[j].count })
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%s=%d", c.code, c.count))
	}
	return strings.Join(parts, " ")
}

// codeAggLite is the public shape for formatCodeAggs; the internal
// printServerSummary uses its own codeAgg defined inline. Kept as
// two so the formatter doesn't have to reach into a package-private
// closure.
type codeAggLite = struct {
	code  string
	count uint64
}

func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * q)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func errPct(total, errs uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(errs) / float64(total) * 100
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stringFlag is a flag.Value collecting repeated --target entries.
type stringFlag []string

func (s *stringFlag) String() string { return strings.Join(*s, ",") }
func (s *stringFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// splitTargets accepts a list of --target args, each of which may
// itself contain a comma-separated list. The flat result is the
// union; empties are dropped.
func splitTargets(raw stringFlag) []string {
	var out []string
	for _, r := range raw {
		for _, p := range strings.Split(r, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
