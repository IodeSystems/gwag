// compare — head-to-head perf comparison orchestrator.
//
// Reads perf/competitors.yaml, boots the hello-* upstream backends
// once, then for each enabled gateway: starts it, waits for /health,
// runs the configured sweep at every scenario the gateway supports,
// captures the JSON sidecar, stops the gateway, moves on.
//
// All sweeps share the same bench/cmd/traffic binary so the gateway-
// side numbers are directly comparable.
//
// Final output: perf/.out/comparison.md — markdown matrix of each
// gateway × scenario, sweep table per row, with the per-scenario
// recommended-ceiling RPS + p99 + gateway-self-time for the headline
// table at the top.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

type config struct {
	Backends  []backendCfg  `yaml:"backends"`
	Sweep     sweepCfg      `yaml:"sweep"`
	Scenarios []scenarioCfg `yaml:"scenarios"`
	Gateways  []gatewayCfg  `yaml:"gateways"`
}

type backendCfg struct {
	Kind string `yaml:"kind"`
	Addr string `yaml:"addr"`
}

type sweepCfg struct {
	Steps    []int  `yaml:"steps"`
	Reps     int    `yaml:"reps"`
	Duration string `yaml:"duration"`
}

type scenarioCfg struct {
	Name        string   `yaml:"name"`
	Query       string   `yaml:"query"`
	SupportedBy []string `yaml:"supported_by"`
}

type gatewayCfg struct {
	Name           string            `yaml:"name"`
	Description    string            `yaml:"description"`
	TargetTemplate string            `yaml:"target_template"`
	QueryOverrides map[string]string `yaml:"query_overrides,omitempty"`
	Enabled        *bool             `yaml:"enabled,omitempty"`
}

func (g gatewayCfg) enabled() bool {
	if g.Enabled == nil {
		return true
	}
	return *g.Enabled
}

func main() {
	configPath := flag.String("config", "perf/competitors.yaml", "competitors YAML")
	outDir := flag.String("out", "perf/.out", "output directory for per-gateway JSON + final comparison.md")
	repoRoot := flag.String("repo", ".", "path to repo root (so we can find bench/cmd/traffic + start scripts)")
	skipBackends := flag.Bool("skip-backends", false, "assume backends already running (debug)")
	only := flag.String("only", "", "comma-separated gateway names to run; default = all enabled")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		die("load config: %v", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		die("mkdir out: %v", err)
	}

	trafficBin, err := ensureTrafficBinary(*repoRoot)
	if err != nil {
		die("build traffic: %v", err)
	}

	if !*skipBackends {
		fmt.Fprintln(os.Stderr, "==> starting backends")
		if err := startBackends(*repoRoot, cfg.Backends); err != nil {
			die("backends: %v", err)
		}
	}

	picked := pickGateways(cfg.Gateways, *only)
	results := make(map[string]*gatewayResults, len(picked))
	for _, gw := range picked {
		fmt.Fprintf(os.Stderr, "\n==> gateway %s: %s\n", gw.Name, gw.Description)
		res, err := runGateway(*repoRoot, *outDir, trafficBin, gw, cfg.Scenarios, cfg.Sweep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  gateway %s failed: %v\n", gw.Name, err)
			res = &gatewayResults{Name: gw.Name, Failure: err.Error()}
		}
		results[gw.Name] = res
	}

	if err := renderComparison(*outDir, cfg, picked, results); err != nil {
		die("render: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\n==> wrote %s\n", filepath.Join(*outDir, "comparison.md"))
}

// gatewayResults captures one gateway's full sweep set — one entry
// per scenario it supports. The renderer merges across gateways.
type gatewayResults struct {
	Name      string                       `json:"name"`
	Scenarios map[string]*scenarioOutcome `json:"scenarios"`
	Failure   string                       `json:"failure,omitempty"`
}

type scenarioOutcome struct {
	Scenario          string             `json:"scenario"`
	SweepPath         string             `json:"sweep_path"`
	RecommendedRPS    int                `json:"recommended_rps"`
	AchievedAtCeiling float64            `json:"achieved_at_ceiling"`
	P99AtCeilingUs    int64              `json:"p99_at_ceiling_us"`
	SelfMeanUsAtCeil  int64              `json:"self_mean_us_at_ceiling"`
}

func loadConfig(p string) (*config, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if len(c.Backends) == 0 || len(c.Scenarios) == 0 || len(c.Gateways) == 0 {
		return nil, errors.New("config: backends/scenarios/gateways required")
	}
	return &c, nil
}

func pickGateways(all []gatewayCfg, only string) []gatewayCfg {
	if only == "" {
		var out []gatewayCfg
		for _, g := range all {
			if g.enabled() {
				out = append(out, g)
			}
		}
		return out
	}
	set := map[string]bool{}
	for _, name := range strings.Split(only, ",") {
		set[strings.TrimSpace(name)] = true
	}
	var out []gatewayCfg
	for _, g := range all {
		if set[g.Name] {
			out = append(out, g)
		}
	}
	return out
}

// ensureTrafficBinary builds bench/cmd/traffic on demand so the
// comparison reuses the gateway's own load driver — apples-to-apples
// across every peer.
func ensureTrafficBinary(repoRoot string) (string, error) {
	bin := filepath.Join(repoRoot, "bench", ".run", "bin", "traffic")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", bin, "./bench/cmd/traffic")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build traffic: %w", err)
	}
	return bin, nil
}

