package gateway

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Two proto slots declaring greeter.v1.GreeterService: the gRPC
// ingress routes to the first in (namespace, version) order on every
// rebuild, and warns naming both slots.
func TestGRPCIngress_SameProcedureTwoSlots(t *testing.T) {
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)

	backend := func(name string) string {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		greeterv1.RegisterGreeterServiceServer(srv, &fakeGreeterServer{
			helloFn: func(context.Context, *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
				return &greeterv1.HelloResponse{Greeting: name}, nil
			},
		})
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		return lis.Addr().String()
	}
	// zeta registers first, so first-registered and lexical differ.
	for _, ns := range []string{"zeta", "alpha"} {
		if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), To(backend(ns)), As(ns)); err != nil {
			t.Fatal(err)
		}
	}

	feLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	feSrv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = feSrv.Serve(feLis) }()
	t.Cleanup(feSrv.Stop)
	conn, err := grpc.NewClient(feLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cli := greeterv1.NewGreeterServiceClient(conn)

	for i := 0; i < 25; i++ {
		// Each registration rebuilds the schema and the route table.
		if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://127.0.0.1:1"), As(fmt.Sprintf("rebuild%d", i))); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := cli.Hello(ctx, &greeterv1.HelloRequest{Name: "x"})
		cancel()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if resp.GetGreeting() != "alpha" {
			t.Fatalf("call %d answered by %q, want alpha", i, resp.GetGreeting())
		}
	}

	out := logs.String()
	if !strings.Contains(out, "served_by=alpha/") || !strings.Contains(out, "skipped=zeta/") {
		t.Fatalf("warning does not name both slots:\n%s", out)
	}
	// One warning per rebuild: 25 rebuilds plus the registration of
	// alpha itself, and not one per method.
	if n := strings.Count(out, "declared by two slots"); n < 25 || n > 27 {
		t.Fatalf("%d warnings, want one per rebuild (25-27)", n)
	}
}
