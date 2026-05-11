package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runDefault implements `bin/bench perf` with no subcommand — the
// "idiot-proof" one-command path that loads bench/perf-scenarios.yaml,
// brings up the stack + the upstream services each scenario needs,
// runs every sweep, and renders docs/perf.md.
//
// Power users still use the subcommands directly (`perf run` for
// one scenario, `perf report` to re-render without re-running).
func runDefault(args []string) error {
	fs := flag.NewFlagSet("perf", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath(), "scenarios YAML file (default: bench/perf-scenarios.yaml)")
	outDir := fs.String("out-dir", "bench/.run/perf", "directory for per-scenario sweep JSONs")
	reportOut := fs.String("report", "docs/perf.md", "where to write the rendered report; '-' for stdout, '' to skip")
	skipBringup := fs.Bool("skip-bringup", false, "assume `bin/bench up` is already running; don't try to start it")
	skipServices := fs.Bool("skip-services", false, "assume the required upstream services are already registered; don't add them")
	noProfile := fs.Bool("no-profile", false, "skip per-scenario pprof capture at the recommended-ceiling rung (default: capture is on, requires --pprof on the gateway)")
	profileSec := fs.Int("profile-seconds", 20, "pprof CPU window length when capturing per-scenario profiles")
	adminTokenPath := fs.String("admin-token", "", "path to the gateway's admin-token file (default: bench/.run/nats/n1/admin-token)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: perf [flags]")
		fmt.Fprintln(fs.Output(), "Loads scenarios YAML, brings up stack + services, runs sweeps, renders docs/perf.md.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional args: %v", fs.Args())
	}

	cfg, err := loadPerfConfig(*configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> loaded %d scenario%s from %s\n", len(cfg.Scenarios), plural(len(cfg.Scenarios)), *configPath)

	if !*skipBringup {
		if err := ensureStackUp(); err != nil {
			return fmt.Errorf("bringup: %w", err)
		}
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	var failed []string
	for _, sc := range cfg.Scenarios {
		fmt.Fprintf(os.Stderr, "\n==> scenario %s (%s)\n", sc.Name, sc.Description)
		if !*skipServices {
			if err := ensureServicesRegistered(sc); err != nil {
				fmt.Fprintf(os.Stderr, "scenario %s: setup failed: %v\n", sc.Name, err)
				failed = append(failed, sc.Name)
				continue
			}
		}
		out := filepath.Join(*outDir, "sweep-"+sc.Name+".json")
		if err := dispatchSweep(sc, out); err != nil {
			fmt.Fprintf(os.Stderr, "scenario %s: sweep failed: %v\n", sc.Name, err)
			failed = append(failed, sc.Name)
			continue
		}
		// Reset the streaming-header flag between scenarios so the
		// next sweep prints its own column header.
		stepHeaderPrinted = false

		if !*noProfile {
			rps := profileRPSFor(out)
			if rps <= 0 {
				fmt.Fprintf(os.Stderr, "  scenario %s: skipping profile (no healthy rung)\n", sc.Name)
				continue
			}
			fmt.Fprintf(os.Stderr, "  scenario %s: capturing profile at %d RPS (%ds CPU window)\n", sc.Name, rps, *profileSec)
			tb, err := ensureTrafficBinary()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  scenario %s: profile skipped (traffic binary): %v\n", sc.Name, err)
				continue
			}
			capt := captureProfileForScenario(sc, rps, *profileSec, *outDir, tb, readAdminToken(*adminTokenPath))
			if err := attachProfileToSweep(out, capt); err != nil {
				fmt.Fprintf(os.Stderr, "  scenario %s: profile attach failed: %v\n", sc.Name, err)
			} else if capt.CaptureError != "" {
				fmt.Fprintf(os.Stderr, "  scenario %s: profile capture error: %s\n", sc.Name, capt.CaptureError)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("scenarios failed: %s", strings.Join(failed, ", "))
	}

	if *reportOut != "" {
		fmt.Fprintln(os.Stderr, "\n==> rendering report")
		if err := runReport([]string{"--in-dir", *outDir, "--out", *reportOut}); err != nil {
			return err
		}
	}
	return nil
}

// defaultConfigPath resolves the bundled scenarios YAML. Prefers the
// repo-relative path so a fresh checkout works without flags;
// operators in deeper trees can pass --config explicitly.
func defaultConfigPath() string {
	for _, p := range []string{
		"bench/perf-scenarios.yaml",
		"perf-scenarios.yaml",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "bench/perf-scenarios.yaml"
}

// ensureStackUp checks /api/health on the default gateway endpoint;
// if not reachable, runs `bin/bench up`. Idempotent — already-up
// stacks are detected and skipped.
func ensureStackUp() error {
	if stackResponsive() {
		fmt.Fprintln(os.Stderr, "==> stack already up — skipping bringup")
		return nil
	}
	fmt.Fprintln(os.Stderr, "==> bringing up bench stack (this takes ~10s)")
	cmd := exec.Command("bin/bench", "up")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bin/bench up: %w", err)
	}
	// `bench up` returns after compose comes online but the gateway
	// itself may still be starting up. Poll briefly.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if stackResponsive() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("gateway not responsive after bringup")
}

func stackResponsive() bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://localhost:18080/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// ensureServicesRegistered launches each required service via
// `bin/bench service add` and then polls /api/schema/graphql for
// the namespace to appear. Already-registered namespaces are
// skipped (idempotent across reruns).
func ensureServicesRegistered(sc perfScenario) error {
	for _, svc := range sc.Services {
		if isNamespaceRegistered(serviceKindToNamespace(svc.Kind)) {
			fmt.Fprintf(os.Stderr, "  service %s already registered, skipping\n", svc.Kind)
			continue
		}
		args := []string{"service", "add", svc.Kind}
		if svc.Delay != "" {
			args = append(args, "--delay", svc.Delay)
		}
		fmt.Fprintf(os.Stderr, "  bin/bench %s\n", strings.Join(args, " "))
		cmd := exec.Command("bin/bench", args...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("service add %s: %w", svc.Kind, err)
		}
		// Wait for the namespace to land in the schema.
		ns := serviceKindToNamespace(svc.Kind)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if isNamespaceRegistered(ns) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !isNamespaceRegistered(ns) {
			return fmt.Errorf("namespace %q not appearing in schema after service add %s", ns, svc.Kind)
		}
	}
	return nil
}

// serviceKindToNamespace maps a bench-managed kind to the GraphQL
// namespace it'll register under. Bench script knows greeter +
// hello-proto / hello-openapi / hello-graphql today.
func serviceKindToNamespace(kind string) string {
	switch kind {
	case "greeter":
		return "greeter"
	case "hello-proto":
		return "hello_proto"
	case "hello-openapi":
		return "hello_openapi"
	case "hello-graphql":
		return "hello_graphql"
	case "library":
		return "library"
	default:
		return kind
	}
}

// isNamespaceRegistered hits /api/schema/graphql and looks for the
// namespace field on Query. Cheap, no auth required.
func isNamespaceRegistered(ns string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:18080/api/schema/graphql")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	buf := bytes.Buffer{}
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return false
	}
	// Naive but sufficient: the namespace appears as a field in the
	// Query type's body, e.g. `  greeter: GreeterQueryNamespace`.
	return bytes.Contains(buf.Bytes(), []byte("  "+ns+":"))
}

// dispatchSweep translates a YAML scenario into runSweep args and
// invokes it. Centralises the YAML → CLI mapping in one place.
func dispatchSweep(sc perfScenario, out string) error {
	stepsCSV := make([]string, len(sc.Sweep.Steps))
	for i, n := range sc.Sweep.Steps {
		stepsCSV[i] = fmt.Sprintf("%d", n)
	}
	args := []string{
		"--scenario", sc.Name,
		"--target", sc.Target,
		"--query", sc.Query,
		"--steps", strings.Join(stepsCSV, ","),
		"--reps", fmt.Sprintf("%d", sc.Sweep.Reps),
		"--duration", sc.Sweep.Duration,
		"--out", out,
	}
	if sc.Sweep.NoWarmup {
		args = append(args, "--no-warmup")
	}
	if sc.Sweep.NoKnee {
		args = append(args, "--no-knee")
	}
	if sc.UpstreamLatency != "" {
		args = append(args, "--upstream-latency", sc.UpstreamLatency)
	}
	return runSweep(args)
}
