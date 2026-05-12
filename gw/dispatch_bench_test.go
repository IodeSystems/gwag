// Baseline (Ryzen 9 3900X, loopback backend, -benchtime=2s):
//
//   BenchmarkProtoDispatcher_Dispatch     ~185 µs/op   10.9 KB/op   174 allocs/op
//   BenchmarkOpenAPIDispatcher_Dispatch   ~128 µs/op    7.8 KB/op    86 allocs/op
//   BenchmarkProtoSchemaExec              ~430 µs/op   67.8 KB/op   972 allocs/op
//   BenchmarkOpenAPISchemaExec            ~391 µs/op   67.5 KB/op   982 allocs/op
//
// _Dispatch benches isolate the per-call work in front of grpc /
// net/http (they call ir.Dispatcher directly); _SchemaExec benches
// drive the same path through graphql.Do so the executor + resolver
// closure overhead shows up.
//
// IR overhead vs pre-cutover (commit 8fd7de8, just before step 3):
//   * Proto SchemaExec:   431 → 429 µs/op,  973 → 972 allocs/op  — flat
//     (nested namespace shape was already the proto convention).
//   * OpenAPI SchemaExec: 337 → 391 µs/op,  813 → 982 allocs/op  — +16%.
//     Entirely from step 4's flat → nested namespace shape change
//     (`pets_v1_getPet` → `pets.v1.getPet`); the IR machinery itself
//     is invisible. ~50µs / ~170 allocs per request to make the
//     namespace shape consistent across formats.
//
// Drive these down with allocation work on arg unmarshal, response-
// shape map building, and dynamicpb churn. A graphql-mirror
// benchmark is omitted because the forwarder needs a full ResolveInfo
// (selection-set, variables) — not stubbable without dragging in
// graphql-go's parser.
package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IodeSystems/graphql-go"

	greeterv1 "github.com/iodesystems/gwag/examples/multi/gen/greeter/v1"
	"github.com/iodesystems/gwag/gw/ir"
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

// BenchmarkProtoSchemaExec drives the same proto dispatch through
// graphql.Do — the resolver closures emitted by RenderGraphQLRuntime
// run, args walk through ResolveParams, then the dispatcher path
// from BenchmarkProtoDispatcher_Dispatch executes. The delta between
// the two benches is the GraphQL executor + IR-emitted resolver
// overhead per request.
//
// Use this number to compare against pre-cutover commits (where the
// resolver was built by buildPoolMethodField instead of
// RenderGraphQLRuntime). A near-zero delta means IR adds no
// runtime overhead vs the legacy per-format builders.
func BenchmarkProtoSchemaExec(b *testing.B) {
	gw, _, cleanup := newProtoBenchGateway(b)
	defer cleanup()

	schema := gw.schema.Load()
	if schema == nil {
		b.Fatal("schema not built")
	}
	query := `{ greeter { hello(name:"x") { greeting } } }`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := graphql.Do(graphql.Params{Schema: *schema, RequestString: query, Context: context.Background()})
		if len(res.Errors) > 0 {
			b.Fatalf("errors: %v", res.Errors)
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

// BenchmarkOpenAPISchemaExec mirrors BenchmarkProtoSchemaExec for the
// OpenAPI ingest path: graphql.Do drives a getThing query through
// the IR-emitted resolver into the openAPIDispatcher. Compare with
// BenchmarkOpenAPIDispatcher_Dispatch to see executor + resolver
// overhead.
func BenchmarkOpenAPISchemaExec(b *testing.B) {
	gw, cleanup := newOpenAPIBenchGateway(b)
	defer cleanup()

	schema := gw.schema.Load()
	if schema == nil {
		b.Fatal("schema not built")
	}
	query := `{ test { getThing(id:"x") { id name } } }`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := graphql.Do(graphql.Params{Schema: *schema, RequestString: query, Context: context.Background()})
		if len(res.Errors) > 0 {
			b.Fatalf("errors: %v", res.Errors)
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
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(b, "greeter.proto"), To(lis.Addr().String()), As("greeter")); err != nil {
		grpcSrv.Stop()
		b.Fatalf("AddProtoBytes: %v", err)
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
