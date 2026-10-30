package gateway

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
)

// benchServerStream is a grpc.ServerStream that hands the handler one
// pre-encoded request and encodes what it sends, so the benchmark
// measures the ingress handler and the upstream call without a
// frontend server and client in the loop.
type benchServerStream struct {
	grpc.ServerStream
	ctx context.Context
	req []byte
}

func (s *benchServerStream) Context() context.Context { return s.ctx }

func (s *benchServerStream) RecvMsg(m any) error {
	return proto.Unmarshal(s.req, m.(proto.Message))
}

func (s *benchServerStream) SendMsg(m any) error {
	_, err := proto.Marshal(m.(proto.Message))
	return err
}

type benchTransportStream struct{ method string }

func (s benchTransportStream) Method() string             { return s.method }
func (benchTransportStream) SetHeader(metadata.MD) error  { return nil }
func (benchTransportStream) SendHeader(metadata.MD) error { return nil }
func (benchTransportStream) SetTrailer(metadata.MD) error { return nil }

// BenchmarkGRPCIngress_Unary measures GRPCUnknownHandler on a unary
// proto→proto call: decode, the ingress wrapper (span, request log,
// dispatch accounting), the upstream call, encode. Metrics and
// backpressure off, as in the dispatch benchmarks.
func BenchmarkGRPCIngress_Unary(b *testing.B) {
	benchGRPCIngress(b, WithoutMetrics(), WithoutBackpressure())
}

// BenchmarkGRPCIngress_UnaryBackpressure adds the default per-pool
// backpressure: a slot taken and released per call.
func BenchmarkGRPCIngress_UnaryBackpressure(b *testing.B) {
	benchGRPCIngress(b, WithoutMetrics())
}

func benchGRPCIngress(b *testing.B, opts ...Option) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	be := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(be, &fakeGreeterServer{})
	go func() { _ = be.Serve(lis) }()
	defer be.Stop()
	gw := New(append(opts, WithAdminToken([]byte("ignored")))...)
	defer gw.Close()
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(b, "greeter.proto"), To(lis.Addr().String()), As("greeter")); err != nil {
		b.Fatal(err)
	}
	h := gw.GRPCUnknownHandler()
	req, err := proto.Marshal(&greeterv1.HelloRequest{Name: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	ctx = grpc.NewContextWithServerTransportStream(ctx, benchTransportStream{"/greeter.v1.GreeterService/Hello"})
	stream := &benchServerStream{ctx: ctx, req: req}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := h(nil, stream); err != nil {
			b.Fatalf("Hello: %v", err)
		}
	}
}

// BenchmarkConnectIngress_Unary is the same call over ConnectHandler
// with the Connect protocol (binary proto), served in-process.
func BenchmarkConnectIngress_Unary(b *testing.B) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	be := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(be, &fakeGreeterServer{})
	go func() { _ = be.Serve(lis) }()
	defer be.Stop()
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	defer gw.Close()
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(b, "greeter.proto"), To(lis.Addr().String()), As("greeter")); err != nil {
		b.Fatal(err)
	}
	h := gw.ConnectHandler()
	body, err := proto.Marshal(&greeterv1.HelloRequest{Name: "bench"})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/greeter.v1.GreeterService/Hello", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/proto")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("Hello: %d %s", rr.Code, rr.Body)
		}
	}
}
