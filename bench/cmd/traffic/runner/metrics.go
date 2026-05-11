package runner

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// metricFamily aliases the dto type so the dependency stays in this
// file; the rest of the package treats it opaquely.
type metricFamily = dto.MetricFamily

var metricsClient = &http.Client{Timeout: 5 * time.Second}

// collectMetrics fetches /api/metrics from each unique gateway in
// the target list and merges into one map keyed by metric family
// name. Same-label series are summed across snapshots so multi-
// gateway clusters report the union.
func collectMetrics(targets []Target) map[string]*metricFamily {
	seen := map[string]bool{}
	out := map[string]*metricFamily{}
	for _, t := range targets {
		mu := t.MetricsURL
		if mu == "" || seen[mu] {
			continue
		}
		seen[mu] = true
		fam, err := fetchMetrics(metricsClient, mu)
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

func fetchMetrics(client *http.Client, u string) (map[string]*metricFamily, error) {
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(resp.Body)
}

func mergeFamilies(dst, src *metricFamily) *metricFamily {
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
	return joinStr(pairs, ",")
}

// printServerSummary diffs the gateway-side dispatch metrics to show
// per-backend RPS, latency quantiles, and code mix.
func printServerSummary(pre, post map[string]*metricFamily, elapsed time.Duration) {
	fmt.Println()
	fmt.Printf("=== gateway-side metrics (delta over %s) ===\n", elapsed.Round(time.Millisecond))
	if pre == nil || post == nil {
		fmt.Println("  no metric snapshots — gateway /api/metrics not reachable.")
		return
	}
	if post["go_api_gateway_dispatch_duration_seconds"] == nil {
		fmt.Println("  go_api_gateway_dispatch_duration_seconds not present in snapshot.")
		return
	}
	rows := AggregateDispatches(pre, post)
	if len(rows) == 0 {
		fmt.Println("  no dispatches observed during this run.")
		return
	}
	totalByCode := map[string]uint64{}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAMESPACE\tVERSION\tMETHOD\tRPS\tP50\tP95\tP99\tCOUNT\tCODES")
	for _, a := range rows {
		rps := float64(a.Count) / elapsed.Seconds()
		p50 := histogramQuantile(0.5, a.Buckets, a.Count)
		p95 := histogramQuantile(0.95, a.Buckets, a.Count)
		p99 := histogramQuantile(0.99, a.Buckets, a.Count)
		for _, c := range a.Codes {
			totalByCode[c.Code] += c.Count
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%d\t%s\n",
			a.Namespace, a.Version, a.Method, rps,
			fmtSeconds(p50), fmtSeconds(p95), fmtSeconds(p99),
			a.Count, formatCodeAggs(toCodeAggs(a.Codes)))
	}
	tw.Flush()

	printIngressTimeRows(pre, post)

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

func toCodeAggs(src []CodeCount) []codeAgg {
	out := make([]codeAgg, len(src))
	for i, c := range src {
		out[i] = codeAgg{code: c.Code, count: c.Count}
	}
	return out
}

// printIngressTimeRows renders per-ingress total + self-time (mean +
// p95) from request_duration_seconds and request_self_seconds. Self
// is the gateway's own slice (total minus the per-request dispatch
// accumulator); pair lets operators compare "what we spent" vs "what
// upstream spent" without writing PromQL.
func printIngressTimeRows(pre, post map[string]*metricFamily) {
	rows := AggregateIngress(pre, post)
	if len(rows) == 0 {
		return
	}
	fmt.Println("  per-ingress request time:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    INGRESS\tTOTAL_MEAN\tTOTAL_P95\tSELF_MEAN\tSELF_P95\tCOUNT")
	for _, a := range rows {
		totMean := MeanDuration(a.TotalSumSec, a.TotalCount)
		selfMean := MeanDuration(a.SelfSumSec, a.SelfCount)
		var totP95, selfP95 time.Duration
		if a.TotalCount > 0 {
			totP95 = histogramQuantile(0.95, a.TotalBuckets, a.TotalCount)
		}
		if a.SelfCount > 0 {
			selfP95 = histogramQuantile(0.95, a.SelfBuckets, a.SelfCount)
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\t%d\n",
			a.Ingress, fmtSeconds(totMean), fmtSeconds(totP95),
			fmtSeconds(selfMean), fmtSeconds(selfP95),
			a.TotalCount)
	}
	tw.Flush()
}

type codeAgg struct {
	code  string
	count uint64
}

func formatCodeAggs(codes []codeAgg) string {
	if len(codes) == 0 {
		return "-"
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].count > codes[j].count })
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%s=%d", c.code, c.count))
	}
	return joinStr(parts, " ")
}
