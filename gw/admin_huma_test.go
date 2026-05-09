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
			ctx = WithHTTPRequest(ctx, req)
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
