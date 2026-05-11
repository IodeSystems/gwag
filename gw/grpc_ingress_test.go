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
	"google.golang.org/grpc/metadata"
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

	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
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

// TestGRPCIngress_ClientStreamingNotRouted asserts client-streaming
// is filtered out (it isn't routable end-to-end). Bidi shares the
// same path. Server-streaming has its own happy-path test below.
func TestGRPCIngress_ClientStreamingNotRouted(t *testing.T) {
	f := newGRPCIngressFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Echo is bidi (client + server streaming) in greeter.proto;
	// rebuildGRPCIngressLocked drops it.
	stream, err := f.cli.Echo(ctx)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return
		}
		t.Fatalf("unexpected open error: %v", err)
	}
	if err := stream.Send(&greeterv1.HelloRequest{}); err != nil {
		// Some clients surface the Unimplemented at first send.
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return
		}
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

// grpcStreamingFixture stands up a NATS-backed gateway behind a
// frontend gRPC server hosting GRPCUnknownHandler so server-streaming
// proto methods can be exercised end-to-end. No backend gRPC server
// needed — the streaming path doesn't dial out (events come through
// NATS), so a nopGRPCConn suffices.
type grpcStreamingFixture struct {
	gw      *Gateway
	cluster *Cluster
	cli     greeterv1.GreeterServiceClient
	conn    *grpc.ClientConn
}

func newGRPCStreamingFixture(t *testing.T, opts ...Option) *grpcStreamingFixture {
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

	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
		To(nopGRPCConn{}),
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

	return &grpcStreamingFixture{
		gw:      gw,
		cluster: cluster,
		cli:     greeterv1.NewGreeterServiceClient(conn),
		conn:    conn,
	}
}

func TestGRPCIngress_ServerStreaming_HappyPath(t *testing.T) {
	f := newGRPCStreamingFixture(t, WithoutSubscriptionAuth())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := f.cli.Greetings(ctx, &greeterv1.GreetingsFilter{Name: "alice"})
	if err != nil {
		t.Fatalf("Greetings: %v", err)
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

	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got.GetGreeting() != "hi alice" {
		t.Fatalf("greeting=%q want %q", got.GetGreeting(), "hi alice")
	}
	if got.GetForName() != "alice" {
		t.Fatalf("forName=%q want %q", got.GetForName(), "alice")
	}
}

func TestGRPCIngress_ServerStreaming_HMACFromMetadata(t *testing.T) {
	// With HMAC enabled, a missing metadata triple → MALFORMED →
	// Unauthenticated. Verifies the metadata lookup is wired.
	secret := []byte("test-secret-32-bytes-long-padding")
	f := newGRPCStreamingFixture(t, WithSubscriptionAuth(SubscriptionAuthOptions{Secret: secret}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Send with bogus HMAC in metadata — verify rejects.
	ctx = metadata.AppendToOutgoingContext(ctx,
		"x-gateway-hmac", "bad",
		"x-gateway-timestamp", "0",
	)
	stream, err := f.cli.Greetings(ctx, &greeterv1.GreetingsFilter{Name: "alice"})
	if err != nil {
		// Stream open may eagerly fail with bad auth.
		return
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected Recv error on bad HMAC, got nil")
	}
	// Subscribe-time errors come through as raw error from the
	// dispatcher — we just verify the stream did NOT successfully
	// receive an event.
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
	gw.Use(Transform{Runtime: countingMiddleware(&middlewareHits)})

	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
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
