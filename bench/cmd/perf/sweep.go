package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sweep is the wire shape one `perf run` invocation produces. The
// report writer consumes Sweeps verbatim — one Sweep per scenario.
type Sweep struct {
	Schema      string      `json:"schema"`
	Specs       HostSpecs   `json:"specs"`
	Scenario    string      `json:"scenario"`
	Target      string      `json:"target"`
	StartedAt   string      `json:"started_at"`
	FinishedAt  string      `json:"finished_at"`
	DurationSec float64     `json:"duration_seconds_per_rep"`
	RepsPerStep int         `json:"reps_per_step"`
	Warmup      bool        `json:"warmup_rep_discarded"`
	Steps       []SweepStep `json:"steps"`
	Knee        *KneeInfo   `json:"knee,omitempty"`
}

// SweepStep is one rung of the sweep — N reps at a single target RPS.
// Aggregates are computed over the non-warmup reps; the raw rep
// outputs are kept so the report (and a human looking at the JSON)
// can drill in.
type SweepStep struct {
	TargetRPS               int          `json:"target_rps"`
	Reps                    []TrafficRep `json:"reps"`
	AchievedRPSMean         float64      `json:"achieved_rps_mean"`
	MeanUsMedian            int64        `json:"client_mean_us_median"`
	P50UsMedian             int64        `json:"client_p50_us_median"`
	P95UsMedian             int64        `json:"client_p95_us_median"`
	P99UsMedian             int64        `json:"client_p99_us_median"`
	SelfMeanUsMedian        int64        `json:"gateway_self_mean_us_median,omitempty"`
	DispatchMeanUsMedian    int64        `json:"gateway_dispatch_mean_us_median,omitempty"`
}

// TrafficRep is a thin wrapper around the JSON the traffic binary
// emits. We preserve it verbatim so a sweep file is forensically
// complete — anything the report or a human needs is in here.
type TrafficRep struct {
	RepIndex int             `json:"rep_index"`
	IsWarmup bool            `json:"is_warmup"`
	Output   json.RawMessage `json:"output"`
}

// KneeInfo is filled in by knee detection (task 3); empty in this
// commit. The field stays on the struct so downstream consumers
// (report writer) don't need an additive schema bump later.
type KneeInfo struct {
	TargetRPS int    `json:"target_rps"`
	Reason    string `json:"reason"`
}

// SweepSchemaVersion identifies the on-disk shape so the report
// writer can refuse a snapshot it doesn't understand.
const SweepSchemaVersion = "bench-perf-sweep/v1"

// runSweep implements `perf run`. Drives bench/.run/bin/traffic at
// escalating target RPS, captures the per-rep --json sidecar, and
// emits a Sweep summary (stdout table + on-disk JSON).
func runSweep(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	target := fs.String("target", "http://localhost:18080/api/graphql", "gateway GraphQL endpoint (used for traffic + /api/metrics)")
	scenario := fs.String("scenario", "graphql", "report label for this sweep (graphql/openapi/mixed/...)")
	stepsRaw := fs.String("steps", "1000,5000,10000,30000,60000,100000", "comma-separated target RPS rungs")
	duration := fs.Duration("duration", 10*time.Second, "duration of each rep")
	reps := fs.Int("reps", 3, "reps per step; rep 1 is the warm-up and excluded from aggregates unless --no-warmup")
	noWarmup := fs.Bool("no-warmup", false, "treat every rep as data (default discards rep 1)")
	outPath := fs.String("out", "bench/.run/perf/sweep.json", "where to write the Sweep JSON; '-' for stdout")
	keepReps := fs.Bool("keep-reps", false, "preserve per-rep traffic --json sidecars under <out>.reps/; default cleans them after aggregation")
	query := fs.String("query", `{ greeter { hello(name: "world") { greeting } } }`, "GraphQL query string for this sweep")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: perf run [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional args: %v", fs.Args())
	}

	steps, err := parseSteps(*stepsRaw)
	if err != nil {
		return err
	}
	if *reps < 1 {
		return fmt.Errorf("--reps must be ≥ 1")
	}

	trafficBin, err := ensureTrafficBinary()
	if err != nil {
		return err
	}

	sw := Sweep{
		Schema:      SweepSchemaVersion,
		Specs:       CollectSpecs(),
		Scenario:    *scenario,
		Target:      *target,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		DurationSec: duration.Seconds(),
		RepsPerStep: *reps,
		Warmup:      !*noWarmup && *reps > 1,
	}

	repsDir := strings.TrimSuffix(*outPath, ".json") + ".reps"
	if *outPath == "-" {
		repsDir, err = os.MkdirTemp("", "perf-reps-")
		if err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(repsDir, 0o755); err != nil {
			return err
		}
	}
	defer func() {
		if !*keepReps {
			os.RemoveAll(repsDir)
		}
	}()

	for _, rps := range steps {
		step, err := runStep(trafficBin, *target, *query, rps, *duration, *reps, sw.Warmup, repsDir)
		if err != nil {
			return fmt.Errorf("step rps=%d: %w", rps, err)
		}
		sw.Steps = append(sw.Steps, step)
		// Stream-print the step's aggregate row as it finishes so a
		// long sweep gives the operator feedback without buffering.
		printStepRow(os.Stdout, step)
	}

	sw.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	if err := writeSweep(*outPath, sw); err != nil {
		return err
	}
	if *outPath != "-" {
		fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
		if *keepReps {
			fmt.Fprintf(os.Stderr, "per-rep sidecars kept at %s\n", repsDir)
		}
	}
	return nil
}

