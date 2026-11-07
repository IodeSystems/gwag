package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func probeHealth(t *testing.T, g *Gateway) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body: %v: %s", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestClusterHealthNotReadyUntilRegistrySynced: a gateway that joins a
// cluster whose registry already holds a service must not answer
// /health 200 until that service is in its schema. Before the fix,
// health was 200 at once and the first calls failed with "Cannot
// query field".
func TestClusterHealthNotReadyUntilRegistrySynced(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	_, greeterAddr := startGreeterServer(t)
	registerGreeterOn(t, a, greeterAddr)
	waitForPool(t, a.gw, "greeter", "v1", 30*time.Second)

	// The joining node, wired the way the binaries wire it: schema
	// handler first, then the control plane (which boots cluster
	// tracking in the background), then health.
	g := New(WithCluster(testCluster.b), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	_ = g.Handler()
	_ = g.ControlPlane()

	sawStarting := false
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body := probeHealth(t, g)
		if code == http.StatusOK {
			if body["status"] != "serving" {
				t.Fatalf("200 with status %v, want serving", body["status"])
			}
			if s := g.schema.Load(); s == nil || s.QueryType().Fields()["greeter"] == nil {
				t.Fatalf("/health 200 before the registry replay: greeter is not in the schema (saw starting: %v)", sawStarting)
			}
			break
		}
		if code != http.StatusServiceUnavailable || body["status"] != "starting" {
			t.Fatalf("health = %d %v, want 503 starting", code, body)
		}
		if _, ok := body["active_streams"]; !ok {
			t.Fatalf("starting body lost the active_streams field: %v", body)
		}
		sawStarting = true
		if time.Now().After(deadline) {
			t.Fatalf("health never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawStarting {
		t.Fatalf("health was never 503 starting; the gate did not arm")
	}

	// Drain still wins over a synced node.
	if err := g.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if code, body := probeHealth(t, g); code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("after drain: %d %v, want 503 draining", code, body)
	}
}

// TestHealthStandaloneReadyAtOnce: no cluster, nothing to replay.
func TestHealthStandaloneReadyAtOnce(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	_ = g.ControlPlane()
	if code, body := probeHealth(t, g); code != http.StatusOK || body["status"] != "serving" {
		t.Fatalf("health = %d %v, want 200 serving", code, body)
	}
}
