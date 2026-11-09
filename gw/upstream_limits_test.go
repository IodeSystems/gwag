package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// waitBudgetGateway is testGateway with a backpressure wait budget,
// so a rate-limited call may wait for a token.
func waitBudgetGateway(t *testing.T, maxWait time.Duration, opts ...Option) *Gateway {
	t.Helper()
	return testGateway(t, append([]Option{WithBackpressure(BackpressureOptions{MaxWaitTime: maxWait})}, opts...)...)
}

func restGet(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRateLimit_SplitRecomputedOnPeerChange(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)
	gw := testGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), RateLimit(10, 10)); err != nil {
		t.Fatal(err)
	}
	l := gw.limits
	b := l.buckets[limitScope{Namespace: "test", Version: "v1"}]
	if b == nil {
		t.Fatal("no bucket for test:v1")
	}
	check := func(n int, wantRate float64, wantBurst int) {
		t.Helper()
		l.setPeers(n)
		lim := b.limiter()
		if lim.Limit() != rate.Limit(wantRate) || lim.Burst() != wantBurst {
			t.Errorf("peers %d: limit %v burst %d, want %v burst %d", n, lim.Limit(), lim.Burst(), wantRate, wantBurst)
		}
	}
	check(1, 10, 10)
	check(2, 5, 5)
	check(4, 2.5, 2)
	check(20, 0.5, 1) // burst floor is 1
	check(1, 10, 10)

	// An operator limit replaces the code limit for the same scope, at
	// the current split; deleting it brings the code limit back.
	l.setPeers(2)
	rec := limitRecord{Kind: limitKindRate, Namespace: "test", Version: "v1", RPS: 4, Burst: 2}
	if err := l.putRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if lim := l.buckets[rec.scope()].limiter(); lim.Limit() != 2 || lim.Burst() != 1 {
		t.Errorf("admin limit at 2 peers: %v burst %d, want 2 burst 1", lim.Limit(), lim.Burst())
	}
	if ok, err := l.deleteRecord(context.Background(), rec.key()); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if lim := l.buckets[rec.scope()].limiter(); lim.Limit() != 5 || lim.Burst() != 5 {
		t.Errorf("after delete: %v burst %d, want code limit 5 burst 5", lim.Limit(), lim.Burst())
	}
}

func TestRateLimit_WaitsThenRejectsWithRetryAfter(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)

	t.Run("wait within budget", func(t *testing.T) {
		gw := waitBudgetGateway(t, time.Second)
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), RateLimit(5, 1)); err != nil {
			t.Fatal(err)
		}
		h := gw.IngressHandler()
		if rr := restGet(t, h, "/things/1", nil); rr.Code != 200 {
			t.Fatalf("first: %d %s", rr.Code, rr.Body)
		}
		start := time.Now()
		if rr := restGet(t, h, "/things/1", nil); rr.Code != 200 {
			t.Fatalf("second: %d %s", rr.Code, rr.Body)
		}
		if d := time.Since(start); d < 150*time.Millisecond {
			t.Errorf("second call took %v; want it to wait about 200ms for a token", d)
		}
	})

	t.Run("wait past budget", func(t *testing.T) {
		gw := waitBudgetGateway(t, 200*time.Millisecond)
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), OperationRateLimit("getThing", 0.5, 1)); err != nil {
			t.Fatal(err)
		}
		h := gw.IngressHandler()
		before := hits.Load()
		if rr := restGet(t, h, "/things/1", nil); rr.Code != 200 {
			t.Fatalf("first: %d %s", rr.Code, rr.Body)
		}
		rr := restGet(t, h, "/things/1", nil)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("second: %d %s, want 429", rr.Code, rr.Body)
		}
		if ra := rr.Header().Get("Retry-After"); ra != "2" {
			t.Errorf("Retry-After %q, want 2 (a token every 2s)", ra)
		}
		if got := hits.Load() - before; got != 1 {
			t.Errorf("upstream got %d calls, want 1", got)
		}
		// GraphQL carries the same code and retry hint.
		body := postGQL(t, gw.Handler(), getThingQuery)
		if !strings.Contains(body, `"retryAfterSeconds":`) || !strings.Contains(body, "rate limit") {
			t.Errorf("graphql body %s", body)
		}
	})
}