// runStep drives one (target-rps) rung — runs `reps` traffic
// invocations, parses each --json output, aggregates the
// non-warmup reps.
func runStep(trafficBin, target, query string, rps int, dur time.Duration, reps int, warmup bool, repsDir string) (SweepStep, error) {
	step := SweepStep{TargetRPS: rps}
	type aggInputs struct {
		achievedRPS []float64
		meanUs      []int64
		p50, p95, p99 []int64
		selfMeanUs  []int64
		dispMeanUs  []int64
	}
	var inputs aggInputs
	for r := 1; r <= reps; r++ {
		repPath := filepath.Join(repsDir, fmt.Sprintf("rps-%d-rep-%d.json", rps, r))
		args := []string{
			"graphql",
			"--rps", strconv.Itoa(rps),
			"--duration", dur.String(),
			"--target", target,
			"--query", query,
			"--json", repPath,
		}
		cmd := exec.Command(trafficBin, args...)
		cmd.Stderr = os.Stderr // traffic prints its summary to stdout — we discard it, errors land here
		if err := cmd.Run(); err != nil {
			return step, fmt.Errorf("rep %d/%d traffic: %w", r, reps, err)
		}
		raw, err := os.ReadFile(repPath)
		if err != nil {
			return step, fmt.Errorf("rep %d/%d read sidecar: %w", r, reps, err)
		}
		isWarmup := warmup && r == 1
		step.Reps = append(step.Reps, TrafficRep{
			RepIndex: r,
			IsWarmup: isWarmup,
			Output:   raw,
		})
		if isWarmup {
			continue
		}
		td, err := parseTrafficOutput(raw)
		if err != nil {
			return step, fmt.Errorf("rep %d/%d parse: %w", r, reps, err)
		}
		inputs.achievedRPS = append(inputs.achievedRPS, td.achievedRPS)
		inputs.meanUs = append(inputs.meanUs, td.meanUs)
		inputs.p50 = append(inputs.p50, td.p50)
		inputs.p95 = append(inputs.p95, td.p95)
		inputs.p99 = append(inputs.p99, td.p99)
		if td.selfMeanUs > 0 {
			inputs.selfMeanUs = append(inputs.selfMeanUs, td.selfMeanUs)
		}
		if td.dispMeanUs > 0 {
			inputs.dispMeanUs = append(inputs.dispMeanUs, td.dispMeanUs)
		}
	}
	step.AchievedRPSMean = mean(inputs.achievedRPS)
	step.MeanUsMedian = medianInt64(inputs.meanUs)
	step.P50UsMedian = medianInt64(inputs.p50)
	step.P95UsMedian = medianInt64(inputs.p95)
	step.P99UsMedian = medianInt64(inputs.p99)
	step.SelfMeanUsMedian = medianInt64(inputs.selfMeanUs)
	step.DispatchMeanUsMedian = medianInt64(inputs.dispMeanUs)
	return step, nil
}

// trafficData is the subset of bench-traffic/v1 the sweep driver
// aggregates per step. Decoupled from the runner.JSONOutput type so
// a future bench-traffic/v2 can introduce additive fields without
// the sweep driver caring.
type trafficData struct {
	achievedRPS float64
	meanUs      int64
	p50, p95, p99 int64
	selfMeanUs  int64
	dispMeanUs  int64
}

