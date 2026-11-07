package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
)

const mp = "go_api_gateway_"

func metricsGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	gw := New(append([]Option{WithoutBackpressure(), WithAdminToken([]byte("t"))}, opts...)...)
	t.Cleanup(gw.Close)
	return gw
}

func scrapeBody(t *testing.T, gw *Gateway) string {
	t.Helper()
	rr := httptest.NewRecorder()
	gw.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rr.Body.String()
}

// series returns the sample value of an exact series line ("" when
// absent). Labels must be in the exposition order (alphabetical).
func series(body, name string) string {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, name+" ") {
			return strings.TrimPrefix(l, name+" ")
		}
	}
	return ""
}

func wantSeries(t *testing.T, body, name, want string) {
	t.Helper()
	if got := series(body, name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func postRaw(h http.Handler, ct, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func gqlBody(q string) string { return `{"query":` + jsonQuote(q) + `}` }

func thingBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTrafficMetrics_GraphQLOutcomes(t *testing.T) {
	be := thingBackend(t)
	gw := metricsGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	ok := postRaw(h, "application/json", gqlBody(`{ test { getThing(id:"1") { id } } }`))
	postRaw(h, "application/json", gqlBody(`{ test { getThing(id:"1") { id } } }`))
	postRaw(h, "application/json", gqlBody(`{ test {`))                                        // parse
	postRaw(h, "application/json", gqlBody(`{ nope }`))                                        // validate
	partial := postRaw(h, "application/json", gqlBody(`{ test { getThing(id:"2") { id } } }`)) // upstream 500
	if !strings.Contains(partial.Body.String(), `"data"`) || !strings.Contains(partial.Body.String(), `"errors"`) {
		t.Fatalf("want data and errors, got %s", partial.Body.String())
	}
	body := scrapeBody(t, gw)

	wantSeries(t, body, mp+`graphql_errors_total{kind="parse"}`, "1")
	wantSeries(t, body, mp+`graphql_errors_total{kind="validate"}`, "1")
	wantSeries(t, body, mp+`graphql_errors_total{kind="execution"}`, "1")
	wantSeries(t, body, mp+`graphql_partial_responses_total`, "1")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="200",ingress="graphql",operation_type="query"}`, "3")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="200",ingress="graphql",operation_type=""}`, "2")
	wantSeries(t, body, mp+`request_self_seconds_count{ingress="graphql"}`, "5")
	wantSeries(t, body, mp+`request_size_bytes_count{ingress="graphql"}`, "5")
	wantSeries(t, body, mp+`response_size_bytes_count{ingress="graphql"}`, "5")
	wantSeries(t, body, mp+`requests_in_progress{ingress="graphql"}`, "0")
	// Sizes are byte counts, not buffers: first response length.
	if got := ok.Body.Len(); got == 0 {
		t.Fatal("empty response")
	}
	if s := series(body, mp+`request_size_bytes_sum{ingress="graphql"}`); s == "" || s == "0" {
		t.Errorf("request size sum = %q", s)
	}
}

func TestTrafficMetrics_Rejections(t *testing.T) {
	trusted := map[string]string{"q1": `{ test { getThing(id:"1") { id } } }`}
	big := `{"query":"` + strings.Repeat("a", 300) + `"}`
	cases := []struct {
		name       string
		opts       []Option
		ct, body   string
		reason     string
		code       string
		wantErrors string // graphql_errors_total validate count
	}{
		{"max_request_bytes", []Option{WithMaxRequestBytes(100)}, "application/json", big, "max_request_bytes", "413", "0"},
		{"malformed_body", nil, "application/json", `{`, "malformed_body", "400", "0"},
		{"max_depth", []Option{WithMaxDepth(2)}, "application/json", gqlBody(`{ test { getThing(id:"1") { id } } }`), "max_depth", "200", "0"},
		{"max_operations", []Option{WithMaxOperations(1)}, "application/json",
			gqlBody(`{ a: test { getThing(id:"1") { id } } b: test { getThing(id:"1") { id } } }`), "max_operations", "200", "0"},
		{"introspection_disabled", []Option{WithoutIntrospection()}, "application/json", gqlBody(`{ __schema { queryType { name } } }`), "introspection_disabled", "200", "0"},
		{"trusted_required", []Option{WithTrustedDocuments(trusted, true)}, "application/json", gqlBody(`{ test { getThing(id:"1") { id } } }`), "trusted_required", "400", "0"},
		{"trusted_unknown", []Option{WithTrustedDocuments(trusted, true)}, "application/json", `{"documentId":"zzz"}`, "trusted_unknown", "200", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := metricsGateway(t, c.opts...)
			if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://upstream.invalid"), As("test")); err != nil {
				t.Fatal(err)
			}
			postRaw(gw.Handler(), c.ct, c.body)
			body := scrapeBody(t, gw)
			wantSeries(t, body, mp+`requests_rejected_total{ingress="graphql",reason="`+c.reason+`"}`, "1")
			wantSeries(t, body, mp+`request_duration_seconds_count{code="`+c.code+`",ingress="graphql",operation_type=""}`, "1")
			// A refusal is not also a query defect.
			for _, k := range []string{"parse", "validate", "execution"} {
				wantSeries(t, body, mp+`graphql_errors_total{kind="`+k+`"}`, "0")
			}
		})
	}
}

