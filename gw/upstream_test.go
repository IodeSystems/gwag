package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// deadAddr returns a 127.0.0.1 address nothing listens on, so a dial
// gets connection refused.
func deadAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func postGQL(t *testing.T, h http.Handler, query string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Body.String()
}

func testGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t"))}, opts...)...)
	t.Cleanup(gw.Close)
	return gw
}

// countingBackend is an OpenAPI backend that counts deliveries and
// replies via fn (default: 200 with a thing).
func countingBackend(t *testing.T, hits *atomic.Int32, fn func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fn != nil {
			fn(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const getThingQuery = `{ test { getThing(id:"1") { id } } }`

// startGreeter runs an in-process greeter gRPC server.
func startGreeter(t *testing.T, fake *fakeGreeterServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

const helloQuery = `{ greeter { hello(name:"w") { greeting } } }`

// ---------------------------------------------------------------------
// Timeouts
// ---------------------------------------------------------------------

func TestUpstreamTimeout_OpenAPI_ServiceOption(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	gw := testGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), Timeout(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	body := postGQL(t, gw.Handler(), getThingQuery)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout did not fire: %s", el)
	}
	if !strings.Contains(body, "upstream timeout after 100ms") {
		t.Fatalf("want timeout error, got %s", body)
	}
	if hits.Load() != 1 {
		t.Fatalf("deliveries=%d want 1", hits.Load())
	}
}

func TestUpstreamTimeout_GRPC_GatewayDefault(t *testing.T) {
	var calls atomic.Int32
	addr := startGreeter(t, &fakeGreeterServer{helloFn: func(ctx context.Context, _ *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	gw := testGateway(t, WithUpstreamTimeout(100*time.Millisecond))
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(addr), As("greeter")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	body := postGQL(t, gw.Handler(), helloQuery)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout did not fire: %s", el)
	}
	if !strings.Contains(body, "upstream timeout after 100ms") {
		t.Fatalf("want timeout error, got %s", body)
	}
	if calls.Load() != 1 {
		t.Fatalf("deliveries=%d want 1", calls.Load())
	}
}

func TestUpstreamTimeout_ServiceOptionBeatsGatewayDefault(t *testing.T) {
	addr := startGreeter(t, &fakeGreeterServer{helloFn: func(ctx context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		select {
		case <-time.After(150 * time.Millisecond):
			return &greeterv1.HelloResponse{Greeting: "late"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	gw := testGateway(t, WithUpstreamTimeout(50*time.Millisecond))
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(addr), As("greeter"), Timeout(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	body := postGQL(t, gw.Handler(), helloQuery)
	if !strings.Contains(body, `"late"`) {
		t.Fatalf("service Timeout should beat gateway default, got %s", body)
	}
}

func TestUpstreamTimeout_GraphQL(t *testing.T) {
	rf := newRemoteFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rf.queryHandler = func(string, map[string]any) any {
		<-release
		return nil
	}
	gw := testGateway(t)
	if err := gw.AddGraphQL(rf.server.URL, As("pets"), Timeout(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	body := postGQL(t, gw.Handler(), `{ pets { users { id } } }`)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout did not fire: %s", el)
	}
	if !strings.Contains(body, "upstream timeout after 100ms") {
		t.Fatalf("want timeout error, got %s", body)
	}
}

func TestUpstreamTimeout_ControlPlaneField(t *testing.T) {
	addr := startGreeter(t, &fakeGreeterServer{})
	gw := testGateway(t, WithUpstreamTimeout(time.Second))
	if _, err := gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr: addr,
		Services: []*cpv1.ServiceBinding{{
			Namespace:   "greeter",
			Version:     "v1",
			ProtoSource: testProtoBytes(t, "greeter.proto"),
			TimeoutMs:   250,
		}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	gw.mu.Lock()
	p := gw.protoSlot(poolKey{namespace: "greeter", version: "v1"})
	gw.mu.Unlock()
	if p == nil {
		t.Fatal("pool not created")
	}
	if p.policy.timeout != 250*time.Millisecond {
		t.Fatalf("policy.timeout=%s want 250ms", p.policy.timeout)
	}
}

// ---------------------------------------------------------------------
// Retry only when the request never reached an upstream
// ---------------------------------------------------------------------

func TestUpstreamRetry_GRPC_ConnectRefusedGoesToOtherReplica(t *testing.T) {
	live := &fakeGreeterServer{}
	liveAddr := startGreeter(t, live)
	gw := testGateway(t)
	proto := testProtoBytes(t, "greeter.proto")
	// Dead replica registered first: lowest-inflight ties go to it.
	if err := gw.AddProtoBytes("greeter.proto", proto, To(deadAddr(t)), As("greeter")); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddProtoBytes("greeter.proto", proto, To(liveAddr), As("greeter")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	for i := 0; i < 3; i++ {
		body := postGQL(t, h, helloQuery)
		if !strings.Contains(body, `"hello w"`) {
			t.Fatalf("call %d: want retry onto live replica, got %s", i, body)
		}
	}
	if live.helloCalls.Load() != 3 {
		t.Fatalf("live calls=%d want 3", live.helloCalls.Load())
	}
}

func TestUpstreamRetry_GRPC_ServerUnavailableNotRetried(t *testing.T) {
	var total atomic.Int32
	fn := func(context.Context, *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		total.Add(1)
		return nil, status.Error(codes.Unavailable, "draining")
	}
	a := startGreeter(t, &fakeGreeterServer{helloFn: fn})
	b := startGreeter(t, &fakeGreeterServer{helloFn: fn})
	gw := testGateway(t)
	proto := testProtoBytes(t, "greeter.proto")
	for _, addr := range []string{a, b} {
		if err := gw.AddProtoBytes("greeter.proto", proto, To(addr), As("greeter")); err != nil {
			t.Fatal(err)
		}
	}
	body := postGQL(t, gw.Handler(), helloQuery)
	if !strings.Contains(body, "draining") {
		t.Fatalf("want upstream error surfaced, got %s", body)
	}
	if total.Load() != 1 {
		t.Fatalf("deliveries=%d want exactly 1 (delivered request must not be retried)", total.Load())
	}
}

func TestUpstreamRetry_GRPC_TimeoutNotRetried(t *testing.T) {
	var total atomic.Int32
	fn := func(ctx context.Context, _ *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		total.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a := startGreeter(t, &fakeGreeterServer{helloFn: fn})
	b := startGreeter(t, &fakeGreeterServer{helloFn: fn})
	gw := testGateway(t, WithUpstreamTimeout(100*time.Millisecond))
	proto := testProtoBytes(t, "greeter.proto")
	for _, addr := range []string{a, b} {
		if err := gw.AddProtoBytes("greeter.proto", proto, To(addr), As("greeter")); err != nil {
			t.Fatal(err)
		}
	}
	body := postGQL(t, gw.Handler(), helloQuery)
	if !strings.Contains(body, "upstream timeout") {
		t.Fatalf("want timeout, got %s", body)
	}
	if total.Load() != 1 {
		t.Fatalf("deliveries=%d want exactly 1", total.Load())
	}
}

func TestUpstreamRetry_OpenAPI_ConnectRefusedGoesToOtherReplica(t *testing.T) {
	var hits atomic.Int32
	live := countingBackend(t, &hits, nil)
	gw := testGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://"+deadAddr(t)), As("test")); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(live.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	for i := 0; i < 4; i++ {
		body := postGQL(t, h, getThingQuery)
		if !strings.Contains(body, `"abc"`) {
			t.Fatalf("call %d: want retry onto live replica, got %s", i, body)
		}
	}
	if hits.Load() != 4 {
		t.Fatalf("live hits=%d want 4", hits.Load())
	}
}

func TestUpstreamRetry_OpenAPI_DeliveredNotRetried(t *testing.T) {
	cases := map[string]struct {
		reply   func(http.ResponseWriter, *http.Request)
		opts    []ServiceOption
		wantErr string
	}{
		"503": {
			reply:   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantErr: "503",
		},
		"hang past timeout": {
			reply:   func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
			opts:    []ServiceOption{Timeout(100 * time.Millisecond)},
			wantErr: "upstream timeout",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			a := countingBackend(t, &hits, tc.reply)
			b := countingBackend(t, &hits, tc.reply)
			gw := testGateway(t)
			for _, u := range []string{a.URL, b.URL} {
				opts := append([]ServiceOption{To(u), As("test")}, tc.opts...)
				if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), opts...); err != nil {
					t.Fatal(err)
				}
			}
			body := postGQL(t, gw.Handler(), getThingQuery)
			if !strings.Contains(body, tc.wantErr) {
				t.Fatalf("want %q, got %s", tc.wantErr, body)
			}
			if hits.Load() != 1 {
				t.Fatalf("deliveries=%d want exactly 1", hits.Load())
			}
		})
	}
}

func TestUpstreamRetry_GraphQL_ConnectRefusedGoesToOtherReplica(t *testing.T) {
	dead := newRemoteFixture(t)
	live := newRemoteFixture(t)
	live.queryHandler = func(string, map[string]any) any {
		return map[string]any{"users": []any{map[string]any{"id": "u1"}}}
	}
	// Own transport so idle conns from introspection can be dropped
	// after the dead replica's server closes.
	tr := &http.Transport{}
	gw := testGateway(t, WithOpenAPIClient(&http.Client{Transport: tr}))
	if err := gw.AddGraphQL(dead.server.URL, As("pets")); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddGraphQL(live.server.URL, As("pets")); err != nil {
		t.Fatal(err)
	}
	dead.server.Close()
	tr.CloseIdleConnections()
	h := gw.Handler()
	for i := 0; i < 4; i++ {
		body := postGQL(t, h, `{ pets { users { id } } }`)
		if !strings.Contains(body, `"u1"`) {
			t.Fatalf("call %d: want retry onto live replica, got %s", i, body)
		}
	}
}

// ---------------------------------------------------------------------
// Ejection
// ---------------------------------------------------------------------

func TestReplicaEjection_SkipsThenReadmits(t *testing.T) {
	var badHits, goodHits atomic.Int32
	bad := countingBackend(t, &badHits, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	good := countingBackend(t, &goodHits, nil)
	const cooldown = 300 * time.Millisecond
	gw := testGateway(t, WithReplicaEjection(2, cooldown))
	for _, u := range []string{bad.URL, good.URL} {
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(u), As("test")); err != nil {
			t.Fatal(err)
		}
	}
	h := gw.Handler()
	call := func(n int) {
		for i := 0; i < n; i++ {
			postGQL(t, h, getThingQuery)
		}
	}
	// Round-robin until bad has 2 consecutive failures, then ejected.
	call(10)
	if got := badHits.Load(); got != 2 {
		t.Fatalf("bad hits after ejection=%d want 2", got)
	}
	call(10)
	if got := badHits.Load(); got != 2 {
		t.Fatalf("ejected replica got traffic: hits=%d want 2", got)
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	// Readmitted: one more try, fails, re-ejected at once.
	call(10)
	if got := badHits.Load(); got != 3 {
		t.Fatalf("bad hits after cooldown=%d want 3 (readmit, fail, re-eject)", got)
	}
	if goodHits.Load() != 27 {
		t.Fatalf("good hits=%d want 27", goodHits.Load())
	}
}

func TestReplicaEjection_FailOpenSingleReplica(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	gw := testGateway(t, WithReplicaEjection(1, time.Hour))
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	for i := 0; i < 5; i++ {
		postGQL(t, h, getThingQuery)
	}
	if hits.Load() != 5 {
		t.Fatalf("hits=%d want 5 (all-ejected must still dispatch)", hits.Load())
	}
}

func TestReplicaEjection_SuccessResetsStreak(t *testing.T) {
	p := &upstreamPolicy{ejectAfter: 3, ejectFor: time.Hour}
	var h replicaHealth
	h.failure(p)
	h.failure(p)
	h.success()
	h.failure(p)
	h.failure(p)
	if h.ejectedUntil.Load() != 0 {
		t.Fatal("streak broken by success must not eject")
	}
	h.failure(p)
	if h.ejectedUntil.Load() == 0 {
		t.Fatal("3 consecutive failures must eject")
	}
}

func TestPickHealthy_PrefersNonEjectedAndExcludesTried(t *testing.T) {
	a, b, c := &replica{addr: "a"}, &replica{addr: "b"}, &replica{addr: "c"}
	rs := []*replica{a, b, c}
	a.health.ejectedUntil.Store(time.Now().Add(time.Hour).UnixNano())
	b.inflight.Store(5)
	if got, _ := pickHealthy(rs, 0, nil); got != c {
		t.Fatalf("got %s want c (a ejected, b busier)", got.addr)
	}
	if got, _ := pickHealthy(rs, 0, []*replica{c}); got != b {
		t.Fatalf("got %s want b (c tried)", got.addr)
	}
	if got, _ := pickHealthy(rs, 0, []*replica{b, c}); got != a {
		t.Fatalf("got %s want a (fail open)", got.addr)
	}
	if _, ok := pickHealthy(rs, 0, []*replica{a, b, c}); ok {
		t.Fatal("all tried must return ok=false")
	}
}

func TestPickHealthy_NoAllocs(t *testing.T) {
	rs := []*replica{{}, {}, {}}
	if n := testing.AllocsPerRun(100, func() { _, _ = pickHealthy(rs, 1, nil) }); n != 0 {
		t.Fatalf("allocs=%v want 0", n)
	}
}

// The upstream's copy of the deadline can reset the stream before
// callCtx's timer fires; that is still the policy timeout.
func TestGRPCUpstreamTimedOut(t *testing.T) {
	p := &upstreamPolicy{timeout: 100 * time.Millisecond}
	dl := status.Error(codes.DeadlineExceeded, "stream terminated by RST_STREAM")
	near, cancelNear := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelNear()
	far, cancelFar := context.WithTimeout(context.Background(), time.Second)
	defer cancelFar()
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	for name, c := range map[string]struct {
		parent, call context.Context
		p            *upstreamPolicy
		err          error
		want         bool
	}{
		"near deadline":     {context.Background(), near, p, dl, true},
		"upstream's own":    {context.Background(), far, p, dl, false},
		"other code":        {context.Background(), near, p, status.Error(codes.Unavailable, "x"), false},
		"caller gone":       {done, near, p, dl, false},
		"no policy timeout": {context.Background(), near, &upstreamPolicy{}, dl, false},
	} {
		if got := grpcUpstreamTimedOut(c.parent, c.call, c.p, c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}
