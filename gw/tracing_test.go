package gateway

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "github.com/iodesystems/gwag/examples/multi/gen/greeter/v1"
)

func newTracingFixture(t *testing.T, opts ...Option) (*Gateway, *tracetest.InMemoryExporter, *fakeGreeterServer) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, greeter)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	all := append([]Option{
		WithoutMetrics(),
		WithoutBackpressure(),
		WithAdminToken([]byte("ignored")),
		WithTracer(tp),
	}, opts...)
	gw := New(all...)
	t.Cleanup(gw.Close)

	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
		To(lis.Addr().String()),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	return gw, exp, greeter
}

func findSpan(spans tracetest.SpanStubs, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

func spanAttr(s *tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func spanNamesFor(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

func TestTracing_GraphQLIngressSpan(t *testing.T) {
	gw, exp, _ := newTracingFixture(t)

	mux := http.NewServeMux()
	mux.Handle("/graphql", gw.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"query":"{ greeter { v1 { hello(name: \"world\") { greeting } } } }"}`
	resp, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	spans := exp.GetSpans()
	ingress := findSpan(spans, "gateway.graphql")
	if ingress == nil {
		t.Fatalf("no gateway.graphql span; got=%v", spanNamesFor(spans))
	}
	if got := spanAttr(ingress, attrIngress); got != "graphql" {
		t.Fatalf("gateway.ingress=%q want=graphql", got)
	}
	if got := spanAttr(ingress, attrHTTPMethod); got != http.MethodPost {
		t.Fatalf("http.method=%q want=POST", got)
	}
}

func TestTracing_HTTPIngressSpan(t *testing.T) {
	gw, exp, _ := newTracingFixture(t)

	mux := http.NewServeMux()
	mux.Handle("/", gw.IngressHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/greeter.v1.GreeterService/Hello",
		"application/json", bytes.NewReader([]byte(`{"name":"world"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	spans := exp.GetSpans()
	ingress := findSpan(spans, "gateway.http")
	if ingress == nil {
		t.Fatalf("no gateway.http span; got=%v", spanNamesFor(spans))
	}
	if got := spanAttr(ingress, attrIngress); got != "http" {
		t.Fatalf("gateway.ingress=%q want=http", got)
	}
	if got := spanAttr(ingress, attrNamespace); got != "greeter" {
		t.Fatalf("gateway.namespace=%q want=greeter", got)
	}
}

func TestTracing_GRPCIngressSpan(t *testing.T) {
	gw, exp, _ := newTracingFixture(t)

	feLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("frontend listen: %v", err)
	}
	feSrv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = feSrv.Serve(feLis) }()
	t.Cleanup(feSrv.Stop)

	conn, err := grpc.NewClient(feLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := greeterv1.NewGreeterServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Hello(ctx, &greeterv1.HelloRequest{Name: "world"}); err != nil {
		t.Fatalf("Hello: %v", err)
	}

	spans := exp.GetSpans()
	ingress := findSpan(spans, "gateway.grpc")
	if ingress == nil {
		t.Fatalf("no gateway.grpc span; got=%v", spanNamesFor(spans))
	}
	if got := spanAttr(ingress, attrIngress); got != "grpc" {
		t.Fatalf("gateway.ingress=%q want=grpc", got)
	}
	if got := spanAttr(ingress, attrNamespace); got != "greeter" {
		t.Fatalf("gateway.namespace=%q want=greeter", got)
	}
	if got := spanAttr(ingress, attrRPCSystem); got != "grpc" {
		t.Fatalf("rpc.system=%q want=grpc", got)
	}
}

func TestTracing_HonorsInboundTraceparent(t *testing.T) {
	gw, exp, _ := newTracingFixture(t)

	mux := http.NewServeMux()
	mux.Handle("/graphql", gw.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/graphql",
		strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", traceparent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	ingress := findSpan(exp.GetSpans(), "gateway.graphql")
	if ingress == nil {
		t.Fatalf("no gateway.graphql span")
	}
	if got := ingress.SpanContext.TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("trace_id=%q want continuation of inbound", got)
	}
}

func TestTracing_NoopWhenUnset(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)
	if gw.tracer == nil {
		t.Fatal("tracer must be non-nil even when WithTracer unset")
	}
	if gw.tracer.enabled {
		t.Fatal("tracer.enabled must be false when WithTracer unset")
	}
}
