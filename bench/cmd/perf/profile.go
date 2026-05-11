package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProfileCapture is the per-scenario profile attached to a Sweep
// when --profile is set on `perf run` (or its YAML/default
// equivalent). Two pprof files land on disk under
// <out-dir>/profile-<scenario>.{cpu,allocs}.pprof; the summary
// blocks parsed below are embedded in the sweep JSON so the
// rendered docs/perf.md can show "where the hot path lives" per
// backend without the reader running pprof themselves.
type ProfileCapture struct {
	// CapturedAtRPS is the target RPS the profile was taken at —
	// typically the recommended-ceiling rung. Sub-ceiling profiles
	// don't surface saturation; over-ceiling profiles surface
	// queueing artifacts that mask the steady-state pattern.
	CapturedAtRPS int `json:"captured_at_rps"`
	// DurationSec is the pprof window length. CPU profiles are
	// duration-bounded; allocs is a since-start snapshot taken at
	// the end of the window.
	DurationSec float64 `json:"duration_seconds"`
	// CPUFile / AllocsFile are the on-disk paths relative to
	// out-dir. Absent when capture failed (e.g. /debug/pprof not
	// reachable) — sweep still ships.
	CPUFile    string `json:"cpu_file,omitempty"`
	AllocsFile string `json:"allocs_file,omitempty"`
	// TopCPU / TopAllocs are pre-rendered "go tool pprof -top
	// -cum -text" blocks (top 10 entries each) so the report can
	// embed them directly.
	TopCPU    string `json:"top_cpu,omitempty"`
	TopAllocs string `json:"top_allocs,omitempty"`
	// CaptureError carries the failure reason when pprof endpoints
	// were unreachable / unauthorized; empty on success.
	CaptureError string `json:"capture_error,omitempty"`
}

// captureProfileForScenario hits /debug/pprof on the gateway and
// pulls CPU + allocs profiles during a synthetic load at the
// chosen RPS. The bench traffic binary drives the load; the perf
// driver coordinates the timing and pprof curls.
//
// adminToken is required because /debug/pprof is gated by
// AdminMiddleware on the bench gateway (--pprof flag).
//
// Returns a *ProfileCapture with the on-disk file paths and the
// pre-rendered top-10 summary blocks. On any non-fatal failure
// (pprof endpoints down, traffic binary missing) the returned
// capture has CaptureError populated and the sweep continues —
// profile data is best-effort polish.
func captureProfileForScenario(sc perfScenario, rps int, durSec int, outDir string, trafficBin, adminToken string) *ProfileCapture {
	cap := &ProfileCapture{
		CapturedAtRPS: rps,
		DurationSec:   float64(durSec),
	}
	if adminToken == "" {
		cap.CaptureError = "no admin token — gateway pprof endpoint requires it; pass via /api/admin or run `bench up` so the token persists to bench/.run/nats/n1/admin-token"
		return cap
	}
	if trafficBin == "" {
		cap.CaptureError = "traffic binary path missing — perf driver should resolve it before calling capture"
		return cap
	}

	gatewayBase := gatewayBaseFromTarget(sc.Target)
	if gatewayBase == "" {
		cap.CaptureError = fmt.Sprintf("could not derive gateway base from target %q", sc.Target)
		return cap
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(durSec+15)*time.Second)
	defer cancel()

	// Kick off traffic in the background; the run is durSec + a few
	// seconds of warm-up so the pprof window overlaps the steady-
	// state portion, not the ramp.
	repPath := filepath.Join(outDir, fmt.Sprintf("profile-%s.traffic.json", sc.Name))
	traffic := exec.CommandContext(ctx, trafficBin,
		"graphql",
		"--rps", strconv.Itoa(rps),
		"--duration", fmt.Sprintf("%ds", durSec+5),
		"--target", sc.Target,
		"--query", sc.Query,
		"--json", repPath,
		"--server-metrics=false",
	)
	traffic.Stdout = io.Discard
	traffic.Stderr = io.Discard
	if err := traffic.Start(); err != nil {
		cap.CaptureError = fmt.Sprintf("start traffic: %v", err)
		return cap
	}
	defer func() { _ = traffic.Wait() }()

	// Warm-up gap so the pprof window catches steady-state.
	time.Sleep(3 * time.Second)

	cpuPath := filepath.Join(outDir, fmt.Sprintf("profile-%s.cpu.pprof", sc.Name))
	allocsPath := filepath.Join(outDir, fmt.Sprintf("profile-%s.allocs.pprof", sc.Name))

	cpuErrCh := make(chan error, 1)
	allocsErrCh := make(chan error, 1)
	go func() {
		cpuErrCh <- fetchPprof(ctx, gatewayBase+fmt.Sprintf("/debug/pprof/profile?seconds=%d", durSec), adminToken, cpuPath)
	}()
	go func() {
		// Allocs is cumulative-since-start; pull at end of CPU
		// window so it covers the same wall clock.
		time.Sleep(time.Duration(durSec) * time.Second)
		allocsErrCh <- fetchPprof(ctx, gatewayBase+"/debug/pprof/allocs", adminToken, allocsPath)
	}()

	cpuErr := <-cpuErrCh
	allocsErr := <-allocsErrCh
	if cpuErr != nil {
		cap.CaptureError = fmt.Sprintf("cpu pprof: %v", cpuErr)
		return cap
	}
	if allocsErr != nil {
		cap.CaptureError = fmt.Sprintf("allocs pprof: %v", allocsErr)
		return cap
	}

	cap.CPUFile = filepath.Base(cpuPath)
	cap.AllocsFile = filepath.Base(allocsPath)
	cap.TopCPU = pprofTopText(cpuPath, "", 10)
	cap.TopAllocs = pprofTopText(allocsPath, "alloc_space", 10)
	return cap
}

