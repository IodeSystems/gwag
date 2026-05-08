package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
