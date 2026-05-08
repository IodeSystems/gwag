package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// httpIngressFixture mirrors newGRPCE2EFixture but mounts the gateway
// at /api/grpc/* (the IngressHandler) in addition to /graphql, so the
// same proto pool can be exercised over both surfaces.
type httpIngressFixture struct {
	gw      *Gateway
	server  *httptest.Server
	greeter *fakeGreeterServer
}

func newHTTPIngressFixture(t *testing.T) *httpIngressFixture {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	grpcSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(grpcSrv, greeter)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)

	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(lis.Addr().String()),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/graphql", gw.Handler())
	mux.Handle("/", gw.IngressHandler())

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &httpIngressFixture{gw: gw, server: srv, greeter: greeter}
}

func TestHTTPIngress_ProtoStyle_HappyPath(t *testing.T) {
	f := newHTTPIngressFixture(t)

	body := strings.NewReader(`{"name":"world"}`)
	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Hello", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := out["greeting"], "hello world"; got != want {
		t.Fatalf("greeting=%v want=%v (out=%v)", got, want, out)
	}
	if f.greeter.helloCalls.Load() != 1 {
		t.Fatalf("backend hello calls=%d want=1", f.greeter.helloCalls.Load())
	}
	if last := f.greeter.lastReq.Load(); last == nil || last.GetName() != "world" {
		t.Fatalf("backend saw req=%v", last)
	}
}

func TestHTTPIngress_ProtoStyle_EmptyBody(t *testing.T) {
	f := newHTTPIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Hello", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// Backend defaults name="" → "hello "
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := out["greeting"], "hello "; got != want {
		t.Fatalf("greeting=%v want=%v", got, want)
	}
}

func TestHTTPIngress_ProtoStyle_UnknownPath(t *testing.T) {
	f := newHTTPIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/DoesNotExist", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want=404", resp.StatusCode)
	}
}

func TestHTTPIngress_ProtoStyle_StreamingMethodNotRouted(t *testing.T) {
	// Server-streaming RPC Greetings should NOT be routable over
	// HTTP/JSON ingress — subscriptions live on a separate transport.
	f := newHTTPIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Greetings", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want=404", resp.StatusCode)
	}
}

func TestHTTPIngress_ProtoStyle_BadJSON(t *testing.T) {
	f := newHTTPIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Hello", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want=400", resp.StatusCode)
	}
}

func TestHTTPIngress_DispatchErrorMapsToHTTPStatus(t *testing.T) {
	f := newHTTPIngressFixture(t)
	f.greeter.helloFn = func(_ context.Context, _ *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		return nil, Reject(CodePermissionDenied, "no")
	}

	resp, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Hello", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// gRPC-side Reject doesn't traverse the wire — backend's
	// PermissionDenied surfaces as a transport error mapped to 500.
	// What matters: ingress doesn't 200 a failed dispatch.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200, got 200")
	}
}

func TestHTTPIngress_GraphQLAndIngressShareDispatcher(t *testing.T) {
	f := newHTTPIngressFixture(t)

	// Hit ingress
	if _, err := http.Post(f.server.URL+"/greeter.v1.GreeterService/Hello", "application/json", strings.NewReader(`{"name":"a"}`)); err != nil {
		t.Fatalf("ingress post: %v", err)
	}
	// Hit graphql
	q := `{"query":"{ greeter { hello(name:\"b\") { greeting } } }"}`
	if _, err := http.Post(f.server.URL+"/graphql", "application/json", strings.NewReader(q)); err != nil {
		t.Fatalf("graphql post: %v", err)
	}
	if got := f.greeter.helloCalls.Load(); got != 2 {
		t.Fatalf("hello calls=%d want=2 (ingress + graphql both reach the same backend)", got)
	}
}

// openAPIIngressFixture wires the openAPIE2EFixture's backend +
// gateway behind the IngressHandler so the same OpenAPI ops can be
// exercised at their declared HTTPMethod/HTTPPath in addition to
// the GraphQL surface.
type openAPIIngressFixture struct {
	*openAPIE2EFixture
	server *httptest.Server
}

func newOpenAPIIngressFixture(t *testing.T) *openAPIIngressFixture {
	t.Helper()
	base := newOpenAPIE2EFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/graphql", base.graphql)
	mux.Handle("/", base.gw.IngressHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &openAPIIngressFixture{openAPIE2EFixture: base, server: srv}
}

func TestHTTPIngress_OpenAPI_GETPathParam(t *testing.T) {
	f := newOpenAPIIngressFixture(t)

	resp, err := http.Get(f.server.URL + "/things/42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Method != http.MethodGet || rec.Path != "/things/42" {
		t.Fatalf("backend got %s %s want GET /things/42", rec.Method, rec.Path)
	}
}

func TestHTTPIngress_OpenAPI_POSTBody(t *testing.T) {
	f := newOpenAPIIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/things", "application/json", strings.NewReader(`{"name":"widget"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Method != http.MethodPost || rec.Path != "/things" {
		t.Fatalf("backend got %s %s want POST /things", rec.Method, rec.Path)
	}
	var bodyJSON map[string]any
	if err := json.Unmarshal(rec.Body, &bodyJSON); err != nil {
		t.Fatalf("backend body not json: %v: %s", err, rec.Body)
	}
	if bodyJSON["name"] != "widget" {
		t.Fatalf("backend body name=%v want widget", bodyJSON["name"])
	}
}