// fetchPprof downloads a pprof endpoint to dst, gated by the admin
// bearer. The endpoint is opaque to this function (cpu, allocs,
// goroutine, etc) — caller picks; we just transfer bytes.
func fetchPprof(ctx context.Context, url, token, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// pprofTopText shells to `go tool pprof -top -cum -text` and
// returns the top N entries. sampleIndex is empty for CPU, or
// e.g. "alloc_space" for allocs profile.
func pprofTopText(path, sampleIndex string, n int) string {
	args := []string{"tool", "pprof"}
	if sampleIndex != "" {
		args = append(args, "-sample_index="+sampleIndex)
	}
	// -top is already a text output format; adding -text errors out
	// ("must set at most one output format"). -cum sorts by cumulative.
	args = append(args, "-top", "-cum", path)
	cmd := exec.Command("go", args...)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("(pprof failed: %v)", err)
	}
	// Trim to top N entries — pprof's default output starts with a
	// header block, then the entry table. Keep the header + N rows.
	lines := strings.Split(string(out), "\n")
	var keep []string
	header := true
	rows := 0
	for _, ln := range lines {
		if header {
			keep = append(keep, ln)
			if strings.HasPrefix(strings.TrimSpace(ln), "flat  flat%") {
				header = false
			}
			continue
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		keep = append(keep, ln)
		rows++
		if rows >= n {
			break
		}
	}
	return strings.Join(keep, "\n")
}

// gatewayBaseFromTarget strips the /api/... suffix from the
// scenario's target URL, leaving scheme + host (e.g.
// http://localhost:18080). Used to construct /debug/pprof paths
// without assuming a fixed ingress prefix.
func gatewayBaseFromTarget(target string) string {
	if target == "" {
		return ""
	}
	// crude but sufficient: cut at the first "/api/" segment, fall
	// back to the host portion if no /api/ found.
	if i := strings.Index(target, "/api/"); i >= 0 {
		return target[:i]
	}
	// Drop trailing path components after the host.
	if i := strings.Index(target, "://"); i >= 0 {
		rest := target[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return target[:i+3+j]
		}
	}
	return target
}

// readAdminToken pulls the admin token from the bench stack's
// well-known path. Returns "" when the file isn't readable; the
// caller surfaces this as a soft capture error.
func readAdminToken(p string) string {
	if p == "" {
		p = "bench/.run/nats/n1/admin-token"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// profileRPSFor reads a freshly-written sweep JSON and returns the
// recommended-ceiling RPS to profile at: the knee's KneeRPS when a
// knee was detected, otherwise the last step's target (the sweep
// ran clean past every rung). Zero means "skip the profile" — the
// sweep produced no rungs, or all rungs failed.
func profileRPSFor(sweepPath string) int {
	raw, err := os.ReadFile(sweepPath)
	if err != nil {
		return 0
	}
	var sw struct {
		Steps []struct {
			TargetRPS int `json:"target_rps"`
		} `json:"steps"`
		Knee *struct {
			KneeRPS int `json:"knee_rps"`
		} `json:"knee"`
	}
	if err := json.Unmarshal(raw, &sw); err != nil {
		return 0
	}
	if sw.Knee != nil && sw.Knee.KneeRPS > 0 {
		return sw.Knee.KneeRPS
	}
	if len(sw.Steps) > 0 {
		return sw.Steps[len(sw.Steps)-1].TargetRPS
	}
	return 0
}

// attachProfileToSweep updates an on-disk Sweep JSON file in place,
// adding the captured ProfileCapture under .profile. Idempotent —
// re-runs overwrite the previous capture for the same scenario.
func attachProfileToSweep(sweepPath string, capt *ProfileCapture) error {
	raw, err := os.ReadFile(sweepPath)
	if err != nil {
		return err
	}
	// Decode into a map so we don't depend on the full Sweep struct
	// having a Profile field; perf report.go reads the .profile key
	// independently.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["profile"] = capt
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sweepPath, out, 0o644)
}
