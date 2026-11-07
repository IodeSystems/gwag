package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Service-lifecycle metrics (lifecycle_metrics.go), asserted on the
// real /metrics output.

const lcPrefix = "go_api_gateway_"

func scrapeMetrics(t *testing.T, g *Gateway) string {
	t.Helper()
	rec := httptest.NewRecorder()
	g.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	return rec.Body.String()
}

// metricVal returns the value of the series `name` whose label set
// contains every k=v pair, and whether it exists.
func metricVal(body, name string, kv ...string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if !strings.Contains(line, fmt.Sprintf(`%s=%q`, kv[i], kv[i+1])) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		if err != nil {
			continue
		}
		return f, true
	}
	return 0, false
}

func wantMetric(t *testing.T, g *Gateway, want float64, name string, kv ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got float64
	var found bool
	for {
		got, found = metricVal(scrapeMetrics(t, g), lcPrefix+name, kv...)
		if found && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s%v = %v (found=%v), want %v\n%s", name, kv, got, found, want, scrapeMetrics(t, g))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func wantNoSeries(t *testing.T, g *Gateway, name string, kv ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := scrapeMetrics(t, g)
		if _, found := metricVal(body, lcPrefix+name, kv...); !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale series %s%v still exported\n%s", name, kv, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLifecycleMetrics_StartGate(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutBackpressure())
	t.Cleanup(g.Close)

	gr := mustRegister(t, g, addr, dependsOn(greeterBinding(t, "v1"), "library", "v1"))
	wantMetric(t, g, 1, "service_instances", "namespace", "greeter", "version", "v1", "state", "waiting")
	wantMetric(t, g, 0, "service_instances", "namespace", "greeter", "version", "v1", "state", "ready")
	wantMetric(t, g, 1, "dependency_unmet", "consumer_namespace", "greeter", "consumer_version", "v1", "provider", "library")
	wantMetric(t, g, 1, "start_gate_transitions_total", "namespace", "greeter", "version", "v1", "from", "none", "to", "waiting")

	// Dependency arrives: unmet -> met, waiting -> ready.
	lib := mustRegister(t, g, addr, libraryBinding(t, "v1"))
	wantMetric(t, g, 1, "service_instances", "namespace", "greeter", "version", "v1", "state", "ready")
	wantMetric(t, g, 0, "service_instances", "namespace", "greeter", "version", "v1", "state", "waiting")
	wantMetric(t, g, 0, "dependency_unmet", "consumer_namespace", "greeter", "consumer_version", "v1", "provider", "library")
	wantMetric(t, g, 1, "start_gate_transitions_total", "namespace", "greeter", "version", "v1", "from", "waiting", "to", "ready")

	// Dependency lost: degraded, still serving.
	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: lib.GetRegistrationId()}); err != nil {
		t.Fatal(err)
	}
	wantMetric(t, g, 1, "service_instances", "namespace", "greeter", "version", "v1", "state", "degraded")
	wantMetric(t, g, 1, "dependency_unmet", "consumer_namespace", "greeter", "consumer_version", "v1", "provider", "library")
	wantMetric(t, g, 1, "start_gate_transitions_total", "namespace", "greeter", "version", "v1", "from", "ready", "to", "degraded")
	// The vanished line has no series left.
	wantNoSeries(t, g, "service_instances", "namespace", "library")

	// Instance gone: its series go, and the removal is counted.
	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: gr.GetRegistrationId()}); err != nil {
		t.Fatal(err)
	}
	wantNoSeries(t, g, "service_instances", "namespace", "greeter")
	wantNoSeries(t, g, "dependency_unmet", "consumer_namespace", "greeter")
	wantMetric(t, g, 1, "start_gate_transitions_total", "namespace", "greeter", "version", "v1", "from", "degraded", "to", "removed")
}