func TestHTTPIngress_OpenAPI_MethodMismatchIs404(t *testing.T) {
	// /things/{id} only declares GET; POST against the templated path
	// should not route to it.
	f := newOpenAPIIngressFixture(t)

	resp, err := http.Post(f.server.URL+"/things/42", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want=404", resp.StatusCode)
	}
}

// sseIngressFixture mirrors subFixture from subscriptions_test.go but
// mounts the IngressHandler so SSE clients can subscribe via plain
// GET. Reuses the NATS-cluster setup so publishGreeting can deliver
// events to the broker.
type sseIngressFixture struct {
	gw      *Gateway
	cluster *Cluster
	server  *httptest.Server
}

func (f *sseIngressFixture) close() {
	f.server.Close()
	f.gw.Close()
	f.cluster.Close()
}

func newSSEIngressFixture(t *testing.T, opts ...Option) *sseIngressFixture {
	t.Helper()
	dir := t.TempDir()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      "test",
		ClientListen:  "127.0.0.1:0",
		ClusterListen: "127.0.0.1:0",
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	allOpts := append([]Option{
		WithCluster(cluster),
		WithoutMetrics(),
		WithoutBackpressure(),
	}, opts...)
	gw := New(allOpts...)
	t.Cleanup(gw.Close)

	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	srv := httptest.NewServer(gw.IngressHandler())
	t.Cleanup(srv.Close)
	return &sseIngressFixture{gw: gw, cluster: cluster, server: srv}
}

// readSSEFrame reads one `data: ...\n\n` frame from a chunked-encoded
// SSE stream and returns the decoded JSON payload. Returns ("", true)
// on `event: complete`. Returns ("", false, err) on error.
func readSSEFrame(t *testing.T, br *bufio.Reader) (payload string, complete bool) {
	t.Helper()
	var (
		ev  string
		dat strings.Builder
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read sse: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			ev = rest
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			if dat.Len() > 0 {
				dat.WriteByte('\n')
			}
			dat.WriteString(rest)
		}
	}
	if ev == "complete" {
		return "", true
	}
	return dat.String(), false
}

func TestHTTPIngress_SSESubscription_HappyPath(t *testing.T) {
	f := newSSEIngressFixture(t, WithoutSubscriptionAuth())
	defer f.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	url := f.server.URL + "/greeter.v1.GreeterService/Greetings?name=alice&hmac=x&timestamp=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q want text/event-stream", got)
	}

	// Wait for the broker to register the subject before publishing.
	deadline := time.Now().Add(2 * time.Second)
	for f.gw.subscriptionBroker().activeSubjectCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("broker never registered subject")
		}
		time.Sleep(20 * time.Millisecond)
	}
	publishGreeting(t, f.cluster, "events.greeter.Greetings.alice", "hi alice", "alice")

	br := bufio.NewReader(resp.Body)
	payload, complete := readSSEFrame(t, br)
	if complete {
		t.Fatal("got `complete` before any data frame")
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		t.Fatalf("decode: %v: %s", err, payload)
	}
	if got := evt["greeting"]; got != "hi alice" {
		t.Fatalf("greeting=%v want %q", got, "hi alice")
	}
	if got := evt["forName"]; got != "alice" {
		t.Fatalf("forName=%v want %q", got, "alice")
	}
}

func TestHTTPIngress_SSESubscription_HMACMismatch(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-padding")
	f := newSSEIngressFixture(t, WithSubscriptionAuth(SubscriptionAuthOptions{Secret: secret}))
	defer f.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wrong HMAC — verify rejects with UNAUTHORIZED before the stream
	// opens. Ingress maps Reject(Unauthenticated) → 401.
	url := f.server.URL + "/greeter.v1.GreeterService/Greetings?name=alice&hmac=bad&timestamp=0&kid="
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// The dispatcher returns the verify error wrapped via gqlerrors —
	// it's not a *rejection, so the handler maps it to 500. What we
	// assert: the request did NOT 200 and did NOT open an SSE stream.
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected non-200 on bad HMAC; got 200 body=%s", body)
	}
	if got := resp.Header.Get("Content-Type"); got == "text/event-stream" {
		t.Fatalf("got SSE stream on bad HMAC")
	}
}

func TestHTTPIngress_OpenAPI_PathParamURLDecoded(t *testing.T) {
	// Inbound %20 should round-trip cleanly: ingress URL-decodes the
	// path segment into args["id"], the OpenAPI egress dispatcher
	// re-encodes via url.PathEscape, and the backend's net/http
	// decodes again — so r.URL.Path arrives as a literal space.
	f := newOpenAPIIngressFixture(t)

	resp, err := http.Get(f.server.URL + "/things/" + "hello%20world")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Path != "/things/hello world" {
		t.Fatalf("backend path=%q want %q", rec.Path, "/things/hello world")
	}
}
