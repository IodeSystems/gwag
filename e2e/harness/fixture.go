//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Fixture kinds.
const (
	KindProto   = "proto"
	KindOpenAPI = "openapi"
	KindGraphQL = "graphql"
)

// FixtureOpts configures one fixture process (e2e/fixture). Each field
// maps to the fixture flag named in its comment.
type FixtureOpts struct {
	Kind      string   // --kind: KindProto (default), KindOpenAPI, KindGraphQL
	Namespace string   // --namespace (required)
	Versions  []string // --versions; default ["v1"]
	// Depends are SERVICE:LINE[,LINE...] entries, e.g. "library:v1,v2"
	// (--depends, repeated).
	Depends []string
	Owner   string // --owner
	// Instance is the instance id and process name (--instance).
	// Default <namespace>-<n>, n counting fixtures in the Env.
	Instance string

	Latency        time.Duration // --latency
	ErrorRate      float64       // --error-rate
	Hang           bool          // --hang
	CrashAfter     int           // --crash-after
	DrainOnSIGTERM bool          // --drain-on-sigterm
	DrainTimeout   time.Duration // --drain-timeout
	TTL            time.Duration // --ttl
	Verify         bool          // --verify: HealthURL / SpecURL on the control listener
	MaxConcurrency int           // --max-concurrency
	// Proto maps a line to a .proto path registered instead of
	// fixture.proto for that line (--proto LINE=PATH).
	Proto map[string]string
	// Args are extra fixture flags.
	Args []string
	// FailoverGateways are more control planes the fixture moves its
	// registration to when its gateway stops answering
	// (--gateway-alt, repeated; controlclient GatewayAddrs).
	FailoverGateways []*Gateway

	// NoWait returns as soon as the process starts, without waiting
	// for "FIXTURE ready" (which a fixture with unmet Depends never
	// prints) or for the gateway to list the service.
	NoWait bool
}

// Fixture is a running fixture process.
type Fixture struct {
	Opts        FixtureOpts
	Gateway     *Gateway // the gateway it registered with
	Addr        string   // service listen address, host:port
	ControlAddr string   // control listener, host:port
	Proc        *Proc

	env *Env
}

// Fixture starts a fixture that registers with gw. Unless NoWait, it
// waits for the fixture to report ready and for gw to list a ready
// instance on every version.
func (e *Env) Fixture(gw *Gateway, opts FixtureOpts) *Fixture {
	e.t.Helper()
	if opts.Namespace == "" {
		e.t.Fatalf("harness: FixtureOpts.Namespace is required")
	}
	if opts.Kind == "" {
		opts.Kind = KindProto
	}
	if len(opts.Versions) == 0 {
		opts.Versions = []string{"v1"}
	}
	if opts.Instance == "" {
		e.mu.Lock()
		n := 1
		for _, p := range e.procs {
			if strings.HasPrefix(p.Name, opts.Namespace+"-") {
				n++
			}
		}
		e.mu.Unlock()
		opts.Instance = fmt.Sprintf("%s-%d", opts.Namespace, n)
	}
	f := &Fixture{
		Opts:        opts,
		Gateway:     gw,
		Addr:        fmt.Sprintf("127.0.0.1:%d", FreePort(e.t)),
		ControlAddr: fmt.Sprintf("127.0.0.1:%d", FreePort(e.t)),
		env:         e,
	}
	args := []string{
		"--kind", opts.Kind,
		"--gateway", gw.CPAddr(),
		"--namespace", opts.Namespace,
		"--versions", strings.Join(opts.Versions, ","),
		"--instance", opts.Instance,
		"--addr", f.Addr,
		"--control", f.ControlAddr,
	}
	for _, d := range opts.Depends {
		args = append(args, "--depends", d)
	}
	if opts.Owner != "" {
		args = append(args, "--owner", opts.Owner)
	}
	if opts.Latency > 0 {
		args = append(args, "--latency", opts.Latency.String())
	}
	if opts.ErrorRate > 0 {
		args = append(args, "--error-rate", strconv.FormatFloat(opts.ErrorRate, 'f', -1, 64))
	}
	if opts.Hang {
		args = append(args, "--hang")
	}
	if opts.CrashAfter > 0 {
		args = append(args, "--crash-after", strconv.Itoa(opts.CrashAfter))
	}
	if opts.DrainOnSIGTERM {
		args = append(args, "--drain-on-sigterm")
	}
	if opts.DrainTimeout > 0 {
		args = append(args, "--drain-timeout", opts.DrainTimeout.String())
	}
	if opts.TTL > 0 {
		args = append(args, "--ttl", opts.TTL.String())
	}
	if opts.Verify {
		args = append(args, "--verify")
	}
	if opts.MaxConcurrency > 0 {
		args = append(args, "--max-concurrency", strconv.Itoa(opts.MaxConcurrency))
	}
	for line, path := range opts.Proto {
		args = append(args, "--proto", line+"="+path)
	}
	for _, alt := range opts.FailoverGateways {
		args = append(args, "--gateway-alt", alt.CPAddr())
	}
	args = append(args, opts.Args...)
	f.Proc = e.Start(opts.Instance, Binary(e.t, "fixture"), args...)
	if !opts.NoWait {
		f.WaitReady()
	}
	return f
}

