// perf — adopter-facing performance report generator.
//
// Drives the bench stack through a sweep of synthetic load, captures
// host specs + client + server metrics, and emits a markdown report
// scoped to the run's machine. The report lives at docs/perf.md and
// answers the first question after the README: "can this work for
// my X machines doing Y RPS?".
//
// Subcommands (more to come — sweep / report are the rest):
//
//	perf specs                      print the report header (host
//	                                specs as markdown) and exit.
//	                                Sanity-check for the detector
//	                                + a building block the sweep
//	                                driver embeds verbatim.
//
// Surface lives behind `bin/bench perf` so adopters don't have to
// know about Go toolchain plumbing.
package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `usage: perf [flags]            (default: run every scenario in bench/perf-scenarios.yaml)
       perf <subcommand> [flags]

With no subcommand: reads bench/perf-scenarios.yaml, brings up the
bench stack + the upstream services each scenario needs, runs every
sweep, and renders docs/perf.md. The idiot-proof one-command path.

Subcommands (power users):
  specs   print the host-specs report header and exit.
  run     drive one sweep at a fixed scenario / query / target.
  all     run every built-in preset (no YAML); writes sweep-*.json.
  report  render docs/perf.md from sweep-*.json under --in-dir.

Run 'perf <subcommand> --help' for per-subcommand flags.
`

func main() {
	args := os.Args[1:]
	// Default behaviour (no subcommand, or flags only): drive the
	// whole YAML-defined workflow.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if err := runDefault(args); err != nil {
			fmt.Fprintln(os.Stderr, "perf:", err)
			os.Exit(1)
		}
		return
	}
	sub := args[0]
	subArgs := args[1:]
	var err error
	switch sub {
	case "specs":
		err = runSpecs(subArgs)
	case "run":
		err = runSweep(subArgs)
	case "all":
		err = runAll(subArgs)
	case "report":
		err = runReport(subArgs)
	case "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", sub)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "perf "+sub+":", err)
		os.Exit(1)
	}
}
