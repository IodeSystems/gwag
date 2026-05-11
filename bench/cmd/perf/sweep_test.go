package main

import (
	"strings"
	"testing"
)

func TestScenarioPresetsRegistered(t *testing.T) {
	// Pin that the plan's three scenarios are wired and resolve to
	// non-empty queries — the report writer + `perf all` rely on the
	// registry shape.
	for _, want := range []string{"proto", "openapi", "mixed"} {
		p := scenarioPresetByName(want)
		if p == nil {
			t.Errorf("scenario %q missing from preset registry", want)
			continue
		}
		if p.query == "" {
			t.Errorf("scenario %q has empty query", want)
		}
		if len(p.requiresNamespaces) == 0 {
			t.Errorf("scenario %q declares no required namespaces", want)
		}
	}
	if scenarioPresetByName("nonexistent") != nil {
		t.Error("scenarioPresetByName should return nil for unknown name")
	}
}

func TestParseScenarios(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"proto,openapi,mixed", []string{"proto", "openapi", "mixed"}},
		{" proto , openapi ", []string{"proto", "openapi"}},
		{"", nil},
		{",,proto,,", []string{"proto"}},
	}
	for _, c := range cases {
		got := parseScenarios(c.in)
		if !sliceEqualStr(got, c.want) {
			t.Errorf("parseScenarios(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func sliceEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseSteps(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		err  bool
	}{
		{"1000,5000,10000", []int{1000, 5000, 10000}, false},
		{" 100 , 200 ", []int{100, 200}, false},
		{"", nil, true},
		{"abc", nil, true},
		{"100,-5", nil, true},
		{"100,,200", []int{100, 200}, false}, // empties dropped
	}
	for _, c := range cases {
		got, err := parseSteps(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseSteps(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSteps(%q) error %v", c.in, err)
			continue
		}
		if !slicesEqualInt(got, c.want) {
			t.Errorf("parseSteps(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func slicesEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMedianInt64(t *testing.T) {
	cases := []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{}, 0},
		{[]int64{5}, 5},
		{[]int64{3, 1, 2}, 2},        // odd: middle after sort
		{[]int64{4, 1, 3, 2}, 2},     // even: mean of two middles -> (2+3)/2 = 2 (int divide)
		{[]int64{10, 20, 30, 40}, 25}, // even: (20+30)/2
	}
	for _, c := range cases {
		got := medianInt64(c.in)
		if got != c.want {
			t.Errorf("medianInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseTrafficOutput_SchemaCheck(t *testing.T) {
	// Schema mismatch surfaces as an error so a v2 sidecar can't be
	// silently aggregated into a v1 sweep.
	_, err := parseTrafficOutput([]byte(`{"schema":"bench-traffic/v2","targets":[]}`))
	if err == nil {
		t.Fatal("expected error on unknown schema")
	}
	if !strings.Contains(err.Error(), "bench-traffic/v1") {
		t.Errorf("error doesn't name expected schema: %v", err)
	}
}

func TestParseTrafficOutput_HappyPath(t *testing.T) {
	raw := []byte(`{
	  "schema": "bench-traffic/v1",
	  "target_rps": 5000,
	  "concurrency": 250,
	  "duration_seconds": 3.0,
	  "targets": [
	    {"label":"x","ok":10000,"errs":0,"achieved_rps":3333.3,"mean_us":500,"p50_us":480,"p95_us":900,"p99_us":1200}
	  ],
	  "gateway": {
	    "dispatches": [
	      {"namespace":"greeter","version":"v1","method":"Hello","count":10000,"rps":3333.3,"mean_us":200,"p50_us":180,"p95_us":400,"p99_us":600,"codes":{"ok":10000}}
	    ],
	    "ingress": {
	      "graphql": {"count":10000,"total_mean_us":350,"total_p95_us":700,"self_mean_us":75,"self_p95_us":250}
	    }
	  }
	}`)
	td, err := parseTrafficOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if td.achievedRPS != 3333.3 {
		t.Errorf("achievedRPS = %v", td.achievedRPS)
	}
	if td.p50 != 480 || td.p95 != 900 || td.p99 != 1200 {
		t.Errorf("client percentiles wrong: %+v", td)
	}
	if td.selfMeanUs != 75 {
		t.Errorf("selfMeanUs = %d, want 75", td.selfMeanUs)
	}
	if td.dispMeanUs != 200 {
		t.Errorf("dispatch mean = %d, want 200 (weighted single row)", td.dispMeanUs)
	}
}

func TestParseTrafficOutput_NoGatewayBlock(t *testing.T) {
	raw := []byte(`{"schema":"bench-traffic/v1","targets":[{"ok":1,"achieved_rps":1,"mean_us":1,"p50_us":1,"p95_us":1,"p99_us":1}]}`)
	td, err := parseTrafficOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if td.selfMeanUs != 0 || td.dispMeanUs != 0 {
		t.Errorf("missing gateway block should leave gateway-side fields zero; got %+v", td)
	}
}

func TestDetectKnee_EmptyOrSingleHealthy(t *testing.T) {
	if _, hit := detectKnee(nil); hit {
		t.Error("empty steps: should not fire")
	}
	// Single healthy step — achieved at target, no prior. No fire.
	healthy := SweepStep{TargetRPS: 1000, AchievedRPSMean: 990, P99UsMedian: 1000}
	if _, hit := detectKnee([]SweepStep{healthy}); hit {
		t.Error("healthy single step should not fire")
	}
}

func TestDetectKnee_AchievedBelow80OnFirstStep(t *testing.T) {
	// First step achieves only 60% of target → fires with KneeRPS=0
	// because there's no prior healthy rung.
	step := SweepStep{TargetRPS: 1000, AchievedRPSMean: 600, P99UsMedian: 1000}
	k, hit := detectKnee([]SweepStep{step})
	if !hit {
		t.Fatal("expected knee fire on first under-achieving step")
	}
	if k.Reason != KneeReasonAchievedBelow80 {
		t.Errorf("reason = %q, want %q", k.Reason, KneeReasonAchievedBelow80)
	}
	if k.FailedAtRPS != 1000 {
		t.Errorf("failed_at = %d, want 1000", k.FailedAtRPS)
	}
	if k.KneeRPS != 0 {
		t.Errorf("knee_rps = %d, want 0 (no prior healthy rung)", k.KneeRPS)
	}
}

func TestDetectKnee_AchievedBelow80AfterHealthy(t *testing.T) {
	prev := SweepStep{TargetRPS: 1000, AchievedRPSMean: 980, P99UsMedian: 800}
	cur := SweepStep{TargetRPS: 5000, AchievedRPSMean: 3600, P99UsMedian: 1500} // 72% < 80%
	k, hit := detectKnee([]SweepStep{prev, cur})
	if !hit {
		t.Fatal("expected knee fire when achieved < 80%")
	}
	if k.KneeRPS != 1000 {
		t.Errorf("knee_rps = %d, want 1000 (prior healthy rung)", k.KneeRPS)
	}
	if k.FailedAtRPS != 5000 {
		t.Errorf("failed_at = %d, want 5000", k.FailedAtRPS)
	}
	if k.Reason != KneeReasonAchievedBelow80 {
		t.Errorf("reason = %q", k.Reason)
	}
}

func TestDetectKnee_P99CliffRequiresThroughputStall(t *testing.T) {
	// Achieved stays > 80% AND achieved still climbs — p99 doubles but
	// throughput is healthy, so the rule must NOT fire (this is the
	// natural queueing creep that fired the old too-eager rule).
	prev := SweepStep{TargetRPS: 1000, AchievedRPSMean: 980, P99UsMedian: 500}
	cur := SweepStep{TargetRPS: 5000, AchievedRPSMean: 4900, P99UsMedian: 1500}
	if _, hit := detectKnee([]SweepStep{prev, cur}); hit {
		t.Error("p99 doubled but throughput is climbing — must not fire")
	}
}

func TestDetectKnee_P99CliffFiresWhenThroughputStalls(t *testing.T) {
	// p99 doubles AND achieved RPS stops growing → real saturation
	// via latency. Target picked so achieved stays > 80% (otherwise
	// the achieved-below-80 rule fires first; that ordering is
	// asserted in TestDetectKnee_BothRulesFire_AchievedWins).
	prev := SweepStep{TargetRPS: 25000, AchievedRPSMean: 24500, P99UsMedian: 30000}
	cur := SweepStep{TargetRPS: 27000, AchievedRPSMean: 24500, P99UsMedian: 100000}
	k, hit := detectKnee([]SweepStep{prev, cur})
	if !hit {
		t.Fatal("expected knee fire when p99 doubled + throughput plateau")
	}
	if k.Reason != KneeReasonP99Cliff {
		t.Errorf("reason = %q, want %q", k.Reason, KneeReasonP99Cliff)
	}
	if k.KneeRPS != 25000 {
		t.Errorf("knee_rps = %d, want 25000", k.KneeRPS)
	}
}

func TestDetectKnee_BothRulesFire_AchievedWins(t *testing.T) {
	// Both predicates fire; the achieved-below-80 rule checks first
	// (it's the more reliable signal — bench client can saturate
	// before the gateway notices). Test pins the precedence so a
	// future refactor doesn't quietly swap order.
	prev := SweepStep{TargetRPS: 1000, AchievedRPSMean: 980, P99UsMedian: 500}
	cur := SweepStep{TargetRPS: 5000, AchievedRPSMean: 3000, P99UsMedian: 1500}
	k, hit := detectKnee([]SweepStep{prev, cur})
	if !hit {
		t.Fatal("expected knee fire")
	}
	if k.Reason != KneeReasonAchievedBelow80 {
		t.Errorf("reason = %q, want %q (achieved-below-80 should win precedence)", k.Reason, KneeReasonAchievedBelow80)
	}
}

func TestDetectKnee_PriorP99ZeroIgnored(t *testing.T) {
	// histogram-coarse 0 p99 from a low-RPS first step must not trip
	// the doubling rule — anything ×2 of 0 is still 0, and we'd
	// otherwise flag every legitimate rung past the first as "knee".
	prev := SweepStep{TargetRPS: 100, AchievedRPSMean: 99, P99UsMedian: 0}
	cur := SweepStep{TargetRPS: 1000, AchievedRPSMean: 990, P99UsMedian: 800}
	if _, hit := detectKnee([]SweepStep{prev, cur}); hit {
		t.Error("zero prior-p99 should not trip the doubling rule")
	}
}

func TestParseTrafficOutput_DispatchWeighting(t *testing.T) {
	// Two dispatches with different counts — weighted mean should
	// favour the larger row.
	raw := []byte(`{
	  "schema": "bench-traffic/v1",
	  "targets": [{"ok":1,"achieved_rps":1,"mean_us":1,"p50_us":1,"p95_us":1,"p99_us":1}],
	  "gateway": {
	    "dispatches": [
	      {"count": 1000, "mean_us": 100},
	      {"count": 9000, "mean_us": 1000}
	    ],
	    "ingress": {}
	  }
	}`)
	td, err := parseTrafficOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	// (1000*100 + 9000*1000) / 10000 = (100000 + 9000000) / 10000 = 910
	if td.dispMeanUs != 910 {
		t.Errorf("weighted dispatch mean = %d, want 910", td.dispMeanUs)
	}
}