func parseTrafficOutput(raw []byte) (trafficData, error) {
	var v struct {
		Schema  string `json:"schema"`
		Targets []struct {
			OK          uint64  `json:"ok"`
			AchievedRPS float64 `json:"achieved_rps"`
			MeanUs      int64   `json:"mean_us"`
			P50Us       int64   `json:"p50_us"`
			P95Us       int64   `json:"p95_us"`
			P99Us       int64   `json:"p99_us"`
		} `json:"targets"`
		Gateway *struct {
			Dispatches []struct {
				Count  uint64 `json:"count"`
				MeanUs int64  `json:"mean_us"`
			} `json:"dispatches"`
			Ingress map[string]struct {
				SelfMeanUs int64 `json:"self_mean_us"`
			} `json:"ingress"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return trafficData{}, err
	}
	if v.Schema != "" && !strings.HasPrefix(v.Schema, "bench-traffic/v1") {
		return trafficData{}, fmt.Errorf("unexpected schema %q (sweep driver knows bench-traffic/v1)", v.Schema)
	}
	if len(v.Targets) == 0 {
		return trafficData{}, errors.New("no targets in traffic JSON")
	}
	t := v.Targets[0]
	td := trafficData{
		achievedRPS: t.AchievedRPS,
		meanUs:      t.MeanUs,
		p50:         t.P50Us,
		p95:         t.P95Us,
		p99:         t.P99Us,
	}
	if v.Gateway != nil {
		// Ingress mean: pool across ingresses by weighting on count.
		// Sweep driver runs only graphql today, so this collapses to
		// one entry — but the math stays right when openapi/grpc
		// scenarios join.
		var sum, count int64
		for _, ing := range v.Gateway.Ingress {
			sum += ing.SelfMeanUs
			count++
		}
		if count > 0 {
			td.selfMeanUs = sum / count
		}
		var dSum, dCount int64
		for _, d := range v.Gateway.Dispatches {
			dSum += d.MeanUs * int64(d.Count)
			dCount += int64(d.Count)
		}
		if dCount > 0 {
			td.dispMeanUs = dSum / dCount
		}
	}
	return td, nil
}

func parseSteps(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("--steps: %q is not an integer", p)
		}
		if n <= 0 {
			return nil, fmt.Errorf("--steps: %d is not > 0", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("--steps cannot be empty")
	}
	return out, nil
}

// ensureTrafficBinary returns the path to bench/.run/bin/traffic,
// building it if absent. The path is resolved relative to the perf
// binary's CWD (which is the repo root when invoked via bin/bench).
// Mirrors cmd_traffic in bin/bench.
func ensureTrafficBinary() (string, error) {
	bin := filepath.Join("bench", ".run", "bin", "traffic")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	fmt.Fprintln(os.Stderr, "==> building traffic generator (bench/.run/bin/traffic)")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", bin, "./bench/cmd/traffic")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build traffic: %w", err)
	}
	return bin, nil
}

func writeSweep(path string, sw Sweep) error {
	var w *os.File
	if path == "-" {
		w = os.Stdout
	} else {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sw)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func medianInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := slices.Clone(xs)
	slices.Sort(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

// printStepRow appends one fixed-width row to stdout. tabwriter
// resets column widths per Flush so streaming rows with tabwriter
// only align within one call; fixed widths keep the table readable
// as the sweep progresses without buffering rows to end-of-sweep.
var stepHeaderPrinted bool

const stepRowFormat = "%-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s  %-10s\n"

func printStepRow(w *os.File, step SweepStep) {
	if !stepHeaderPrinted {
		fmt.Fprintf(w, stepRowFormat,
			"TARGET_RPS", "ACHIEVED", "MEAN", "P50", "P95", "P99", "SELF_MEAN", "DISPATCH_MEAN")
		stepHeaderPrinted = true
	}
	fmt.Fprintf(w, stepRowFormat,
		strconv.Itoa(step.TargetRPS),
		fmt.Sprintf("%.0f", step.AchievedRPSMean),
		usToStr(step.MeanUsMedian),
		usToStr(step.P50UsMedian),
		usToStr(step.P95UsMedian),
		usToStr(step.P99UsMedian),
		usToStr(step.SelfMeanUsMedian),
		usToStr(step.DispatchMeanUsMedian),
	)
}

func usToStr(us int64) string {
	if us <= 0 {
		return "-"
	}
	if us < 1000 {
		return fmt.Sprintf("%dµs", us)
	}
	return fmt.Sprintf("%.2fms", float64(us)/1000)
}
