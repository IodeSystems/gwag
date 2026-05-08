// Baseline (Ryzen 9 3900X, loopback backend, -benchtime=2s):
//
//   BenchmarkProtoDispatcher_Dispatch     ~186 µs/op   10.9 KB/op   178 allocs/op
//   BenchmarkOpenAPIDispatcher_Dispatch   ~131 µs/op    7.8 KB/op    86 allocs/op
//
// Captured before any optimisation pass. Drive these down with the
// allocation work in plan.md (arg unmarshal, response-shape map
// building, dynamicpb churn). A graphql-mirror benchmark is omitted
// because the forwarder needs a full ResolveInfo (selection-set,
// variables) — not stubbable without dragging in graphql-go's parser.
package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
	"github.com/iodesystems/go-api-gateway/gw/ir"
	"google.golang.org/grpc"
)

// BenchmarkProtoDispatcher_Dispatch measures the proto dispatch hot
// path: canonical args → dynamicpb marshal → user middleware chain
// (empty here) → pickReplica → grpc.Invoke → dynamicpb unmarshal →
// canonical map. The grpc backend runs in-process on 127.0.0.1:0 to
// keep the loop bounded by allocation + reflection cost rather than
// network jitter.
//
// Read the result as the per-dispatch overhead the gateway adds on
// top of a raw grpc.ClientConn.Invoke. Use this number as the
// baseline before any optimisation pass; a future codegen path
// should drive it down.
func BenchmarkProtoDispatcher_Dispatch(b *testing.B) {
	gw, _, cleanup := newProtoBenchGateway(b)
	defer cleanup()

	disp := gw.dispatchers.Get(ir.MakeSchemaID("greeter", "v1", "Hello"))
	if disp == nil {
		b.Fatal("greeter dispatcher missing")
	}
	ctx := context.Background()
	args := map[string]any{"name": "bench"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := disp.Dispatch(ctx, args); err != nil {
			b.Fatalf("Dispatch: %v", err)
		}
	}
}

// BenchmarkOpenAPIDispatcher_Dispatch covers the HTTP/JSON dispatch
// hot path: canonical args → URL substitution → http.Do → JSON
// decode → canonical map. The backend is httptest with a static
// JSON response.
func BenchmarkOpenAPIDispatcher_Dispatch(b *testing.B) {
	gw, cleanup := newOpenAPIBenchGateway(b)
	defer cleanup()

	disp := gw.dispatchers.Get(ir.MakeSchemaID("test", "v1", "getThing"))
	if disp == nil {
		b.Fatal("openapi dispatcher missing")
	}
	ctx := context.Background()
	args := map[string]any{"id": "x"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := disp.Dispatch(ctx, args); err != nil {
			b.Fatalf("Dispatch: %v", err)
		}
	}
}

// newProtoBenchGateway sets up an in-process grpc backend + a
// gateway pointing at it. Mirrors newGRPCE2EFixture but takes
// *testing.B and returns a cleanup func instead of registering with
// t.Cleanup.
func newProtoBenchGateway(b *testing.B) (*Gateway, *fakeGreeterServer, func()) {
	b.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	grpcSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(grpcSrv, greeter)
	go func() { _ = grpcSrv.Serve(lis) }()

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	if err := gw.AddProtoDescriptor(greeterv1.File_greeter_proto, To(lis.Addr().String()), As("greeter")); err != nil {
		grpcSrv.Stop()
		b.Fatalf("AddProtoDescriptor: %v", err)
	}
	// Force the initial schema build so dispatchers populate the
	// registry — joinPoolLocked otherwise defers the first assemble
	// until Handler() is called.
	gw.mu.Lock()
	if err := gw.assembleLocked(); err != nil {
		gw.mu.Unlock()
		grpcSrv.Stop()
		b.Fatalf("assembleLocked: %v", err)
	}
	gw.mu.Unlock()
	cleanup := func() {
		gw.Close()
		grpcSrv.Stop()
	}
	return gw, greeter, cleanup
}

// newOpenAPIBenchGateway sets up an httptest backend + a gateway
// pointing at it. Mirrors newOpenAPIE2EFixture but takes *testing.B.
func newOpenAPIBenchGateway(b *testing.B) (*Gateway, func()) {
	b.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","name":"thing"}`))
	}))
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		backend.Close()
		gw.Close()
		b.Fatalf("AddOpenAPIBytes: %v", err)
	}
	gw.mu.Lock()
	if err := gw.assembleLocked(); err != nil {
		gw.mu.Unlock()
		backend.Close()
		gw.Close()
		b.Fatalf("assembleLocked: %v", err)
	}
	gw.mu.Unlock()
	cleanup := func() {
		gw.Close()
		backend.Close()
	}
	return gw, cleanup
}
