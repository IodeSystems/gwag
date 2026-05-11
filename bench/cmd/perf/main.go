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
)

const usage = `usage: perf <subcommand> [flags]

Subcommands:
  specs   print the report header (host specs as markdown) and exit.

Run 'perf <subcommand> --help' for per-subcommand flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "specs":
		if err := runSpecs(args); err != nil {
			fmt.Fprintln(os.Stderr, "perf specs:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", sub)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
