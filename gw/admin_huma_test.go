package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestAdminHuma_ServiceStats_Window1m(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Bye", 9*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "users", "v1", "List", 12*time.Millisecond, nil)

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/greeter/v1/stats?window=1m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Window  string `json:"window"`
		Methods []struct {
			Method  string `json:"method"`
			Caller  string `json:"caller"`
			Count   uint64 `json:"count"`
			OkCount uint64 `json:"okCount"`
		} `json:"methods"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Window != "1m" {
		t.Errorf("window=%q, want 1m", out.Window)
	}
	if len(out.Methods) != 2 {
		t.Fatalf("got %d methods, want 2 (only greeter/v1 rows): %+v", len(out.Methods), out.Methods)
	}
	// Sorted: Bye then Hello.
	if out.Methods[0].Method != "Bye" || out.Methods[0].Count != 1 {
		t.Errorf("Bye row=%+v", out.Methods[0])
	}
	if out.Methods[1].Method != "Hello" || out.Methods[1].Count != 2 {
		t.Errorf("Hello row=%+v", out.Methods[1])
	}
}

func TestAdminHuma_ServiceStats_RejectsBadWindow(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/greeter/v1/stats?window=5m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("expected non-200 for bad window; got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestAdminHuma_ServicesStats_Aggregate confirms the /admin/services/stats
// route collapses per-(method, caller) rows into one row per
// (namespace, version) — the shape the Services list pulls so it can
// render columns without N round-trips. Counts and throughput sum;
// percentile fields take the max ("worst method drives the row").
func TestAdminHuma_ServicesStats_Aggregate(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Bye", 9*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "users", "v1", "List", 12*time.Millisecond, nil)

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/stats?window=1m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Window   string `json:"window"`
		Services []struct {
			Namespace string `json:"namespace"`
			Version   string `json:"version"`
			Count     uint64 `json:"count"`
			OkCount   uint64 `json:"okCount"`
		} `json:"services"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Window != "1m" {
		t.Errorf("window=%q, want 1m", out.Window)
	}
	if len(out.Services) != 2 {
		t.Fatalf("got %d rows, want 2 (greeter/v1 + users/v1): %+v", len(out.Services), out.Services)
	}
	// Sorted: greeter then users.
	if out.Services[0].Namespace != "greeter" || out.Services[0].Version != "v1" || out.Services[0].Count != 3 {
		t.Errorf("greeter row=%+v want count=3 (Hello x2 + Bye)", out.Services[0])
	}
	if out.Services[1].Namespace != "users" || out.Services[1].Version != "v1" || out.Services[1].Count != 1 {
		t.Errorf("users row=%+v want count=1", out.Services[1])
	}
}

// TestAdminHuma_ListServices_ManualDeprecationReason confirms that a
// service flipped via the Deprecate RPC surfaces its reason on the
// listServices response — the field the UI consumes to render the
// "manual" half of the deprecated badge.
func TestAdminHuma_ListServices_ManualDeprecationReason(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	// Plant a deprecation in the side-state mirror; the next slot
	// registration stamps it onto the slot via registerSlotLocked
	// (plan §5 deprecate path).
	gw.mu.Lock()
	if gw.deprecation == nil {
		gw.deprecation = map[poolKey]string{}
	}
	gw.deprecation[poolKey{namespace: "users", version: "v1"}] = "rotated to v2"
	// Insert a synthetic slot so ListServices has something to walk.
	if gw.slots == nil {
		gw.slots = map[poolKey]*slot{}
	}
	gw.slots[poolKey{namespace: "users", version: "v1"}] = &slot{
		kind:              slotKindProto,
		deprecationReason: "rotated to v2",
	}
	gw.mu.Unlock()

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Services []struct {
			Namespace               string `json:"namespace"`
			Version                 string `json:"version"`
			ManualDeprecationReason string `json:"manualDeprecationReason"`
		} `json:"services"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, s := range out.Services {
		if s.Namespace == "users" && s.Version == "v1" {
			found = true
			if s.ManualDeprecationReason != "rotated to v2" {
				t.Errorf("manualDeprecationReason=%q, want %q", s.ManualDeprecationReason, "rotated to v2")
			}
		}
	}
	if !found {
		t.Fatalf("users/v1 missing from response: %+v", out.Services)
	}
}
