package main

import (
	"strings"
	"testing"
)

func TestRenderReport_KneeBlock(t *testing.T) {
	sw := Sweep{
		Schema:            SweepSchemaVersion,
		Specs:             HostSpecs{CapturedAt: "2026-01-01T00:00:00Z", CPUCores: 8, GoVersion: "go1.26.2"},
		Scenario:          "proto",
		Target:            "http://gw/api/graphql",
		StartedAt:         "2026-01-01T00:00:00Z",
		FinishedAt:        "2026-01-01T00:01:00Z",
		DurationSec:       10,
		RepsPerStep:       3,
		Warmup:            true,
		UpstreamLatencyUs: 100,
		Steps: []SweepStep{
			{TargetRPS: 1000, AchievedRPSMean: 980, MeanUsMedian: 500, P50UsMedian: 480, P95UsMedian: 900, P99UsMedian: 1100, SelfMeanUsMedian: 70, DispatchMeanUsMedian: 270},
			{TargetRPS: 5000, AchievedRPSMean: 3500, MeanUsMedian: 600, P50UsMedian: 580, P95UsMedian: 1400, P99UsMedian: 1900, SelfMeanUsMedian: 65, DispatchMeanUsMedian: 260},
		},
		Knee: &KneeInfo{
			KneeRPS:     1000,
			FailedAtRPS: 5000,
			Reason:      KneeReasonAchievedBelow80,
			Detail:      "achieved 3500 / 5000 target (70% < 80% threshold)",
		},
	}
	md, err := renderReport(reportData{
		GeneratedAt:    "2026-01-01T00:00:00Z",
		SpecsMarkdown:  sw.Specs.Markdown(),
		Sweeps:         []Sweep{sw},
		HeadlineRPS:    980,
		HeadlineP95Us:  900,
		HeadlineSelfUs: 70,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"DO NOT EDIT",
		"# Performance",
		"**Headline (proto scenario, last healthy rung):** **980 RPS**",
		"## Machine",
		"| CPU | n/a |",
		"## Scenario: `proto`",
		"pure proto/gRPC backend (greeter); baseline for native-format dispatch cost.",
		"Upstream latency configured on backend: **100µs**",
		"| 1000 | 980 |",
		"| 5000 | 3500 |",
		"**Knee detected at 5000 RPS** (achieved_below_80pct)",
		"Recommended ceiling: **1000 RPS**",
		"### Interpretation",
		"**~122 RPS / core** across 8 logical cores",
		"Gateway self-time mean is **70µs** while upstream is configured at 100µs",
		"### Knee heuristic",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q\n--- rendered ---\n%s", want, md)
		}
	}
}

func TestRenderReport_NoKneeBlock(t *testing.T) {
	sw := Sweep{
		Schema:      SweepSchemaVersion,
		Specs:       HostSpecs{CPUCores: 4},
		Scenario:    "openapi",
		Target:      "http://gw/api/graphql",
		StartedAt:   "x",
		DurationSec: 5,
		RepsPerStep: 1,
		Steps: []SweepStep{
			{TargetRPS: 1000, AchievedRPSMean: 999, P95UsMedian: 800, SelfMeanUsMedian: 50},
		},
	}
	md, err := renderReport(reportData{Sweeps: []Sweep{sw}, SpecsMarkdown: sw.Specs.Markdown()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "No knee detected") {
		t.Errorf("missing no-knee block; rendered:\n%s", md)
	}
}

func TestPickHeadlines_PrefersProto(t *testing.T) {
	sweeps := []Sweep{
		{Scenario: "openapi", Steps: []SweepStep{{TargetRPS: 1000, AchievedRPSMean: 100, P95UsMedian: 2000, SelfMeanUsMedian: 200}}},
		{Scenario: "proto", Steps: []SweepStep{{TargetRPS: 1000, AchievedRPSMean: 900, P95UsMedian: 900, SelfMeanUsMedian: 70}}},
	}
	rps, p95, self := pickHeadlines(sweeps)
	if rps != 900 || p95 != 900 || self != 70 {
		t.Errorf("pickHeadlines should prefer proto scenario; got rps=%d p95=%d self=%d", rps, p95, self)
	}
}

func TestPickHeadlines_UsesKneeRung(t *testing.T) {
	sweeps := []Sweep{{
		Scenario: "proto",
		Steps: []SweepStep{
			{TargetRPS: 1000, AchievedRPSMean: 980, P95UsMedian: 900, SelfMeanUsMedian: 70},
			{TargetRPS: 5000, AchievedRPSMean: 3500, P95UsMedian: 1500, SelfMeanUsMedian: 50},
		},
		Knee: &KneeInfo{KneeRPS: 1000, FailedAtRPS: 5000, Reason: KneeReasonAchievedBelow80},
	}}
	rps, _, _ := pickHeadlines(sweeps)
	if rps != 980 {
		t.Errorf("pickHeadlines should pick the knee rung (1000 → 980 RPS), got rps=%d", rps)
	}
}
