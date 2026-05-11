package main

import (
	"strings"
	"testing"
)

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
