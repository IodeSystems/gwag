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

	"github.com/iodesystems/go-api-gateway/gw/controlclient"
	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

type greeterImpl struct {
	greeterv1.UnimplementedGreeterServiceServer
	delay time.Duration
}

func (g *greeterImpl) Hello(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	name := req.GetName()
	if name == "" {
		name = "stranger"
	}
	return &greeterv1.HelloResponse{Greeting: "Hello, " + name + "!"}, nil
}

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
		Services: []controlclient.Service{
			{Namespace: "greeter", Version: *version, FileDescriptor: greeterv1.File_greeter_proto},
		},
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