func TestRateLimit_LimitedServiceDoesNotDelayAnother(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)
	gw := waitBudgetGateway(t, 3*time.Second)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("slow"), RateLimit(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("fast")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	postGQL(t, h, `{ slow { getThing(id:"1") { id } } }`) // takes the token
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postGQL(t, h, `{ slow { getThing(id:"1") { id } } }`) // each waits ~1s more
		}()
	}
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 20; i++ {
		start := time.Now()
		body := postGQL(t, h, `{ fast { getThing(id:"1") { id } } }`)
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("fast call %d took %v while slow waits for tokens", i, d)
		}
		if !strings.Contains(body, `"id":"abc"`) {
			t.Fatalf("fast call %d: %s", i, body)
		}
	}
	wg.Wait()
}

// throttlingBackend answers 429 (with retryAfter, when set) for the
// first n calls, then 200.
func throttlingBackend(t *testing.T, hits *atomic.Int32, n int32, retryAfter string) *httptest.Server {
	return countingBackend(t, hits, func(w http.ResponseWriter, r *http.Request) {
		if hits.Load() <= n {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	})
}

func TestUpstream429_PausesLimitedService(t *testing.T) {
	var hits atomic.Int32
	be := throttlingBackend(t, &hits, 1, "2")
	gw := waitBudgetGateway(t, time.Second)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), RateLimit(100, 100)); err != nil {
		t.Fatal(err)
	}
	h := gw.IngressHandler()
	rr := restGet(t, h, "/things/1", nil)
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "2" {
		t.Fatalf("first: %d Retry-After %q; want the upstream's 429 and 2", rr.Code, rr.Header().Get("Retry-After"))
	}
	if d := gw.limits.pausedFor("test", "v1"); d < time.Second || d > 2*time.Second {
		t.Fatalf("pause %v, want about 2s", d)
	}
	rr = restGet(t, h, "/things/1", nil)
	if rr.Code != 429 || !strings.Contains(rr.Body.String(), "paused") {
		t.Fatalf("during pause: %d %s", rr.Code, rr.Body)
	}
	if ra := rr.Header().Get("Retry-After"); ra != "2" && ra != "1" {
		t.Errorf("during pause Retry-After %q", ra)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream got %d calls during the pause, want 1", got)
	}

	// No Retry-After: the default pause (here 300ms) applies.
	var hits2 atomic.Int32
	be2 := throttlingBackend(t, &hits2, 1, "")
	gw2 := waitBudgetGateway(t, time.Second, WithUpstream429Pause(300*time.Millisecond))
	if err := gw2.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be2.URL), As("test"), RateLimit(100, 100)); err != nil {
		t.Fatal(err)
	}
	restGet(t, gw2.IngressHandler(), "/things/1", nil)
	if d := gw2.limits.pausedFor("test", "v1"); d <= 0 || d > 300*time.Millisecond {
		t.Fatalf("default pause %v, want <= 300ms", d)
	}
	time.Sleep(350 * time.Millisecond)
	if rr := restGet(t, gw2.IngressHandler(), "/things/1", nil); rr.Code != 200 {
		t.Fatalf("after the pause: %d %s", rr.Code, rr.Body)
	}

	// A service without a rate limit relays 429 and is not paused.
	var hits3 atomic.Int32
	be3 := throttlingBackend(t, &hits3, 1, "30")
	gw3 := waitBudgetGateway(t, time.Second)
	if err := gw3.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be3.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	restGet(t, gw3.IngressHandler(), "/things/1", nil)
	if d := gw3.limits.pausedFor("test", "v1"); d != 0 {
		t.Fatalf("unlimited service paused for %v", d)
	}
	if rr := restGet(t, gw3.IngressHandler(), "/things/1", nil); rr.Code != 200 {
		t.Fatalf("unlimited service second call: %d", rr.Code)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"3":                             3 * time.Second,
		"0":                             0,
		"junk":                          0,
		"999999":                        maxPause429,
		"Fri, 02 Jan 2026 03:04:15 GMT": 10 * time.Second,
		"Fri, 02 Jan 2026 03:04:00 GMT": 0,
	} {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func callerHdr(id string) map[string]string { return map[string]string{PublicCallerIDHeader: id} }

func TestQuota_PerCallerDefaultAndOverride(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)
	gw := waitBudgetGateway(t, time.Second, WithCallerIDPublic())
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range []limitRecord{
		{Kind: limitKindQuota, Default: true, Namespace: "test", Limit: 2, PerMs: time.Hour.Milliseconds()},
		{Kind: limitKindQuota, Caller: "carol", Namespace: "test", Limit: 4, PerMs: time.Hour.Milliseconds()},
	} {
		if err := gw.limits.putRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	h := gw.IngressHandler()
	codes := func(caller string, n int) []int {
		var out []int
		for i := 0; i < n; i++ {
			out = append(out, restGet(t, h, "/things/1", callerHdr(caller)).Code)
		}
		return out
	}
	if got := codes("alice", 3); got[0] != 200 || got[1] != 200 || got[2] != 429 {
		t.Errorf("alice: %v, want 200 200 429", got)
	}
	if got := codes("bob", 2); got[0] != 200 || got[1] != 200 {
		t.Errorf("bob (own count under the default): %v", got)
	}
	if got := codes("carol", 5); got[3] != 200 || got[4] != 429 {
		t.Errorf("carol (own quota of 4): %v", got)
	}
	rr := restGet(t, h, "/things/1", callerHdr("alice"))
	if !strings.Contains(rr.Body.String(), "quota exceeded for caller=alice") || rr.Header().Get("Retry-After") == "" {
		t.Errorf("quota refusal: %d %q Retry-After %q", rr.Code, rr.Body, rr.Header().Get("Retry-After"))
	}
}

func TestQuota_CostUnits(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)
	gw := waitBudgetGateway(t, time.Second, WithCallerIDPublic())
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), OperationCost("getThing", 3)); err != nil {
		t.Fatal(err)
	}
	if err := gw.limits.putRecord(context.Background(), limitRecord{Kind: limitKindQuota, Default: true, Limit: 7, PerMs: time.Minute.Milliseconds(), Units: true}); err != nil {
		t.Fatal(err)
	}
	h := gw.IngressHandler()
	var got []int
	for i := 0; i < 3; i++ {
		got = append(got, restGet(t, h, "/things/1", callerHdr("alice")).Code)
	}
	if got[0] != 200 || got[1] != 200 || got[2] != 429 {
		t.Errorf("3-unit calls against 7 units: %v, want 200 200 429", got)
	}
	// An operator cost replaces the code cost.
	if err := gw.limits.putRecord(context.Background(), limitRecord{Kind: limitKindCost, Namespace: "test", Version: "v1", Operation: "getThing", Cost: 1}); err != nil {
		t.Fatal(err)
	}
	if c := restGet(t, h, "/things/1", callerHdr("alice")).Code; c != 200 {
		t.Errorf("1-unit call with 1 unit left: %d", c)
	}
}