func TestTrafficMetrics_InProgressAndMutation(t *testing.T) {
	release := make(chan struct{})
	var entered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	defer srv.Close()
	gw := metricsGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(srv.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		postRaw(gw.Handler(), "application/json", gqlBody(`mutation { test { createThing(body:{name:"x"}) { id } } }`))
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for entered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	wantSeries(t, scrapeBody(t, gw), mp+`requests_in_progress{ingress="graphql"}`, "1")
	close(release)
	<-done
	body := scrapeBody(t, gw)
	wantSeries(t, body, mp+`requests_in_progress{ingress="graphql"}`, "0")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="200",ingress="graphql",operation_type="mutation"}`, "1")
}

func TestTrafficMetrics_RESTIngress(t *testing.T) {
	be := thingBackend(t)
	gw := metricsGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	h := gw.IngressHandler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/things/1", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/nope", nil))
	body := scrapeBody(t, gw)
	wantSeries(t, body, mp+`request_duration_seconds_count{code="200",ingress="http",operation_type=""}`, "1")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="404",ingress="http",operation_type=""}`, "1")
	wantSeries(t, body, mp+`response_size_bytes_count{ingress="http"}`, "2")
	wantSeries(t, body, mp+`response_size_bytes_sum{ingress="http"}`, strconv.Itoa(rr.Body.Len()+rr2.Body.Len()))
}

func greeterGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	be := &fakeGreeterServer{}
	addr := startGreeter(t, be)
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(addr), As("greeter")); err != nil {
		t.Fatal(err)
	}
	return gw, addr
}

