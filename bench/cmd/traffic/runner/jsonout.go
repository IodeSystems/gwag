package runner

import (
	"encoding/json"
	"io"
	"maps"
	"slices"
	"sync/atomic"
	"time"
)

// JSONSchemaVersion identifies the on-disk shape so downstream
// consumers (perf sweep driver, future CI gate) can refuse a
// snapshot they don't understand. Bump on any breaking change to
// JSONOutput; additive fields can land without a bump because Go's
// JSON decoder ignores unknown keys.
const JSONSchemaVersion = "bench-traffic/v1"

// JSONOutput is the stable wire shape `--json` emits per pass. One
// invocation of `bench traffic <kind> --json PATH` writes exactly
// one JSONOutput record (the gateway pass; --direct compare passes
// are not exported). Times are µs (int64) so the JSON is integer-
// friendly for diff tooling and grafana.
type JSONOutput struct {
	Schema      string        `json:"schema"`
	Label       string        `json:"label,omitempty"`
	TargetRPS   int           `json:"target_rps"`
	Concurrency int           `json:"concurrency"`
	Duration    float64       `json:"duration_seconds"`
	Targets     []TargetStats `json:"targets"`
	Gateway     *GatewayStats `json:"gateway,omitempty"`
}

// TargetStats is one row of per-target client-side outcomes. Single-
// target runs (the common case for the sweep driver) emit one entry;
// multi-target runs emit one per target so consumers can drill in.
type TargetStats struct {
	Label       string            `json:"label"`
	MetricsURL  string            `json:"metrics_url,omitempty"`
	OK          uint64            `json:"ok"`
	Errs        uint64            `json:"errs"`
	AchievedRPS float64           `json:"achieved_rps"`
	MeanUs      int64             `json:"mean_us"`
	P50Us       int64             `json:"p50_us"`
	P95Us       int64             `json:"p95_us"`
	P99Us       int64             `json:"p99_us"`
	Codes       map[string]uint64 `json:"codes,omitempty"`
	Errors      map[string]uint64 `json:"errors,omitempty"`
}

// GatewayStats is the gateway-side snapshot diff, populated when
// ServerMetrics is enabled and /api/metrics was reachable on both
// pre- and post-snapshots.
type GatewayStats struct {
	Dispatches []DispatchStats          `json:"dispatches,omitempty"`
	Ingress    map[string]IngressStats `json:"ingress,omitempty"`
}

// DispatchStats is one per-backend row from
// go_api_gateway_dispatch_duration_seconds.
type DispatchStats struct {
	Namespace string            `json:"namespace"`
	Version   string            `json:"version"`
	Method    string            `json:"method"`
	Count     uint64            `json:"count"`
	RPS       float64           `json:"rps"`
	MeanUs    int64             `json:"mean_us"`
	P50Us     int64             `json:"p50_us"`
	P95Us     int64             `json:"p95_us"`
	P99Us     int64             `json:"p99_us"`
	Codes     map[string]uint64 `json:"codes,omitempty"`
}

// IngressStats is one per-ingress row from
// request_duration_seconds + request_self_seconds. SelfMeanUs is the
// gateway-only slice (total minus dispatch accumulator) — adopters
// reading the perf report read this as "gateway overhead".
type IngressStats struct {
	Count       uint64 `json:"count"`
	TotalMeanUs int64  `json:"total_mean_us"`
	TotalP95Us  int64  `json:"total_p95_us"`
	SelfMeanUs  int64  `json:"self_mean_us"`
	SelfP95Us   int64  `json:"self_p95_us"`
}

