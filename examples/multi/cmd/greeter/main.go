// Greeter service: starts a real gRPC server on :50051 and self-registers
// with the gateway's control plane. Heartbeats forever; deregisters on
// SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/iodesystems/go-api-gateway/gw/controlclient"
	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

type greeterImpl struct {
	greeterv1.UnimplementedGreeterServiceServer
	delay time.Duration
}

func (g *greeterImpl) Hello(ctx context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	// Surface the X-Source-IP metadata stamped by the gateway's
	// InjectHeader demo (see examples/multi/cmd/gateway/main.go).
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-source-ip"); len(v) > 0 {
			log.Printf("greeter: hello name=%q x-source-ip=%s", req.GetName(), v[0])
		}
	}
	name := req.GetName()
	if name == "" {
		name = "stranger"
	}
	return &greeterv1.HelloResponse{Greeting: "Hello, " + name + "!"}, nil
}

// buildTag is stamped at link time on release builds:
//
//	go build -ldflags "-X 'main.buildTag=v1.2.3'" ./cmd/greeter
//
// Trunk CI omits the -ldflags so the binary registers as `unstable`;
// release CI sets it to the cut version so SelfRegister refuses the
// `unstable` slot. Plan §4 forcing function — see controlclient.Options.
var buildTag string

func main() {
	addr := flag.String("addr", ":50051", "gRPC listen address")
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	advertise := flag.String("advertise", "localhost:50051", "Address to advertise to the gateway")
	version := flag.String("version", "v1", "Service version (v1, v2, ...)")
	delay := flag.Duration("delay", 0, "Artificial delay per Hello call (for backpressure tests)")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, &greeterImpl{delay: *delay})
	go func() {
		log.Printf("greeter gRPC listening on %s", *addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
		GatewayAddr: *gatewayAddr,
		ServiceAddr: *advertise,
		InstanceID:  "greeter@" + *addr,
		BuildTag:    buildTag,
		Services: []controlclient.Service{{
			Namespace:      "greeter",
			Version:        *version,
			FileDescriptor: greeterv1.File_greeter_proto,
			// Each greeter instance handles up to 16 concurrent unary
			// dispatches; the pool aggregates to 64 across all
			// replicas. Demonstrates the per-binding caps that ship
			// in cpv1.ServiceBinding.
			MaxConcurrency:            64,
			MaxConcurrencyPerInstance: 16,
		}},
	})
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	log.Printf("greeter registered with %s", *gatewayAddr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("greeter shutting down")
	_ = reg.Close(context.Background())
	srv.GracefulStop()
}
