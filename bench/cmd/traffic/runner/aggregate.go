package runner

import (
	"sort"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// DispatchAggregate is the pre/post-diffed view of one (namespace,
// version, method) row from go_api_gateway_dispatch_duration_seconds.
// Buckets is the histogram remainder after differencing; Sum is
// seconds, Count is samples. Codes carries one entry per code label
// observed (OK / FAILED_PRECONDITION / etc.).
type DispatchAggregate struct {
	Namespace string
	Version   string
	Method    string
	Count     uint64
	SumSec    float64
	Buckets   map[float64]uint64
	Codes     []CodeCount
}

// CodeCount is one (code, samples) pair attached to a dispatch row.
type CodeCount struct {
	Code  string
	Count uint64
}

// IngressAggregate is the pre/post-diffed view of one ingress slice
// from request_duration_seconds + request_self_seconds. Total* is
// the full request, Self* is the gateway-only slice (total minus
// per-request dispatch accumulator).
type IngressAggregate struct {
	Ingress      string
	TotalCount   uint64
	TotalSumSec  float64
	TotalBuckets map[float64]uint64
	SelfCount    uint64
	SelfSumSec   float64
	SelfBuckets  map[float64]uint64
}

// AggregateDispatches diffs go_api_gateway_dispatch_duration_seconds
// across pre/post snapshots, grouping by (namespace, version, method).
// Rows with zero delta are dropped. Returned slice is sorted by
// (namespace, version, method) so the order is deterministic for
// both the printer and the JSON encoder.
func AggregateDispatches(pre, post map[string]*metricFamily) []DispatchAggregate {
	if post == nil {
		return nil
	}
	postFam := post["go_api_gateway_dispatch_duration_seconds"]
	if postFam == nil {
		return nil
	}
	preFam := preOf(pre, "go_api_gateway_dispatch_duration_seconds")
	preIndex := indexByLabels(preFam)
	type key struct{ ns, ver, method string }
	agg := map[key]*DispatchAggregate{}
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
		hist := m.GetHistogram()
		if hist == nil {
			continue
		}
		prev := preIndex[labelKey(m)]
		dCount, dSum, dBuckets := diffHistogram(prev, hist)
		if dCount == 0 {
			continue
		}
		k := key{ns, ver, method}
		a, ok := agg[k]
		if !ok {
			a = &DispatchAggregate{
				Namespace: ns,
				Version:   ver,
				Method:    method,
				Buckets:   map[float64]uint64{},
			}
			agg[k] = a
		}
		a.Count += dCount
		a.SumSec += dSum
		a.Codes = append(a.Codes, CodeCount{Code: code, Count: dCount})
		for ub, n := range dBuckets {
			a.Buckets[ub] += n
		}
	}
	out := make([]DispatchAggregate, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// AggregateIngress diffs request_duration_seconds + request_self_seconds
// across pre/post snapshots, grouping by `ingress` label. Returned slice
// is sorted by ingress so callers (printer / JSON encoder) get
// deterministic order.
func AggregateIngress(pre, post map[string]*metricFamily) []IngressAggregate {
	if post == nil {
		return nil
	}
	agg := map[string]*IngressAggregate{}
	apply := func(famName string, isSelf bool) {
		postFam := post[famName]
		if postFam == nil {
			return
		}
		preFam := preOf(pre, famName)
		preIndex := indexByLabels(preFam)
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
			dCount, dSum, dBuckets := diffHistogram(prev, hist)
			if dCount == 0 {
				continue
			}
			a, ok := agg[ingress]
			if !ok {
				a = &IngressAggregate{
					Ingress:      ingress,
					TotalBuckets: map[float64]uint64{},
					SelfBuckets:  map[float64]uint64{},
				}
				agg[ingress] = a
			}
			if isSelf {
				a.SelfCount += dCount
				a.SelfSumSec += dSum
				for ub, n := range dBuckets {
					a.SelfBuckets[ub] += n
				}
			} else {
				a.TotalCount += dCount
				a.TotalSumSec += dSum
				for ub, n := range dBuckets {
					a.TotalBuckets[ub] += n
				}
			}
		}
	}
	apply("go_api_gateway_request_duration_seconds", false)
	apply("go_api_gateway_request_self_seconds", true)
	out := make([]IngressAggregate, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ingress < out[j].Ingress })
	return out
}

// preOf returns the same-named family from `pre` or nil — used so
// callers don't have to nil-check `pre` everywhere.
func preOf(pre map[string]*metricFamily, name string) *metricFamily {
	if pre == nil {
		return nil
	}
	return pre[name]
}

// indexByLabels keys the family's metrics by their labelKey so the
// post-side walk can look up the matching pre-side row in O(1).
// Returns an empty map for a nil family so callers don't branch.
func indexByLabels(fam *metricFamily) map[string]*dto.Metric {
	idx := map[string]*dto.Metric{}
	if fam == nil {
		return idx
	}
	for _, m := range fam.Metric {
		idx[labelKey(m)] = m
	}
	return idx
}

// diffHistogram subtracts pre from a post histogram, returning the
// delta count / sum / per-bucket counts. Handles nil pre (treat as
// zero across the board) so cold-start runs without a pre-snapshot
// still produce useful numbers.
func diffHistogram(prev *dto.Metric, post *dto.Histogram) (count uint64, sumSec float64, buckets map[float64]uint64) {
	count = post.GetSampleCount()
	sumSec = post.GetSampleSum()
	buckets = map[float64]uint64{}
	var preBuckets map[float64]uint64
	if prev != nil && prev.GetHistogram() != nil {
		count -= prev.GetHistogram().GetSampleCount()
		sumSec -= prev.GetHistogram().GetSampleSum()
		preBuckets = map[float64]uint64{}
		for _, b := range prev.GetHistogram().GetBucket() {
			preBuckets[b.GetUpperBound()] = b.GetCumulativeCount()
		}
	}
	for _, b := range post.GetBucket() {
		buckets[b.GetUpperBound()] = b.GetCumulativeCount() - preBuckets[b.GetUpperBound()]
	}
	return
}

// MeanDuration returns sumSec/count as time.Duration; zero on zero
// count to avoid div-by-zero.
func MeanDuration(sumSec float64, count uint64) time.Duration {
	if count == 0 {
		return 0
	}
	return time.Duration(sumSec / float64(count) * float64(time.Second))
}
