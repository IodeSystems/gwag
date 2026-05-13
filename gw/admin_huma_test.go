package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

// /admin/services/history is the public-status-page backbone. Pins:
//   - bucket array length matches the ring size for the chosen window
//     (60 for 1m, 60 for 1h, 144 for 24h)
//   - method+caller dimensions collapse: one row per (ns, ver)
//   - bucket count + okCount surface; error is implied (count - ok)
//   - empty buckets land as zero-count entries with a wall-clock
//     pinned StartUnixSec, so the dot strip can render a continuous
//     timeline regardless of recent traffic
func TestAdminHuma_ServicesHistory_Window1m(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	// Two ok + one error in the current second; the error pulls the
	// dot color toward "yellow" UI-side.
	errResp := errors.New("upstream 500")
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 5*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Hello", 7*time.Millisecond, nil)
	gw.cfg.metrics.RecordDispatch(context.Background(), "greeter", "v1", "Bye", 9*time.Millisecond, errResp)

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/history?window=1m", nil)
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
			Buckets   []struct {
				StartUnixSec int64  `json:"startUnixSec"`
				DurationSec  int64  `json:"durationSec"`
				Count        uint64 `json:"count"`
				OkCount      uint64 `json:"okCount"`
			} `json:"buckets"`
		} `json:"services"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Window != "1m" {
		t.Errorf("window=%q, want 1m", out.Window)
	}
	if len(out.Services) != 1 {
		t.Fatalf("want 1 service row (greeter/v1), got %d: %+v", len(out.Services), out.Services)
	}
	row := out.Services[0]
	if row.Namespace != "greeter" || row.Version != "v1" {
		t.Errorf("row=%+v", row)
	}
	if len(row.Buckets) != 60 {
		t.Errorf("buckets=%d, want 60 (1s × 60 = 1m ring)", len(row.Buckets))
	}
	// All three observations land in the same wall-clock second; one
	// bucket carries count=3 / ok=2.
	var hits int
	for _, b := range row.Buckets {
		if b.Count > 0 {
			hits++
			if b.Count != 3 || b.OkCount != 2 {
				t.Errorf("active bucket=%+v want count=3 ok=2", b)
			}
			if b.DurationSec != 1 {
				t.Errorf("durationSec=%d, want 1", b.DurationSec)
			}
		}
	}
	if hits != 1 {
		t.Errorf("hits=%d, want 1", hits)
	}
}

// Registered services with no traffic still appear on the status
// page — both servicesHistory (with empty Buckets) and
// servicesStats (with zero counts). Without this the dashboard
// would silently hide a freshly-registered service until its first
// dispatch.
func TestAdminHuma_ServicesHistory_IncludesUntrafficked(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("untrafficked")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/history?window=1m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Services []struct {
			Namespace string        `json:"namespace"`
			Version   string        `json:"version"`
			Buckets   []interface{} `json:"buckets"`
		} `json:"services"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, s := range out.Services {
		if s.Namespace == "untrafficked" && s.Version == "v1" {
			found = true
			if len(s.Buckets) != 0 {
				t.Errorf("expected empty Buckets for untrafficked service, got %d", len(s.Buckets))
			}
		}
	}
	if !found {
		t.Fatalf("registered service untrafficked/v1 missing from history; got %+v", out.Services)
	}
}

