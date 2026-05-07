package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newAdminRouter wires the huma admin mux behind AdminMiddleware so
// each test can hit /admin/* endpoints directly without booting an
// example binary. Token defaults to "tok"; reads stay public, writes
// require Bearer.
func newAdminRouter(t *testing.T, gw *Gateway) http.Handler {
	t.Helper()
	mux, _, err := gw.AdminHumaRouter()
	if err != nil {
		t.Fatalf("AdminHumaRouter: %v", err)
	}
	return gw.AdminMiddleware(mux)
}

func TestAdminHuma_Channels_EmptyWhenNoBroker(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	req := httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Channels []struct {
			Subject   string
			Consumers int
		} `json:"channels"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Channels == nil {
		t.Fatal("channels is nil; expected empty slice (NonNull contract)")
	}
	if len(out.Channels) != 0 {
		t.Fatalf("got %d channels, want 0", len(out.Channels))
	}
}

func TestAdminHuma_Drain_NoActiveStreamsReturnsImmediately(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	body := strings.NewReader(`{"timeoutSeconds": 5}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/drain", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Drained       bool   `json:"drained"`
		ActiveStreams int    `json:"activeStreams"`
		Reason        string `json:"reason"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Drained {
		t.Fatalf("drained=false reason=%q (expected immediate success when no active streams)", out.Reason)
	}
	if out.ActiveStreams != 0 {
		t.Fatalf("activeStreams=%d, want 0", out.ActiveStreams)
	}
	if !gw.IsDraining() {
		t.Fatal("Gateway should be marked draining after /admin/drain")
	}
}

func TestAdminHuma_Drain_RequiresBearer(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/drain", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (drain requires bearer)", rr.Code)
	}
	if gw.IsDraining() {
		t.Fatal("Gateway should NOT be draining after rejected /admin/drain")
	}
}

func TestAdminHuma_OpenAPIJSONReachable(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	req := httptest.NewRequest(http.MethodGet, "/admin/openapi.json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"openapi"`) {
		t.Fatalf("body missing openapi key: %s", rr.Body.String()[:min(rr.Body.Len(), 200)])
	}
}

func TestAdminHuma_Channels_ReflectsActiveSubjects(t *testing.T) {
	// Confirm the wiring path: ActiveSubjects → channels listing.
	// We don't boot NATS here; instead we manually populate the
	// broker as if a subscribe happened.
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	gw.broker = &subBroker{
		subs: map[string]*subFanout{
			"events.greeter.Greetings.alice": {
				subject: "events.greeter.Greetings.alice",
				targets: map[uint64]chan any{1: make(chan any, 1), 2: make(chan any, 1)},
			},
			"events.greeter.Greetings.bob": {
				subject: "events.greeter.Greetings.bob",
				targets: map[uint64]chan any{3: make(chan any, 1)},
			},
		},
	}

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Channels []struct {
			Subject   string `json:"subject"`
			Consumers int    `json:"consumers"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Channels) != 2 {
		t.Fatalf("got %d channels, want 2: %+v", len(out.Channels), out.Channels)
	}
	// Sorted by subject — alice before bob.
	if out.Channels[0].Subject != "events.greeter.Greetings.alice" || out.Channels[0].Consumers != 2 {
		t.Errorf("channels[0]=%+v want alice/2", out.Channels[0])
	}
	if out.Channels[1].Subject != "events.greeter.Greetings.bob" || out.Channels[1].Consumers != 1 {
		t.Errorf("channels[1]=%+v want bob/1", out.Channels[1])
	}
}

// silence unused for older toolchains
var _ = context.Background
