package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"
)

// runReport implements `perf report`. Globs sweep JSON files under
// --in-dir (defaults to bench/.run/perf), renders them through the
// report template, and writes to --out (defaults to docs/perf.md).
//
// The "do not edit" banner at the top of the rendered file is the
// promise back to the operator: every line below regenerates from
// the sweep JSONs, so a stale docs/perf.md never goes uncaught.
func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	inDir := fs.String("in-dir", "bench/.run/perf", "directory of sweep-*.json inputs")
	out := fs.String("out", "docs/perf.md", "where to write the report; '-' for stdout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: perf report [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional args: %v", fs.Args())
	}

	sweeps, err := loadSweeps(*inDir)
	if err != nil {
		return err
	}
	if len(sweeps) == 0 {
		return fmt.Errorf("no sweep-*.json files found in %s (run `bench perf run` or `bench perf all` first)", *inDir)
	}

	specs := sweeps[0].Specs
	headlineRPS, headlineLatency, headlineSelf := pickHeadlines(sweeps)

	tmplData := reportData{
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		SpecsMarkdown:   specs.Markdown(),
		Sweeps:          sweeps,
		HeadlineRPS:     headlineRPS,
		HeadlineP95Us:   headlineLatency,
		HeadlineSelfUs:  headlineSelf,
	}

	md, err := renderReport(tmplData)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err := os.Stdout.WriteString(md)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d scenario%s)\n", *out, len(sweeps), plural(len(sweeps)))
	return nil
}

// loadSweeps walks dir for sweep-*.json files, decodes each into a
// Sweep, and returns them in scenario-name order so the rendered
// report is deterministic.
func loadSweeps(dir string) ([]Sweep, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read in-dir: %w", err)
	}
	var out []Sweep
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "sweep-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		var sw Sweep
		if err := json.Unmarshal(raw, &sw); err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		if sw.Schema != SweepSchemaVersion {
			return nil, fmt.Errorf("%s schema=%q, expected %q (re-run the sweep)", name, sw.Schema, SweepSchemaVersion)
		}
		out = append(out, sw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scenario < out[j].Scenario })
	return out, nil
}

// pickHeadlines extracts the headline numbers for the README/intro
// quote: the highest healthy achieved-RPS, the matching p95 and the
// gateway self-time mean. Picks from the proto scenario when
// present (best-case native dispatch); otherwise the first sweep.
func pickHeadlines(sweeps []Sweep) (rps, p95Us, selfUs int64) {
	pick := sweeps[0]
	for _, s := range sweeps {
		if s.Scenario == "proto" {
			pick = s
			break
		}
	}
	// Use the knee — the last healthy rung — when one was detected.
	// Otherwise the highest-RPS step that landed.
	if len(pick.Steps) == 0 {
		return 0, 0, 0
	}
	step := pick.Steps[len(pick.Steps)-1]
	if pick.Knee != nil && pick.Knee.KneeRPS > 0 {
		for _, s := range pick.Steps {
			if s.TargetRPS == pick.Knee.KneeRPS {
				step = s
				break
			}
		}
	}
	return int64(step.AchievedRPSMean), step.P95UsMedian, step.SelfMeanUsMedian
}

// reportData is the template input. Kept separate from Sweep so the
// template author can address derived fields by name.
type reportData struct {
	GeneratedAt    string
	SpecsMarkdown  string
	Sweeps         []Sweep
	HeadlineRPS    int64
	HeadlineP95Us  int64
	HeadlineSelfUs int64
}