// Mirror coverage for servicesStats: untrafficked services appear
// with zero counts so the Services list shows them too.
func TestAdminHuma_ServicesStats_IncludesUntrafficked(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("idle")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/stats?window=1m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Services []struct {
			Namespace string `json:"namespace"`
			Version   string `json:"version"`
			Count     uint64 `json:"count"`
		} `json:"services"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, s := range out.Services {
		if s.Namespace == "idle" && s.Version == "v1" {
			found = true
			if s.Count != 0 {
				t.Errorf("expected zero Count for idle service, got %d", s.Count)
			}
		}
	}
	if !found {
		t.Fatalf("registered service idle/v1 missing from stats; got %+v", out.Services)
	}
}

func TestAdminHuma_ServicesHistory_RejectsBadWindow(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/history?window=5m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("expected non-200; got %d body=%s", rr.Code, rr.Body.String())
	}
}

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

// deprecatedStatsResponse mirrors the JSON shape for assertions —
// we don't pull the gateway's internal types into the test scope so
// drift in field tags shows up as a decode error rather than a silent
// rename.
type deprecatedStatsResponse struct {
	Window   string `json:"window"`
	Services []struct {
		Namespace       string  `json:"namespace"`
		Version         string  `json:"version"`
		ManualReason    string  `json:"manualReason"`
		AutoReason      string  `json:"autoReason"`
		TotalCount      uint64  `json:"totalCount"`
		TotalThroughput float64 `json:"totalThroughput"`
		Methods         []struct {
			Method  string `json:"method"`
			Count   uint64 `json:"count"`
			Callers []struct {
				Caller string `json:"caller"`
				Count  uint64 `json:"count"`
			} `json:"callers"`
		} `json:"methods"`
	} `json:"services"`
}

// stampSlot plants a synthetic slot + matching deprecation side-state
// for tests that need ListServices to surface deprecation without a
// live registration. Plan §5 path: registerSlotLocked stamps a slot
// from g.deprecation; tests skip the registration and write both
// directly.
func stampSlot(t *testing.T, gw *Gateway, ns, ver, manualReason string) {
	t.Helper()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.slots == nil {
		gw.slots = map[poolKey]*slot{}
	}
	if gw.deprecation == nil {
		gw.deprecation = map[poolKey]string{}
	}
	gw.slots[poolKey{namespace: ns, version: ver}] = &slot{
		kind:              slotKindProto,
		deprecationReason: manualReason,
	}
	if manualReason != "" {
		gw.deprecation[poolKey{namespace: ns, version: ver}] = manualReason
	}
}

// TestAdminHuma_DeprecatedStats_ManualAndAuto exercises the cross-
// service "should I retire this?" panel. Three (ns, ver) registered:
// users/v1 (manually deprecated), users/v2 (auto-deprecated by v3),
// users/v3 (current — must NOT appear). The non-deprecated greeter/v1
// must also not appear. Sort posture is total-count desc, so the
// busier deprecated service surfaces first.
func TestAdminHuma_DeprecatedStats_ManualAndAuto(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithCallerHeaders("X-Caller-Service"))
	t.Cleanup(gw.Close)

	stampSlot(t, gw, "users", "v1", "rotated to v2")
	stampSlot(t, gw, "users", "v2", "")
	stampSlot(t, gw, "users", "v3", "")
	stampSlot(t, gw, "greeter", "v1", "")

	// Two callers on the manually-deprecated users/v1.list; one
	// caller on the auto-deprecated users/v2.read; one caller on the
	// current users/v3.list (should NOT appear). users/v1 ends up at
	// 3 calls > users/v2 at 2, so sort puts users/v1 first.
	rec := func(ns, ver, method, caller string, ms int64) {
		ctx := context.Background()
		if caller != "" {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Caller-Service", caller)
			ctx = withHTTPRequest(ctx, req)
		}
		gw.cfg.metrics.RecordDispatch(ctx, ns, ver, method, time.Duration(ms)*time.Millisecond, nil)
	}
	rec("users", "v1", "list", "ui", 5)
	rec("users", "v1", "list", "ui", 6)
	rec("users", "v1", "list", "legacy-importer", 8)
	rec("users", "v2", "read", "ui", 4)
	rec("users", "v2", "read", "ui", 4)
	rec("users", "v3", "list", "ui", 3)
	rec("greeter", "v1", "Hello", "ui", 2)

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/deprecated/stats?window=1m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out deprecatedStatsResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Window != "1m" {
		t.Errorf("window=%q, want 1m", out.Window)
	}
	if len(out.Services) != 2 {
		t.Fatalf("got %d deprecated services, want 2 (users/v1 + users/v2): %+v", len(out.Services), out.Services)
	}
	// users/v1 has 3 calls (manual reason); users/v2 has 2 (auto).
	first, second := out.Services[0], out.Services[1]
	if first.Namespace != "users" || first.Version != "v1" {
		t.Errorf("first row=%s/%s, want users/v1 (highest call volume)", first.Namespace, first.Version)
	}
	if first.ManualReason != "rotated to v2" {
		t.Errorf("first manualReason=%q, want %q", first.ManualReason, "rotated to v2")
	}
	if first.AutoReason != "v3 is current" {
		t.Errorf("first autoReason=%q, want %q (older than v3)", first.AutoReason, "v3 is current")
	}
	if first.TotalCount != 3 {
		t.Errorf("first totalCount=%d, want 3", first.TotalCount)
	}
	if len(first.Methods) != 1 || first.Methods[0].Method != "list" {
		t.Fatalf("first methods=%+v, want one entry for 'list'", first.Methods)
	}
	if len(first.Methods[0].Callers) != 2 {
		t.Fatalf("first method callers=%+v, want 2 (ui + legacy-importer)", first.Methods[0].Callers)
	}
	// Caller sort: count desc — ui at 2 first, legacy-importer at 1.
	if first.Methods[0].Callers[0].Caller != "ui" || first.Methods[0].Callers[0].Count != 2 {
		t.Errorf("top caller=%+v, want ui count=2", first.Methods[0].Callers[0])
	}
	if second.Namespace != "users" || second.Version != "v2" {
		t.Errorf("second row=%s/%s, want users/v2", second.Namespace, second.Version)
	}
	if second.ManualReason != "" {
		t.Errorf("second manualReason=%q, want empty (auto-only)", second.ManualReason)
	}
	if second.AutoReason != "v3 is current" {
		t.Errorf("second autoReason=%q, want %q", second.AutoReason, "v3 is current")
	}
	// users/v3 (current) must not appear.
	for _, s := range out.Services {
		if s.Namespace == "users" && s.Version == "v3" {
			t.Errorf("users/v3 leaked into deprecated panel: %+v", s)
		}
		if s.Namespace == "greeter" {
			t.Errorf("non-deprecated greeter leaked: %+v", s)
		}
	}
}

// TestAdminHuma_DeprecatedStats_NoTrafficSafeToRetire confirms a
// deprecated service with zero recorded traffic still surfaces — the
// "safe to retire" candidate. Methods are empty + totalCount is 0.
func TestAdminHuma_DeprecatedStats_NoTrafficSafeToRetire(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)

	stampSlot(t, gw, "users", "v1", "rotated to v2")
	stampSlot(t, gw, "users", "v2", "")
	// No RecordDispatch calls.

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/deprecated/stats?window=24h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out deprecatedStatsResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Services) != 1 {
		t.Fatalf("got %d, want 1 (users/v1 with zero traffic): %+v", len(out.Services), out.Services)
	}
	row := out.Services[0]
	if row.TotalCount != 0 {
		t.Errorf("totalCount=%d, want 0 (safe-to-retire)", row.TotalCount)
	}
	if len(row.Methods) != 0 {
		t.Errorf("methods=%+v, want empty (no traffic recorded)", row.Methods)
	}
	if row.ManualReason != "rotated to v2" {
		t.Errorf("manualReason=%q, want preserved", row.ManualReason)
	}
}

// TestAdminHuma_MCP_RoundTrip exercises the four admin_mcp_* routes
// (list / include / exclude / setAutoInclude) end-to-end against the
// in-process MCPConfig state. Pins: list reflects the result of every
// write; include / exclude are idempotent (re-adding doesn't double
// up); the GET form has no Body wrapping (huma flattens that into the
// JSON; the writes return the new MCPConfig directly).
func TestAdminHuma_MCP_RoundTrip(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	type listShape struct {
		AutoInclude bool     `json:"autoInclude"`
		Include     []string `json:"include"`
		Exclude     []string `json:"exclude"`
	}
	getList := func(t *testing.T) listShape {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/admin/mcp", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
		}
		var out listShape
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out
	}
	post := func(t *testing.T, path, body string) listShape {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var out listShape
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode POST %s: %v", path, err)
		}
		return out
	}

	// Fresh: empty config; include/exclude are [] (never null).
	if got := getList(t); got.AutoInclude || len(got.Include) != 0 || len(got.Exclude) != 0 {
		t.Fatalf("fresh list=%+v, want empty", got)
	}

	// include adds + is idempotent.
	got := post(t, "/admin/mcp/include", `{"path":"admin.peers.*"}`)
	if len(got.Include) != 1 || got.Include[0] != "admin.peers.*" {
		t.Errorf("after include 1: %+v", got)
	}
	post(t, "/admin/mcp/include", `{"path":"users.list"}`)
	got = post(t, "/admin/mcp/include", `{"path":"admin.peers.*"}`) // dup
	if len(got.Include) != 2 {
		t.Errorf("after dup include: %+v, want 2 entries", got)
	}

	// exclude adds independently.
	got = post(t, "/admin/mcp/exclude", `{"path":"users.delete"}`)
	if len(got.Exclude) != 1 || got.Exclude[0] != "users.delete" {
		t.Errorf("after exclude: %+v", got)
	}

	// setAutoInclude toggles.
	got = post(t, "/admin/mcp/auto-include", `{"autoInclude":true}`)
	if !got.AutoInclude {
		t.Errorf("setAutoInclude(true): %+v", got)
	}

	// Final list reflects every write.
	final := getList(t)
	if !final.AutoInclude || len(final.Include) != 2 || len(final.Exclude) != 1 {
		t.Errorf("final list=%+v", final)
	}

	// Sanity: gateway in-process state matches.
	snap := gw.mcpConfigSnapshot()
	if !snap.AutoInclude || len(snap.Include) != 2 || len(snap.Exclude) != 1 {
		t.Errorf("gateway snapshot=%+v drifted from admin list=%+v", snap, final)
	}
}

func TestAdminHuma_MCP_WritesRequireBearer(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	// No Authorization header → 401, state unchanged.
	for _, c := range []struct {
		path string
		body string
	}{
		{"/admin/mcp/include", `{"path":"x"}`},
		{"/admin/mcp/exclude", `{"path":"x"}`},
		{"/admin/mcp/auto-include", `{"autoInclude":true}`},
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without bearer: status=%d, want 401", c.path, rr.Code)
		}
	}
	if snap := gw.mcpConfigSnapshot(); snap.AutoInclude || len(snap.Include) != 0 || len(snap.Exclude) != 0 {
		t.Errorf("state drifted after rejected writes: %+v", snap)
	}
}

func TestAdminHuma_MCP_RejectsEmptyPath(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)

	for _, p := range []string{"/admin/mcp/include", "/admin/mcp/exclude"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"path":""}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("POST %s empty path: status=%d, want 400", p, rr.Code)
		}
	}
}

// TestAdminHuma_MCPSchema_AdminRoutesRoundTrip exercises the three
// schema tools exposed under /admin/mcp/schema/*. Confirms the wiring
// honors the MCPConfig allowlist (the underlying tools already test
// every flavor; here we just want the admin HTTP layer to surface the
// right shape).
func TestAdminHuma_MCPSchema_AdminRoutesRoundTrip(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("things")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	if err := gw.setMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}

	h := newAdminRouter(t, gw)

	// list
	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/schema/list", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("schema/list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var listOut struct {
		Entries []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&listOut); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listOut.Entries) != 2 {
		t.Fatalf("list entries=%d, want 2: %+v", len(listOut.Entries), listOut.Entries)
	}

	// search
	req = httptest.NewRequest(http.MethodPost, "/admin/mcp/schema/search", strings.NewReader(`{"pathGlob":"things.get*"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("schema/search status=%d body=%s", rr.Code, rr.Body.String())
	}

	// expand
	req = httptest.NewRequest(http.MethodPost, "/admin/mcp/schema/expand", strings.NewReader(`{"name":"things.getThing"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("schema/expand status=%d body=%s", rr.Code, rr.Body.String())
	}

	// expand on unknown name returns 400
	req = httptest.NewRequest(http.MethodPost, "/admin/mcp/schema/expand", strings.NewReader(`{"name":"things.nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expand on unknown name: status=%d, want 400", rr.Code)
	}
}

// TestAdminHuma_MCPQuery exercises the in-process query tool via the
// admin route. The schema must be materialized (assembleLocked) for
// the query to dispatch.
func TestAdminHuma_MCPQuery(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("things")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	// Force initial schema build (matches MCP tool fixture).
	gw.mu.Lock()
	if err := gw.assembleLocked(); err != nil {
		gw.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	gw.mu.Unlock()

	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodPost, "/admin/mcp/query", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Result struct {
			Response any `json:"response"`
			Events   struct {
				Level    string `json:"level"`
				Channels []any  `json:"channels"`
			} `json:"events"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Result.Response == nil {
		t.Fatal("Response missing")
	}
	if out.Result.Events.Level != "none" {
		t.Errorf("events.level=%q, want none in v1", out.Result.Events.Level)
	}
	if out.Result.Events.Channels == nil || len(out.Result.Events.Channels) != 0 {
		t.Errorf("events.channels=%+v, want empty slice", out.Result.Events.Channels)
	}
}

// Writes require bearer.
func TestAdminHuma_MCPQuery_RequiresBearer(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodPost, "/admin/mcp/query", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// TestAdminHuma_DeprecatedStats_RejectsBadWindow keeps the boundary
// check on this endpoint in lockstep with serviceStats / servicesStats.
func TestAdminHuma_DeprecatedStats_RejectsBadWindow(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(gw.Close)
	h := newAdminRouter(t, gw)
	req := httptest.NewRequest(http.MethodGet, "/admin/services/deprecated/stats?window=5m", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("expected non-200 for bad window; got %d body=%s", rr.Code, rr.Body.String())
	}
}