func TestLifecycleMetrics_Drain(t *testing.T) {
	_, a1 := startGreeterServer(t)
	_, a2 := startGreeterServer(t)
	g := New(WithoutBackpressure())
	t.Cleanup(g.Close)
	regA := registerOn(t, g, greeterReq(t, a1, "greeter-a", "v1"))
	registerOn(t, g, greeterReq(t, a2, "greeter-b", "v1"))

	wantMetric(t, g, 0, "service_drain_instances", "namespace", "greeter", "version", "v1", "drain_state", "safe_to_stop")
	waitDrainState(t, g, "greeter-a", cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP, 5*time.Second)
	wantMetric(t, g, 1, "service_drain_instances", "namespace", "greeter", "version", "v1", "drain_state", "safe_to_stop")
	wantMetric(t, g, 1, "drain_requests_total", "outcome", "requested")
	// Polls (waitDrainState repeats the call) are not new requests.
	if _, ok := metricVal(scrapeMetrics(t, g), lcPrefix+"drain_requests_total", "outcome", "already_requested"); !ok {
		t.Error("repeat drain call not counted as already_requested")
	}

	// Unknown instance is counted, not hidden.
	if _, err := g.ControlPlane().DrainInstance(context.Background(), &cpv1.DrainInstanceRequest{Instance: "nope"}); err == nil {
		t.Fatal("drain of unknown instance succeeded")
	}
	wantMetric(t, g, 1, "drain_requests_total", "outcome", "unknown_instance")

	// Cancel: gauge back to 0.
	drainRPC(t, g, regA, true)
	wantMetric(t, g, 0, "service_drain_instances", "namespace", "greeter", "version", "v1", "drain_state", "safe_to_stop")
	wantMetric(t, g, 1, "drain_requests_total", "outcome", "cancelled")

	// Last covering instance of a dependent: blocked.
	g2 := New(WithoutBackpressure())
	t.Cleanup(g2.Close)
	registerOn(t, g2, greeterReq(t, a1, "greeter-a", "v1"))
	registerOn(t, g2, libraryConsumerReq(t, "v1"))
	drainRPC(t, g2, "greeter-a", false)
	wantMetric(t, g2, 1, "service_drain_instances", "namespace", "greeter", "version", "v1", "drain_state", "blocked")
	wantMetric(t, g2, 0, "service_drain_instances", "namespace", "greeter", "version", "v1", "drain_state", "safe_to_stop")
}

func TestLifecycleMetrics_Sunsetable(t *testing.T) {
	_, a1 := startGreeterServer(t)
	_, a2 := startGreeterServer(t)
	g := New(WithoutBackpressure(), WithCallerHeaders("X-Caller-Service"))
	t.Cleanup(g.Close)
	registerOn(t, g, greeterReq(t, a1, "greeter-v1", "v1"))
	registerOn(t, g, greeterReq(t, a2, "greeter-v2", "v2"))
	regLib := registerOn(t, g, consumerReq(t, "library", "v1", "v2"))

	wantMetric(t, g, 1, "line_sunsetable", "namespace", "greeter", "version", "v1")
	// Only line of its namespace: never sunset-able.
	wantMetric(t, g, 0, "line_sunsetable", "namespace", "library", "version", "v1")

	// Traffic inside the 1h window: no longer sunset-able.
	recordCall(g, "greeter", "v1", "Hello", "library")
	wantMetric(t, g, 0, "line_sunsetable", "namespace", "greeter", "version", "v1")

	// Series follow the line: gone with it.
	if _, err := g.ControlPlane().Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: regLib}); err != nil {
		t.Fatal(err)
	}
	wantNoSeries(t, g, "line_sunsetable", "namespace", "library")
	if !strings.Contains(scrapeMetrics(t, g), "last 1h") {
		t.Error("line_sunsetable help must name the 1h window")
	}
}

type noopSink struct{ noopMetrics }

func TestLifecycleMetrics_CustomSinkRegistersNothing(t *testing.T) {
	g := New(WithMetrics(noopSink{}))
	t.Cleanup(g.Close)
	if g.lifecycle != nil {
		t.Fatal("lifecycle collector registered with a custom sink")
	}
	rec := httptest.NewRecorder()
	g.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestLifecycleMetrics_Prefix(t *testing.T) {
	g := New(WithMetricPrefix("gwag"))
	t.Cleanup(g.Close)
	_, addr := startGreeterServer(t)
	registerOn(t, g, greeterReq(t, addr, "greeter-a", "v1"))
	body := scrapeMetrics(t, g)
	if !strings.Contains(body, "gwag_service_instances{") || strings.Contains(body, "go_api_gateway_service_instances") {
		t.Errorf("prefix not honoured:\n%s", body)
	}
}