const reportTmpl = `<!--
DO NOT EDIT — this file regenerates from sweep JSONs via 'bin/bench perf report'.
Run 'bin/bench perf all' to refresh the inputs under bench/.run/perf/.
-->

# Performance

> _Generated {{ .GeneratedAt }} from {{ len .Sweeps }} scenario sweep{{ if ne (len .Sweeps) 1 }}s{{ end }} via ` + "`bin/bench perf report`" + `._

{{- if and (gt .HeadlineRPS 0) (gt .HeadlineP95Us 0) }}

**Headline (proto scenario, last healthy rung):** **{{ .HeadlineRPS }} RPS** at p95 **{{ usFormat .HeadlineP95Us }}** with gateway self-time mean **{{ usFormat .HeadlineSelfUs }}**.
{{- end }}

## Machine

{{ .SpecsMarkdown }}

{{ range .Sweeps -}}
## Scenario: ` + "`{{ .Scenario }}`" + `

{{- with scenarioDescription .Scenario }}
{{ . }}
{{ end }}

- Endpoint: ` + "`{{ .Target }}`" + `
- Duration per rep: ` + "`{{ printf \"%.1fs\" .DurationSec }}`" + ` × {{ .RepsPerStep }} rep{{ if ne .RepsPerStep 1 }}s{{ end }}{{ if .Warmup }} (rep 1 discarded as warm-up){{ end }}
{{- if gt .UpstreamLatencyUs 0 }}
- Upstream latency configured on backend: **{{ usFormat .UpstreamLatencyUs }}**
{{- end }}

| Target RPS | Achieved | Client mean | p50 | p95 | p99 | Gateway self (mean) | Dispatch (mean) |
|---:|---:|---:|---:|---:|---:|---:|---:|
{{- range .Steps }}
| {{ .TargetRPS }} | {{ printf "%.0f" .AchievedRPSMean }} | {{ usFormat .MeanUsMedian }} | {{ usFormat .P50UsMedian }} | {{ usFormat .P95UsMedian }} | {{ usFormat .P99UsMedian }} | {{ usFormat .SelfMeanUsMedian }} | {{ usFormat .DispatchMeanUsMedian }} |
{{- end }}

{{- if .Knee }}

**Knee detected at {{ .Knee.FailedAtRPS }} RPS** ({{ .Knee.Reason }}): {{ .Knee.Detail }}.
{{- if gt .Knee.KneeRPS 0 }} Recommended ceiling: **{{ .Knee.KneeRPS }} RPS** on this host.{{ end }}
{{- else }}

No knee detected within the configured sweep — the gateway absorbed every rung tested without tripping the achieved-below-80%-of-target or p99-doubled predicates. Push higher with ` + "`--steps`" + ` to find the actual ceiling.
{{- end }}

### Interpretation

{{ interpretSweep . }}

{{ end }}
## How to read this

Three numbers tell most of the story per scenario:

- **Achieved RPS / target RPS** — anything < 80 % of target is saturation (gateway, client, or upstream).
- **Gateway self (mean)** — the gateway-only slice of each request (` + "`request_self_seconds`" + ` mean). Compare across upstream-latency runs to see "how much does the gateway add"; this number should be roughly upstream-independent.
- **Dispatch (mean)** — upstream time as measured by the gateway. Climbs with configured upstream latency; the delta vs. self-time is the upstream's contribution.

### Knee heuristic

A rung is flagged as the knee when:

- **achieved_below_80pct** — actual RPS < 0.80 × target. The client / gateway / upstream couldn't keep up; throughput collapsed.
- **p99_cliff** — step's p99 > 2 × prior step's p99 **AND** achieved RPS no longer climbed vs the prior step. Catches saturation-via-latency: the gateway is going slow rather than dropping requests. A pure latency creep with healthy throughput growth is normal queueing, not a knee, and is intentionally not flagged.

First-firing predicate stops the sweep; the prior step is the recommended ceiling. Pass ` + "`--no-knee`" + ` to ` + "`bench perf run`" + ` to walk every rung regardless (useful for the full curve).

### Regenerating

The one-command path reads ` + "`bench/perf-scenarios.yaml`" + `, brings up
the stack and the upstream services each scenario needs, runs every
sweep, and renders this file:

` + "```" + `bash
bin/bench perf
` + "```" + `

Customise the sweep (different RPS rungs, your own query, regression
runs) by editing ` + "`bench/perf-scenarios.yaml`" + ` or passing
` + "`--config path/to/your.yaml`" + `.

Subcommands for power users:

` + "```" + `bash
bin/bench perf specs                  # print host-specs header only
bin/bench perf run --scenario proto   # one ad-hoc sweep
bin/bench perf report --in-dir ...    # re-render without re-running
` + "```" + `
`

func renderReport(data reportData) (string, error) {
	t := template.New("perf").Funcs(template.FuncMap{
		"usFormat":             usToStr,
		"scenarioDescription":  scenarioDescriptionForReport,
		"interpretSweep":       interpretSweep,
	})
	t, err := t.Parse(reportTmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// scenarioDescriptionForReport pulls the preset's description so the
// rendered section explains itself. Returns "" for ad-hoc scenarios
// the registry doesn't know about — the template skips the block on
// empty.
func scenarioDescriptionForReport(name string) string {
	if p := scenarioPresetByName(name); p != nil {
		return p.description + "."
	}
	return ""
}

// interpretSweep emits a short paragraph derived from the data: per-
// core RPS, where the knee landed, and the gateway-overhead framing
// when upstream-latency is non-zero. Stays terse so a reader can
// skim a 3-scenario report in under a minute.
func interpretSweep(sw Sweep) string {
	if len(sw.Steps) == 0 {
		return "_No data._"
	}
	// Use the knee rung when available; otherwise the final step.
	var ref SweepStep
	for _, s := range sw.Steps {
		ref = s
		if sw.Knee != nil && sw.Knee.KneeRPS == s.TargetRPS {
			ref = s
			break
		}
	}
	var parts []string
	if sw.Specs.CPUCores > 0 && ref.AchievedRPSMean > 0 {
		parts = append(parts, fmt.Sprintf("**~%.0f RPS / core** across %d logical cores at the recommended ceiling.",
			ref.AchievedRPSMean/float64(sw.Specs.CPUCores), sw.Specs.CPUCores))
	}
	if ref.SelfMeanUsMedian > 0 {
		if sw.UpstreamLatencyUs > 0 {
			parts = append(parts, fmt.Sprintf("Gateway self-time mean is **%s** while upstream is configured at %s — the gateway's contribution is the self-time slice, independent of upstream cost.",
				usToStr(ref.SelfMeanUsMedian), usToStr(sw.UpstreamLatencyUs)))
		} else {
			parts = append(parts, fmt.Sprintf("Gateway self-time mean is **%s** at the recommended ceiling — this is the per-request overhead the gateway adds on top of whatever the upstream takes.",
				usToStr(ref.SelfMeanUsMedian)))
		}
	}
	if sw.Knee != nil {
		switch sw.Knee.Reason {
		case KneeReasonAchievedBelow80:
			parts = append(parts, fmt.Sprintf("The knee fired because achieved RPS fell below 80%% of target — typically the bench client itself running out of fired RPS, the gateway, or an upstream cap. Drill into `bench/.run/perf/sweep-%s.reps/` with `--keep-reps` to see which.", sw.Scenario))
		case KneeReasonP99Cliff:
			parts = append(parts, "The knee fired because p99 latency doubled while throughput stopped climbing — the gateway saturated by going slow rather than dropping requests. Look for backpressure (per-pool queue depth, per-replica inflight) on the upstream or contention in the dispatch path.")
		}
	}
	if len(parts) == 0 {
		return "_No interpretable signal — re-run with more reps or a wider --steps range._"
	}
	return strings.Join(parts, " ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
