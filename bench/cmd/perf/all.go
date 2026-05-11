package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// runAll implements `perf all`: walks the scenario registry,
// dispatches each one through runSweep, and writes per-scenario
// JSON files into --out-dir. Each sweep is independent — a failure
// in one scenario doesn't cancel the others; failures are surfaced
// at end-of-run with a non-zero exit code if any sweep errored.
//
// The report writer (task 6 of plan §perf-report) consumes the
// out-dir contents to produce docs/perf.md with one section per
// scenario.
func runAll(args []string) error {
	fs := flag.NewFlagSet("all", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scenariosRaw := fs.String("scenarios", strings.Join(scenarioPresetNames(), ","), "comma-separated scenarios to run (defaults to every preset)")
	outDir := fs.String("out-dir", "bench/.run/perf", "directory under which sweep-<scenario>.json is written")
	// Passthrough flags: everything that runSweep needs except
	// --scenario, --query, --out — those are computed per scenario.
	target := fs.String("target", "http://localhost:18080/api/graphql", "gateway GraphQL endpoint (passthrough)")
	stepsRaw := fs.String("steps", "1000,5000,10000,30000,60000,100000", "comma-separated target RPS rungs (passthrough)")
	duration := fs.String("duration", "10s", "duration per rep (passthrough)")
	reps := fs.String("reps", "3", "reps per step (passthrough)")
	noKnee := fs.Bool("no-knee", false, "passthrough: disable knee-detection early stop")
	keepReps := fs.Bool("keep-reps", false, "passthrough: keep per-rep traffic sidecars")
	upstreamLatency := fs.String("upstream-latency", "0", "passthrough: --upstream-latency stamp for every sub-sweep")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: perf all [flags]")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output())
		fmt.Fprint(fs.Output(), scenarioHelp())
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional args: %v", fs.Args())
	}

	scenarios := parseScenarios(*scenariosRaw)
	if len(scenarios) == 0 {
		return fmt.Errorf("--scenarios cannot be empty")
	}
	for _, s := range scenarios {
		if scenarioPresetByName(s) == nil {
			return fmt.Errorf("unknown scenario %q; built-ins: %s", s, strings.Join(scenarioPresetNames(), ", "))
		}
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	var failed []string
	for _, s := range scenarios {
		out := filepath.Join(*outDir, "sweep-"+s+".json")
		fmt.Fprintf(os.Stderr, "==> scenario %s → %s\n", s, out)
		subArgs := []string{
			"--scenario", s,
			"--target", *target,
			"--steps", *stepsRaw,
			"--duration", *duration,
			"--reps", *reps,
			"--upstream-latency", *upstreamLatency,
			"--out", out,
		}
		if *noKnee {
			subArgs = append(subArgs, "--no-knee")
		}
		if *keepReps {
			subArgs = append(subArgs, "--keep-reps")
		}
		if err := runSweep(subArgs); err != nil {
			fmt.Fprintf(os.Stderr, "scenario %s: %v\n", s, err)
			failed = append(failed, s)
			continue
		}
		// runSweep mutates a package-level streaming flag; reset
		// between scenarios so the second sweep prints its own
		// header row.
		stepHeaderPrinted = false
	}
	if len(failed) > 0 {
		return fmt.Errorf("scenarios failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func parseScenarios(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