// WaitReady waits for the current incarnation to log "FIXTURE ready"
// and for the gateway to list this instance as ready on every line.
func (f *Fixture) WaitReady() {
	t := f.env.t
	t.Helper()
	f.Proc.WaitLog("FIXTURE ready", 30*time.Second)
	for _, v := range f.Opts.Versions {
		f.Gateway.Admin().WaitInstance(f.Opts.Namespace, v, f.Opts.Instance, "ready", 30*time.Second)
	}
}

// Restart restarts the fixture (SIGTERM first) on the same ports and
// waits until it is ready again, unless NoWait.
func (f *Fixture) Restart() {
	f.env.t.Helper()
	f.Proc.Restart()
	if !f.Opts.NoWait {
		f.WaitReady()
	}
}

// Faults is the fixture's live fault set (POST /faults on the control
// listener replaces all of it).
type Faults struct {
	LatencyMs  int64   `json:"latencyMs"`
	ErrorRate  float64 `json:"errorRate"`
	Hang       bool    `json:"hang"`
	CrashAfter int64   `json:"crashAfter"`
	Unhealthy  bool    `json:"unhealthy"` // GET /healthz answers 500 (--verify)
}

// FixtureStats is GET /stats from the control listener.
type FixtureStats struct {
	Calls  int64  `json:"calls"`
	Errors int64  `json:"errors"`
	Faults Faults `json:"faults"`
}

// SetFaults replaces the fixture's fault set at runtime.
func (f *Fixture) SetFaults(fl Faults) {
	t := f.env.t
	t.Helper()
	b, _ := json.Marshal(fl)
	resp, err := http.Post("http://"+f.ControlAddr+"/faults", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("harness: %s set faults: %v", f.Opts.Instance, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("harness: %s set faults: HTTP %d", f.Opts.Instance, resp.StatusCode)
	}
}

// Stats returns the fixture's call counters.
func (f *Fixture) Stats() FixtureStats {
	t := f.env.t
	t.Helper()
	var s FixtureStats
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+f.ControlAddr+"/stats", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("harness: %s stats: %v", f.Opts.Instance, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("harness: %s stats: %v", f.Opts.Instance, err)
	}
	return s
}

// ProtoPackage is the proto package the fixture registers for line:
// fixture.<namespace>.<line>, non-identifier characters as "_".
func ProtoPackage(namespace, line string) string {
	clean := func(s string) string {
		b := []byte(s)
		for i, c := range b {
			if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				b[i] = '_'
			}
		}
		return string(b)
	}
	return "fixture." + clean(namespace) + "." + clean(line)
}

// EchoMethod is the fully qualified gRPC method of the proto fixture's
// Echo for line, e.g. /fixture.orders.v1.FixtureService/Echo. It is
// also the method label of proto dispatch metrics.
func EchoMethod(namespace, line string) string {
	return "/" + ProtoPackage(namespace, line) + ".FixtureService/Echo"
}

// MetricMethod is the method label the gateway puts on dispatch
// metrics for a call to this fixture's Echo on line.
func (f *Fixture) MetricMethod(line string) string {
	switch f.Opts.Kind {
	case KindOpenAPI:
		return "POST /" + f.Opts.Namespace + "/" + line + "/echo"
	case KindGraphQL:
		return "query echo"
	default:
		return EchoMethod(f.Opts.Namespace, line)
	}
}