func TestQuota_ShortWindowSplitsAcrossPeers(t *testing.T) {
	qr := newQuotaRule("k", limitRecord{Kind: limitKindQuota, Default: true, Limit: 10, PerMs: 1000})
	now := time.UnixMilli(1_000_000_000_000) // a window start
	allowed := 0
	for i := 0; i < 20; i++ {
		if _, _, ok := qr.debit("alice", 1, now, 2, true); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("2 peers, 10/s: node allowed %d, want 5", allowed)
	}
	// Next window resets; standalone (not clustered) gets the full 10.
	qr2 := newQuotaRule("k2", limitRecord{Kind: limitKindQuota, Default: true, Limit: 10, PerMs: 1000})
	allowed = 0
	for i := 0; i < 20; i++ {
		if _, _, ok := qr2.debit("alice", 1, now, 2, false); ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("standalone: allowed %d, want 10", allowed)
	}
	if _, retry, ok := qr.debit("alice", 1, now.Add(400*time.Millisecond), 2, true); ok || retry != 600*time.Millisecond {
		t.Errorf("over limit: ok %v retry %v, want refusal with 600ms to the window end", ok, retry)
	}
	if _, _, ok := qr.debit("alice", 1, now.Add(time.Second), 2, true); !ok {
		t.Error("next window still refused")
	}
}