func TestTrafficMetrics_GRPCAndConnect(t *testing.T) {
	gw, _ := greeterGateway(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fe := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = fe.Serve(lis) }()
	t.Cleanup(fe.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := greeterv1.NewGreeterServiceClient(conn).Hello(ctx, &greeterv1.HelloRequest{Name: "w"}); err != nil {
		t.Fatal(err)
	}
	// Unknown method: Unimplemented.
	_ = conn.Invoke(ctx, "/missing.Service/Method", &greeterv1.HelloRequest{}, &greeterv1.HelloResponse{})

	url := serveConnect(t, gw.ConnectHandler())
	c := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](http.DefaultClient, url+"/greeter.v1.GreeterService/Hello")
	if _, err := c.CallUnary(ctx, connect.NewRequest(&greeterv1.HelloRequest{Name: "w"})); err != nil {
		t.Fatal(err)
	}
	body := scrapeBody(t, gw)
	wantSeries(t, body, mp+`request_duration_seconds_count{code="OK",ingress="grpc",operation_type=""}`, "1")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="Unimplemented",ingress="grpc",operation_type=""}`, "1")
	wantSeries(t, body, mp+`request_duration_seconds_count{code="200",ingress="connect",operation_type=""}`, "1")
	wantSeries(t, body, mp+`request_self_seconds_count{ingress="connect"}`, "1")
	wantSeries(t, body, mp+`request_self_seconds_count{ingress="grpc"}`, "1")
	if s := series(body, mp+`request_size_bytes_sum{ingress="grpc"}`); s == "" || s == "0" {
		t.Errorf("grpc request size sum = %q", s)
	}
	if s := series(body, mp+`response_size_bytes_sum{ingress="connect"}`); s == "" || s == "0" {
		t.Errorf("connect response size sum = %q", s)
	}
	wantSeries(t, body, mp+`requests_in_progress{ingress="grpc"}`, "0")
	wantSeries(t, body, mp+`requests_in_progress{ingress="connect"}`, "0")
}

func TestTrafficMetrics_RetryTimeoutEjection(t *testing.T) {
	var badHits, goodHits atomic.Int32
	bad := countingBackend(t, &badHits, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	good := countingBackend(t, &goodHits, nil)
	gw := metricsGateway(t, WithReplicaEjection(2, time.Minute))
	for _, u := range []string{bad.URL, good.URL} {
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(u), As("test")); err != nil {
			t.Fatal(err)
		}
	}
	h := gw.Handler()
	for i := 0; i < 10; i++ {
		postGQL(t, h, getThingQuery)
	}
	body := scrapeBody(t, gw)
	wantSeries(t, body, mp+`replica_ejections_total{namespace="test",replica="`+bad.URL+`",version="v1"}`, "1")
	wantSeries(t, body, mp+`replicas_ejected{namespace="test",version="v1"}`, "1")
	wantSeries(t, body, mp+`registered_services{kind="openapi"}`, "1")
	wantSeries(t, body, mp+`registrations_total{kind="openapi",outcome="created"}`, "1")
	wantSeries(t, body, mp+`registrations_total{kind="openapi",outcome="joined"}`, "1")
	wantSeries(t, body, mp+`cluster_peers`, "0")
	if s := series(body, mp+`schema_rebuilds_total{result="ok"}`); s == "" || s == "0" {
		t.Errorf("schema_rebuilds_total = %q", s)
	}
	if series(body, mp+`schema_rebuild_duration_seconds_count`) == "" {
		t.Error("no rebuild duration histogram")
	}
}

func TestTrafficMetrics_RetryConnectRefused(t *testing.T) {
	var hits atomic.Int32
	live := countingBackend(t, &hits, nil)
	gw := metricsGateway(t, WithReplicaEjection(0, 0))
	for _, u := range []string{"http://" + deadAddr(t), live.URL} {
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(u), As("test")); err != nil {
			t.Fatal(err)
		}
	}
	h := gw.Handler()
	for i := 0; i < 4; i++ {
		postGQL(t, h, getThingQuery)
	}
	if hits.Load() != 4 {
		t.Fatalf("live hits=%d", hits.Load())
	}
	// Idle ties go to the first registered replica: the dead one, every
	// call, then a retry onto the live one.
	wantSeries(t, scrapeBody(t, gw), mp+`upstream_retries_total{namespace="test",reason="no_connection",version="v1"}`, "4")
}

func TestTrafficMetrics_UpstreamTimeout(t *testing.T) {
	var hits atomic.Int32
	be := countingBackend(t, &hits, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	gw := metricsGateway(t)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test"), Timeout(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	postGQL(t, gw.Handler(), getThingQuery)
	wantSeries(t, scrapeBody(t, gw), mp+`upstream_timeouts_total{namespace="test",version="v1"}`, "1")
}

func TestTrafficMetrics_BuildInfoAndOff(t *testing.T) {
	gw := metricsGateway(t)
	body := scrapeBody(t, gw)
	found := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, mp+"build_info{") && strings.Contains(l, `go_version="go`) && strings.HasSuffix(l, " 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("no build_info = 1 line:\n%s", body)
	}
	off := New(WithoutMetrics(), WithAdminToken([]byte("t")))
	t.Cleanup(off.Close)
	if off.tm != nil {
		t.Error("tm allocated with WithoutMetrics")
	}
	if off.instrument(ingGraphQL, false, http.NotFoundHandler()) == nil {
		t.Error("nil handler")
	}
}
