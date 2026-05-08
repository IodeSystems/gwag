package gateway

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// grpcIngressFixture spins:
//   - a backend gRPC server (greeter)
//   - a gateway with that backend registered via AddProtoDescriptor
//   - a frontend gRPC server hosting the gateway's UnknownServiceHandler
//   - a client conn pointed at the frontend
//
// Mirrors the http_ingress_test.go fixture shape but on the gRPC
// transport so we can assert ingress doesn't add a JSON detour for
// proto→proto calls.
type grpcIngressFixture struct {
	gw       *Gateway
	greeter  *fakeGreeterServer
	frontend *grpc.Server
	cli      greeterv1.GreeterServiceClient
	conn     *grpc.ClientConn
}

func newGRPCIngressFixture(t *testing.T) *grpcIngressFixture {
	t.Helper()

	// Backend.
	beLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	beSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(beSrv, greeter)
	go func() { _ = beSrv.Serve(beLis) }()
	t.Cleanup(beSrv.Stop)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)

	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(beLis.Addr().String()),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}

	// Frontend gRPC server hosting the gateway's unknown handler.
	feLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("frontend listen: %v", err)
	}
	feSrv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = feSrv.Serve(feLis) }()
	t.Cleanup(feSrv.Stop)

	conn, err := grpc.NewClient(feLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial frontend: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &grpcIngressFixture{
		gw:       gw,
		greeter:  greeter,
		frontend: feSrv,
		cli:      greeterv1.NewGreeterServiceClient(conn),
		conn:     conn,
	}
}

func TestGRPCIngress_UnaryHappyPath(t *testing.T) {
	f := newGRPCIngressFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := f.cli.Hello(ctx, &greeterv1.HelloRequest{Name: "world"})
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if got, want := resp.GetGreeting(), "hello world"; got != want {
		t.Fatalf("greeting=%q want=%q", got, want)
	}
	if got := f.greeter.helloCalls.Load(); got != 1 {
		t.Fatalf("backend calls=%d want=1", got)
	}
	if last := f.greeter.lastReq.Load(); last == nil || last.GetName() != "world" {
		t.Fatalf("backend saw req=%v", last)
	}
}

func TestGRPCIngress_UnknownMethod(t *testing.T) {
	f := newGRPCIngressFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Direct call with a made-up method on a non-registered service.
	err := f.conn.Invoke(ctx, "/missing.Service/Method", &greeterv1.HelloRequest{}, &greeterv1.HelloResponse{})
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code=%v want=Unimplemented (msg=%q)", st.Code(), st.Message())
	}
}

func TestGRPCIngress_UpstreamErrorPropagates(t *testing.T) {
	f := newGRPCIngressFixture(t)
	f.greeter.helloFn = func(_ context.Context, _ *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "no")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.cli.Hello(ctx, &greeterv1.HelloRequest{Name: "x"})
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Fatalf("code=%v want=PermissionDenied", st.Code())
	}
}

func TestGRPCIngress_StreamingMethodNotRouted(t *testing.T) {
	// Server-streaming Greetings should NOT route through gRPC ingress.
	f := newGRPCIngressFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := f.cli.Greetings(ctx, &greeterv1.GreetingsFilter{})
	if err != nil {
		// Some clients return error from stream-open immediately.
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return
		}
		t.Fatalf("unexpected open error: %v", err)
	}
	_, err = stream.Recv()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code=%v want=Unimplemented (msg=%q)", st.Code(), st.Message())
	}
}

func TestGRPCIngress_RuntimeMiddlewareApplies(t *testing.T) {
	// gRPC ingress is proto-native (skips canonical args) but must
	// still run the user runtime chain so transforms apply. Counting
	// middleware verifies the chain saw the dispatch.
	beLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	beSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(beSrv, greeter)
	go func() { _ = beSrv.Serve(beLis) }()
	t.Cleanup(beSrv.Stop)

	var middlewareHits atomic.Int32
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)
	gw.Use(Pair{Runtime: countingMiddleware(&middlewareHits)})

	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(beLis.Addr().String()),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}

	feLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	feSrv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = feSrv.Serve(feLis) }()
	t.Cleanup(feSrv.Stop)

	conn, err := grpc.NewClient(feLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cli := greeterv1.NewGreeterServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Hello(ctx, &greeterv1.HelloRequest{Name: "x"}); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if got := middlewareHits.Load(); got != 1 {
		t.Fatalf("middleware hits=%d want=1", got)
	}
}

// countingMiddleware bumps `n` every time the wrapped Handler runs;
// used to verify gRPC ingress applies the user runtime chain.
func countingMiddleware(n *atomic.Int32) Middleware {
	return func(next Handler) Handler {
		return Handler(func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			n.Add(1)
			return next(ctx, req)
		})
	}
}