func TestUsage_AggregatesAndCSV(t *testing.T) {
	var hits atomic.Int32
	be := throttlingBackend(t, &hits, 1, "")
	gw := waitBudgetGateway(t, time.Second, WithCallerIDPublic(), WithUsage(UsageOptions{}), WithAdminToken([]byte("tok")))
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), OperationCost("getThing", 2)); err != nil {
		t.Fatal(err)
	}
	if err := gw.limits.putRecord(context.Background(), limitRecord{Kind: limitKindQuota, Caller: "bob", Limit: 1, PerMs: time.Hour.Milliseconds()}); err != nil {
		t.Fatal(err)
	}
	h := gw.IngressHandler()
	restGet(t, h, "/things/1", callerHdr("alice")) // upstream 429 (no limit: relayed, not paused)
	restGet(t, h, "/things/1", callerHdr("alice")) // ok
	restGet(t, h, "/things/1", callerHdr("alice")) // ok
	restGet(t, h, "/things/1", callerHdr("bob"))   // ok
	restGet(t, h, "/things/1", callerHdr("bob"))   // quota

	admin := newAdminRouter(t, gw)
	rr := httptest.NewRecorder()
	admin.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/usage?service=test", nil))
	if rr.Code != 200 {
		t.Fatalf("usage: %d %s", rr.Code, rr.Body)
	}
	var rep struct {
		From, To string
		Rows     []usageRow
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	today := dayString(epochDay(time.Now()))
	if rep.To != today || len(rep.Rows) != 2 {
		t.Fatalf("report %+v", rep)
	}
	a, b := rep.Rows[0], rep.Rows[1]
	if a.Caller != "alice" || a.Operation != "getThing" || a.Day != today ||
		a.usageCounts != (usageCounts{Calls: 3, Units: 6, Upstream429: 1}) {
		t.Errorf("alice row %+v", a)
	}
	if b.Caller != "bob" || b.usageCounts != (usageCounts{Calls: 2, Units: 2, Quota: 1}) {
		t.Errorf("bob row %+v", b)
	}

	rr = httptest.NewRecorder()
	admin.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/usage?format=csv&caller=bob", nil))
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv: %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	want := "day,caller,namespace,version,operation,calls,units,rate_limited,quota_exceeded,upstream_429,errors\n" +
		today + ",bob,test,v1,getThing,2,2,0,1,0,0\n"
	if rr.Body.String() != want {
		t.Errorf("csv:\n%s\nwant:\n%s", rr.Body, want)
	}
}

func TestLimitsAdmin_WritesFrozen(t *testing.T) {
	gw := waitBudgetGateway(t, time.Second, WithAdminToken([]byte("tok")))
	h := newAdminRouter(t, gw)
	put := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/admin/limits/rate", strings.NewReader(`{"service":"vendor:v1","rps":5,"burst":5}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", bearerHex("tok"))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := put(); rr.Code != 200 {
		t.Fatalf("put: %d %s", rr.Code, rr.Body)
	}
	if _, err := gw.setFreeze(context.Background(), time.Minute, "test", "t"); err != nil {
		t.Fatal(err)
	}
	if rr := put(); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "frozen") {
		t.Fatalf("frozen put: %d %s", rr.Code, rr.Body)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/limits", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"namespace":"vendor"`) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body)
	}
}

