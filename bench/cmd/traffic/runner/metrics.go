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
	type backendKey struct{ namespace, version, method string }
	type backendAgg struct {
		count   uint64
		buckets map[float64]uint64
		sum     float64
		codes   []codeAgg
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
		a.codes = append(a.codes, codeAgg{code: code, count: dCount})
		totalByCode[code] += dCount
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

// printIngressTimeRows renders per-ingress total + self-time (mean +
// p95) from request_duration_seconds and request_self_seconds. Self
// is the gateway's own slice (total minus the per-request dispatch
// accumulator); pair lets operators compare "what we spent" vs "what
// upstream spent" without writing PromQL.
func printIngressTimeRows(pre, post map[string]*metricFamily) {
	type ingressAgg struct {
		totalCount, selfCount uint64
		totalSum, selfSum     float64
		totalBuckets          map[float64]uint64
		selfBuckets           map[float64]uint64
	}
	agg := map[string]*ingressAgg{}
	add := func(famName string, isSelf bool) {
		postFam := post[famName]
		if postFam == nil {
			return
		}
		preFam := pre[famName]
		preIndex := map[string]*dto.Metric{}
		if preFam != nil {
			for _, m := range preFam.Metric {
				preIndex[labelKey(m)] = m
			}
		}
		for _, m := range postFam.Metric {
			var ingress string
			for _, l := range m.GetLabel() {
				if l.GetName() == "ingress" {
					ingress = l.GetValue()
					break
				}
			}
			hist := m.GetHistogram()
			if hist == nil {
				continue
			}
			prev := preIndex[labelKey(m)]
			dCount := hist.GetSampleCount()
			dSum := hist.GetSampleSum()
			if prev != nil && prev.GetHistogram() != nil {
				dCount -= prev.GetHistogram().GetSampleCount()
				dSum -= prev.GetHistogram().GetSampleSum()
			}
			if dCount == 0 {
				continue
			}
			a, ok := agg[ingress]
			if !ok {
				a = &ingressAgg{
					totalBuckets: map[float64]uint64{},
					selfBuckets:  map[float64]uint64{},
				}
				agg[ingress] = a
			}
			preBuckets := map[float64]uint64{}
			if prev != nil && prev.GetHistogram() != nil {
				for _, b := range prev.GetHistogram().GetBucket() {
					preBuckets[b.GetUpperBound()] = b.GetCumulativeCount()
				}
			}
			if isSelf {
				a.selfCount += dCount
				a.selfSum += dSum
				for _, b := range hist.GetBucket() {
					a.selfBuckets[b.GetUpperBound()] += b.GetCumulativeCount() - preBuckets[b.GetUpperBound()]
				}
			} else {
				a.totalCount += dCount
				a.totalSum += dSum
				for _, b := range hist.GetBucket() {
					a.totalBuckets[b.GetUpperBound()] += b.GetCumulativeCount() - preBuckets[b.GetUpperBound()]
				}
			}
		}
	}
	add("go_api_gateway_request_duration_seconds", false)
	add("go_api_gateway_request_self_seconds", true)
	if len(agg) == 0 {
		return
	}
	keys := make([]string, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("  per-ingress request time:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    INGRESS\tTOTAL_MEAN\tTOTAL_P95\tSELF_MEAN\tSELF_P95\tCOUNT")
	for _, k := range keys {
		a := agg[k]
		var totMean, totP95, selfMean, selfP95 time.Duration
		if a.totalCount > 0 {
			totMean = time.Duration(a.totalSum / float64(a.totalCount) * float64(time.Second))
			totP95 = histogramQuantile(0.95, a.totalBuckets, a.totalCount)
		}
		if a.selfCount > 0 {
			selfMean = time.Duration(a.selfSum / float64(a.selfCount) * float64(time.Second))
			selfP95 = histogramQuantile(0.95, a.selfBuckets, a.selfCount)
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\t%d\n",
			k, fmtSeconds(totMean), fmtSeconds(totP95),
			fmtSeconds(selfMean), fmtSeconds(selfP95),
			a.totalCount)
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
