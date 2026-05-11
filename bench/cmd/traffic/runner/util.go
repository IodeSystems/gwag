package runner

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

// joinStr is a tiny shim so files in the package don't have to import
// strings just for Join. (strings is fine, but keeping the surface
// of imports flat in printers makes them easier to scan.)
func joinStr(parts []string, sep string) string {
	return strings.Join(parts, sep)
}

// histogramQuantile computes the q-th quantile from a Prometheus-shape
// histogram by linear interpolation within the bucket containing the
// target rank. Mirrors prometheus's `histogram_quantile()`. `buckets`
// maps upper-bound → cumulative count delta (snapshot-differenced);
// `total` is the sum across all buckets at +Inf.
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

// Truncate collapses runs of whitespace and clips to n runes.
// Adapters call this before RecordBody so the summary table doesn't
// eat a screen on a pretty-printed JSON response.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// autoConcurrency is the runner's default when an adapter passes
// Concurrency=0: max(64, rps/20). The 5%-of-RPS floor scales with
// load so a 30k-rps run doesn't cap on the same 64 in-flight slots
// that suit a 1k-rps smoke test.
func autoConcurrency(rps int) int {
	if c := rps / 20; c > 64 {
		return c
	}
	return 64
}

// autoShardCount is the runner's default when an adapter passes
// Shards=0: ceil(rps/1500). A single Go time.Ticker + goroutine-
// spawn loop empirically caps at ~3.2k Hz on Linux (scheduler +
// runtime-timer granularity, not CPU), so anything above ~3k RPS
// has to be sharded across N driver goroutines or every reported
// "achieved RPS" is bench-client-bound, not target-bound. 1500 RPS
// per shard leaves 2× headroom under that empirical cap.
func autoShardCount(rps int) int {
	if rps <= 1500 {
		return 1
	}
	n := (rps + 1499) / 1500 // ceil(rps/1500)
	return n
}

// MetricsURLFromGateway derives /api/metrics from any gateway URL
// (HTTP base, /api/graphql, /api/ingress/foo, etc.). Adapters call
// this when constructing Targets so the runner can snapshot
// gateway-side dispatch counters.
func MetricsURLFromGateway(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	u.Path = "/api/metrics"
	u.RawQuery = ""
	return u.String()
}

// SplitCSV accepts a list of args, each of which may itself be a
// comma-separated list, and returns the flat union with empties
// dropped. Used to handle repeated/CSV --target flags.
func SplitCSV(raw []string) []string {
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

// StringFlag is a flag.Value collecting repeated args (e.g. --target).
type StringFlag []string

func (s *StringFlag) String() string     { return strings.Join(*s, ",") }
func (s *StringFlag) Set(v string) error { *s = append(*s, v); return nil }