// startBackends launches each hello-* binary in the background and
// leaves them running for the rest of the process. Best-effort:
// already-bound ports are silently skipped (assume something else is
// already serving the same kind).
func startBackends(repoRoot string, backends []backendCfg) error {
	for _, b := range backends {
		bin := filepath.Join(repoRoot, "bench", ".run", "bin", b.Kind)
		if _, err := os.Stat(bin); err != nil {
			// Fallback: build the binary on demand.
			fmt.Fprintf(os.Stderr, "  building %s\n", b.Kind)
			build := exec.Command("go", "build", "-o", bin, "./examples/multi/cmd/"+b.Kind)
			build.Dir = repoRoot
			build.Stdout = os.Stderr
			build.Stderr = os.Stderr
			if err := build.Run(); err != nil {
				return fmt.Errorf("build %s: %w", b.Kind, err)
			}
		}
		port := strings.TrimPrefix(b.Addr, ":")
		if listening(port) {
			fmt.Fprintf(os.Stderr, "  %s already listening on %s — skipping\n", b.Kind, b.Addr)
			continue
		}
		args := backendArgs(b)
		fmt.Fprintf(os.Stderr, "  starting %s %v\n", b.Kind, args)
		cmd := exec.Command(bin, args...)
		cmd.Dir = filepath.Join(repoRoot, "examples", "multi")
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", b.Kind, err)
		}
		// Don't wait — backends are long-running for the whole
		// duration of the compare run. Release the goroutine.
		go cmd.Wait()
	}
	// Smoke-wait: every backend's listening port has to be hot before
	// gateways can introspect them.
	deadline := time.Now().Add(15 * time.Second)
	for _, b := range backends {
		port := strings.TrimPrefix(b.Addr, ":")
		for time.Now().Before(deadline) {
			if listening(port) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !listening(port) {
			return fmt.Errorf("backend %s never listened on %s", b.Kind, b.Addr)
		}
	}
	return nil
}

// backendArgs builds the kind-specific flag set. Mirrors the bench
// script's per-kind launcher so the backends behave identically
// here and there.
func backendArgs(b backendCfg) []string {
	switch b.Kind {
	case "hello-proto":
		return []string{"--addr", b.Addr, "--advertise", "localhost" + b.Addr}
	case "hello-openapi":
		return []string{"--addr", b.Addr, "--advertise", "http://localhost" + b.Addr}
	case "hello-graphql":
		return []string{"--addr", b.Addr, "--advertise", "http://localhost" + b.Addr + "/graphql"}
	default:
		return []string{"--addr", b.Addr}
	}
}

func listening(port string) bool {
	c, err := http.NewRequest("GET", "http://localhost:"+port, nil)
	if err != nil {
		return false
	}
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Do(c)
	if err == nil {
		_ = resp.Body.Close()
		return true
	}
	return strings.Contains(err.Error(), "connection reset") ||
		strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "non-HTTP") // gRPC ports refuse plain HTTP
}