func scrape(t *testing.T, gw *Gateway) string {
	t.Helper()
	rr := httptest.NewRecorder()
	gw.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rr.Body)
	return string(b)
}

func TestUpstreamSLAMetrics(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, nil)
	gw := New(WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), RateLimit(1, 1)); err != nil {
		t.Fatal(err)
	}
	h := gw.IngressHandler()
	restGet(t, h, "/things/1", nil)
	restGet(t, h, "/things/1", nil) // refused by the rate limit (no wait budget)
	addr := startGreeter(t, &fakeGreeterServer{})
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(addr), As("greeter")); err != nil {
		t.Fatal(err)
	}
	postGQL(t, gw.Handler(), helloQuery)

	out := scrape(t, gw)
	for _, want := range []string{
		`go_api_gateway_upstream_requests_total{code="200",method="GET /things/{id}",namespace="test",version="v1"} 1`,
		`go_api_gateway_upstream_ttfb_seconds_count{method="GET /things/{id}",namespace="test",version="v1"} 1`,
		`go_api_gateway_upstream_request_bytes_count{method="GET /things/{id}",namespace="test",version="v1"} 1`,
		`go_api_gateway_upstream_response_bytes_sum{method="GET /things/{id}",namespace="test",version="v1"} 27`,
		`go_api_gateway_limit_rejections_total{namespace="test",reason="rate_limit",version="v1"} 1`,
		`go_api_gateway_rate_limit_split_peers 1`,
		`go_api_gateway_upstream_requests_total{code="OK",method="/greeter.v1.GreeterService/Hello",namespace="greeter",version="v1"} 1`,
		`go_api_gateway_upstream_ttfb_seconds_count{method="/greeter.v1.GreeterService/Hello",namespace="greeter",version="v1"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if t.Failed() {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "upstream_") || strings.Contains(l, "limit_") {
				t.Log(l)
			}
		}
	}
}

// The gRPC ingress runs proto routes without the dispatcher
// middlewares; the gate wraps the route's handler instead.
func TestRateLimit_GRPCIngressRoute(t *testing.T) {
	addr := startGreeter(t, &fakeGreeterServer{})
	gw := testGateway(t)
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(addr), As("greeter"), RateLimit(0.5, 1)); err != nil {
		t.Fatal(err)
	}
	gw.Handler() // build the schema and routes
	h := gw.ConnectHandler()
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/greeter.v1.GreeterService/Hello", strings.NewReader(`{"name":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := call(); rr.Code != 200 {
		t.Fatalf("first: %d %s", rr.Code, rr.Body)
	}
	rr := call()
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "resource_exhausted") {
		t.Fatalf("second: %d %s, want 429 resource_exhausted", rr.Code, rr.Body)
	}
}

func TestUpstreamThrottled(t *testing.T) {
	st, err := status.New(codes.ResourceExhausted, "slow down").WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(4 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := upstreamThrottled(st.Err()); !ok || d != 4*time.Second {
		t.Errorf("gRPC RetryInfo: %v %v, want 4s", d, ok)
	}
	if _, ok := upstreamThrottled(status.Error(codes.ResourceExhausted, "no retry info")); ok {
		t.Error("ResourceExhausted without RetryInfo counted as throttled")
	}
	rej := rejectUpstreamStatus(429, "x")
	markThrottled(rej, http.Header{"Retry-After": []string{"7"}})
	if d, ok := upstreamThrottled(rej); !ok || d != 7*time.Second {
		t.Errorf("HTTP 429: %v %v, want 7s", d, ok)
	}
	if _, ok := upstreamThrottled(&rejection{Code: CodeResourceExhausted, status: 429}); ok {
		t.Error("a gateway 429 counted as an upstream 429")
	}
}