// BuildJSONOutput assembles a JSONOutput from a PassResult. Splitting
// build from write means tests can inspect the shape directly. The
// gateway block is included when ServerMetrics was on AND at least
// one snapshot family came back populated; otherwise it's omitted.
func BuildJSONOutput(opts Options, res PassResult) JSONOutput {
	concurrency := res.EffectiveConcurrency
	if concurrency == 0 {
		concurrency = opts.Concurrency
	}
	out := JSONOutput{
		Schema:      JSONSchemaVersion,
		Label:       res.Label,
		TargetRPS:   opts.RPS,
		Concurrency: concurrency,
		Duration:    res.Elapsed.Seconds(),
	}
	for i, t := range res.Targets {
		s := res.Stats[i]
		s.mu.Lock()
		latencies := append([]time.Duration(nil), s.latencies...)
		codesCopy := maps.Clone(s.codes)
		errs := make(map[string]uint64, len(s.errs))
		for k, v := range s.errs {
			if v > 0 {
				errs[string(k)] = v
			}
		}
		s.mu.Unlock()
		ok := atomic.LoadUint64(&s.count)
		var errsTotal uint64
		for _, v := range errs {
			errsTotal += v
		}
		row := TargetStats{
			Label:      t.Label,
			MetricsURL: t.MetricsURL,
			OK:         ok,
			Errs:       errsTotal,
			Codes:      codesCopy,
			Errors:     errs,
		}
		if res.Elapsed > 0 {
			row.AchievedRPS = float64(ok+errsTotal) / res.Elapsed.Seconds()
		}
		if len(latencies) > 0 {
			slices.Sort(latencies)
			var sum time.Duration
			for _, l := range latencies {
				sum += l
			}
			row.MeanUs = (sum / time.Duration(len(latencies))).Microseconds()
			row.P50Us = pct(latencies, 0.5).Microseconds()
			row.P95Us = pct(latencies, 0.95).Microseconds()
			row.P99Us = pct(latencies, 0.99).Microseconds()
		}
		out.Targets = append(out.Targets, row)
	}
	if gw := buildGatewayStats(res); gw != nil {
		out.Gateway = gw
	}
	return out
}

func buildGatewayStats(res PassResult) *GatewayStats {
	if res.PreSnap == nil || res.PostSnap == nil {
		return nil
	}
	disp := AggregateDispatches(res.PreSnap, res.PostSnap)
	ing := AggregateIngress(res.PreSnap, res.PostSnap)
	if len(disp) == 0 && len(ing) == 0 {
		return nil
	}
	gw := &GatewayStats{}
	if len(disp) > 0 {
		gw.Dispatches = make([]DispatchStats, 0, len(disp))
		for _, a := range disp {
			rps := 0.0
			if res.Elapsed > 0 {
				rps = float64(a.Count) / res.Elapsed.Seconds()
			}
			codes := map[string]uint64{}
			for _, c := range a.Codes {
				codes[c.Code] += c.Count
			}
			gw.Dispatches = append(gw.Dispatches, DispatchStats{
				Namespace: a.Namespace,
				Version:   a.Version,
				Method:    a.Method,
				Count:     a.Count,
				RPS:       rps,
				MeanUs:    MeanDuration(a.SumSec, a.Count).Microseconds(),
				P50Us:     histogramQuantile(0.5, a.Buckets, a.Count).Microseconds(),
				P95Us:     histogramQuantile(0.95, a.Buckets, a.Count).Microseconds(),
				P99Us:     histogramQuantile(0.99, a.Buckets, a.Count).Microseconds(),
				Codes:     codes,
			})
		}
	}
	if len(ing) > 0 {
		gw.Ingress = make(map[string]IngressStats, len(ing))
		for _, a := range ing {
			var totP95, selfP95 time.Duration
			if a.TotalCount > 0 {
				totP95 = histogramQuantile(0.95, a.TotalBuckets, a.TotalCount)
			}
			if a.SelfCount > 0 {
				selfP95 = histogramQuantile(0.95, a.SelfBuckets, a.SelfCount)
			}
			gw.Ingress[a.Ingress] = IngressStats{
				Count:       a.TotalCount,
				TotalMeanUs: MeanDuration(a.TotalSumSec, a.TotalCount).Microseconds(),
				TotalP95Us:  totP95.Microseconds(),
				SelfMeanUs:  MeanDuration(a.SelfSumSec, a.SelfCount).Microseconds(),
				SelfP95Us:   selfP95.Microseconds(),
			}
		}
	}
	return gw
}

// WriteJSON serialises BuildJSONOutput(opts, res) to w with two-
// space indent so the output is human-skimmable as well as
// machine-readable. Returns the encoder error verbatim so the
// adapter can decide whether to fail the run.
func WriteJSON(w io.Writer, opts Options, res PassResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(BuildJSONOutput(opts, res))
}