// runGateway is the per-gateway entry point. It invokes the gateway-
// specific start script (perf/scripts/start-<name>.sh), waits for the
// target endpoint to respond, runs the sweep, then runs the stop
// script (start-<name>.sh stop) before returning.
func runGateway(repoRoot, outDir, trafficBin string, gw gatewayCfg, scenarios []scenarioCfg, sw sweepCfg) (*gatewayResults, error) {
	startScript := filepath.Join(repoRoot, "perf", "scripts", "start-"+gw.Name+".sh")
	if _, err := os.Stat(startScript); err != nil {
		return nil, fmt.Errorf("no start script at %s", startScript)
	}
	fmt.Fprintf(os.Stderr, "  start: %s\n", startScript)
	start := exec.Command(startScript, "start")
	start.Dir = repoRoot
	start.Stdout = os.Stderr
	start.Stderr = os.Stderr
	if err := start.Run(); err != nil {
		return nil, fmt.Errorf("start gateway: %w", err)
	}
	defer func() {
		stop := exec.Command(startScript, "stop")
		stop.Dir = repoRoot
		stop.Stdout = os.Stderr
		stop.Stderr = os.Stderr
		_ = stop.Run()
	}()

	// Wait for target to respond. Gateways take 2-15s to boot
	// depending on stack (gwag fast, mesh slow because Node).
	if err := waitForGateway(gw.TargetTemplate, 30*time.Second); err != nil {
		return nil, fmt.Errorf("gateway never became ready: %w", err)
	}

	res := &gatewayResults{Name: gw.Name, Scenarios: map[string]*scenarioOutcome{}}
	for _, sc := range scenarios {
		if !contains(sc.SupportedBy, gw.Name) {
			continue
		}
		query := sc.Query
		if override, ok := gw.QueryOverrides[sc.Name]; ok {
			query = override
		}
		out := filepath.Join(outDir, fmt.Sprintf("sweep-%s-%s.json", gw.Name, sc.Name))
		fmt.Fprintf(os.Stderr, "  scenario %s\n", sc.Name)
		if err := runSweep(trafficBin, gw.TargetTemplate, query, sw, out); err != nil {
			return nil, fmt.Errorf("sweep %s: %w", sc.Name, err)
		}
		so, err := summariseSweep(out, sc.Name)
		if err != nil {
			return nil, fmt.Errorf("summarise %s: %w", sc.Name, err)
		}
		res.Scenarios[sc.Name] = so
	}
	return res, nil
}

// waitForGateway polls the gateway's target endpoint until it
// responds to a trivial GraphQL query, or the deadline elapses.
func waitForGateway(target string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(`{"query":"{__typename}"}`))
		req.Header.Set("Content-Type", "application/json")
		client := http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// runSweep drives the same escalating-RPS sweep used by `bench perf
// run`, capturing each rep's --json sidecar into rps-<step>-rep-N.json
// alongside the aggregate output file.
func runSweep(trafficBin, target, query string, sw sweepCfg, out string) error {
	repsDir := strings.TrimSuffix(out, ".json") + ".reps"
	if err := os.MkdirAll(repsDir, 0o755); err != nil {
		return err
	}
	type stepStats struct {
		TargetRPS              int     `json:"target_rps"`
		AchievedRPSMean        float64 `json:"achieved_rps_mean"`
		ClientP99UsMedian      int64   `json:"client_p99_us_median"`
		GatewaySelfMeanUs      int64   `json:"gateway_self_mean_us_median"`
	}
	type sweepOut struct {
		Schema string      `json:"schema"`
		Steps  []stepStats `json:"steps"`
	}
	all := sweepOut{Schema: "perf-compare/v1"}
	for _, rps := range sw.Steps {
		// Three reps; rep 1 warm-up discarded, reps 2+3 medianed
		// (mean for achieved_rps).
		var (
			achieved  []float64
			p99us     []int64
			selfMeanUs []int64
		)
		for r := 1; r <= sw.Reps; r++ {
			repPath := filepath.Join(repsDir, fmt.Sprintf("rps-%d-rep-%d.json", rps, r))
			args := []string{
				"graphql",
				"--rps", strconv.Itoa(rps),
				"--duration", sw.Duration,
				"--target", target,
				"--query", query,
				"--json", repPath,
				"--server-metrics=true",
			}
			cmd := exec.Command(trafficBin, args...)
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("rep %d/%d (rps=%d): %w", r, sw.Reps, rps, err)
			}
			if r == 1 && sw.Reps > 1 {
				continue // warm-up
			}
			raw, err := os.ReadFile(repPath)
			if err != nil {
				return err
			}
			td, err := parseRep(raw)
			if err != nil {
				return err
			}
			achieved = append(achieved, td.achievedRPS)
			p99us = append(p99us, td.p99us)
			selfMeanUs = append(selfMeanUs, td.selfMeanUs)
		}
		all.Steps = append(all.Steps, stepStats{
			TargetRPS:         rps,
			AchievedRPSMean:   meanFloat(achieved),
			ClientP99UsMedian: medianInt64(p99us),
			GatewaySelfMeanUs: medianInt64(selfMeanUs),
		})
		// Stop the sweep early if we've crossed the latency SLA — same
		// 50ms p99 ceiling the gwag-side report uses.
		last := all.Steps[len(all.Steps)-1]
		if last.ClientP99UsMedian > 50_000 {
			break
		}
	}
	enc, err := os.Create(out)
	if err != nil {
		return err
	}
	defer enc.Close()
	e := json.NewEncoder(enc)
	e.SetIndent("", "  ")
	return e.Encode(all)
}

