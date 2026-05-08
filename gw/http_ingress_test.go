package gateway

import (
	"bytes"
	"context"
	"encoding/json"
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