type repSummary struct {
	achievedRPS float64
	p99us       int64
	selfMeanUs  int64
}

func parseRep(raw []byte) (repSummary, error) {
	var v struct {
		Targets []struct {
			AchievedRPS float64 `json:"achieved_rps"`
			P99Us       int64   `json:"p99_us"`
		} `json:"targets"`
		Gateway *struct {
			Ingress map[string]struct {
				SelfMeanUs int64 `json:"self_mean_us"`
			} `json:"ingress"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return repSummary{}, err
	}
	if len(v.Targets) == 0 {
		return repSummary{}, errors.New("no targets")
	}
	r := repSummary{
		achievedRPS: v.Targets[0].AchievedRPS,
		p99us:       v.Targets[0].P99Us,
	}
	if v.Gateway != nil {
		// Mean of per-ingress self-time, weighted equal (single ingress
		// is the common case for this benchmark).
		var sum, n int64
		for _, ing := range v.Gateway.Ingress {
			sum += ing.SelfMeanUs
			n++
		}
		if n > 0 {
			r.selfMeanUs = sum / n
		}
	}
	return r, nil
}

func summariseSweep(path, name string) (*scenarioOutcome, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v struct {
		Steps []struct {
			TargetRPS         int     `json:"target_rps"`
			AchievedRPSMean   float64 `json:"achieved_rps_mean"`
			ClientP99UsMedian int64   `json:"client_p99_us_median"`
			GatewaySelfMeanUs int64   `json:"gateway_self_mean_us_median"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out := &scenarioOutcome{Scenario: name, SweepPath: filepath.Base(path)}
	// Recommended ceiling = last step whose p99 stayed under 50ms.
	for _, s := range v.Steps {
		if s.ClientP99UsMedian > 50_000 {
			break
		}
		out.RecommendedRPS = s.TargetRPS
		out.AchievedAtCeiling = s.AchievedRPSMean
		out.P99AtCeilingUs = s.ClientP99UsMedian
		out.SelfMeanUsAtCeil = s.GatewaySelfMeanUs
	}
	return out, nil
}

func renderComparison(outDir string, cfg *config, picked []gatewayCfg, results map[string]*gatewayResults) error {
	var b strings.Builder
	b.WriteString("# Perf comparison — gwag vs peers\n\n")
	fmt.Fprintf(&b, "_Generated %s. Run via `docker run gwag-perf` or `perf/run.sh local`._\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("Each gateway runs the same `bench/cmd/traffic` sweep against the same `hello-*` backends on the same host (serial; no concurrent gateways). Knee = highest rung where p99 stays under 50ms.\n\n")

	// Headline matrix table: rows = scenarios, columns = gateways.
	b.WriteString("## Headline matrix\n\n")
	b.WriteString("| Scenario | ")
	for _, gw := range picked {
		b.WriteString(gw.Name)
		b.WriteString(" | ")
	}
	b.WriteString("\n|---|")
	for range picked {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, sc := range cfg.Scenarios {
		fmt.Fprintf(&b, "| **%s** | ", sc.Name)
		for _, gw := range picked {
			r := results[gw.Name]
			if r == nil || r.Failure != "" {
				b.WriteString("— | ")
				continue
			}
			so := r.Scenarios[sc.Name]
			if so == nil {
				b.WriteString("not supported | ")
				continue
			}
			fmt.Fprintf(&b, "%d RPS @ p99 %.1fms | ", so.RecommendedRPS, float64(so.P99AtCeilingUs)/1000)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Per-gateway sections.
	for _, gw := range picked {
		r := results[gw.Name]
		fmt.Fprintf(&b, "## %s\n\n", gw.Name)
		fmt.Fprintf(&b, "%s\n\n", gw.Description)
		if r == nil || r.Failure != "" {
			msg := "did not run"
			if r != nil {
				msg = r.Failure
			}
			fmt.Fprintf(&b, "_Skipped: %s_\n\n", msg)
			continue
		}
		b.WriteString("| Scenario | Ceiling RPS | Achieved | p99 @ ceiling | Gateway self-time |\n")
		b.WriteString("|---|---:|---:|---:|---:|\n")
		for _, sc := range cfg.Scenarios {
			so := r.Scenarios[sc.Name]
			if so == nil {
				continue
			}
			fmt.Fprintf(&b, "| %s | %d | %.0f | %.1fms | %dµs |\n",
				sc.Name, so.RecommendedRPS, so.AchievedAtCeiling, float64(so.P99AtCeilingUs)/1000, so.SelfMeanUsAtCeil)
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outDir, "comparison.md"), []byte(b.String()), 0o644)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func medianInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	// Simple sort for tiny slices — Reps is always ≤ a handful.
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}
