// hello-proto: a tiny native gRPC service self-registering with the
// gateway via the proto control plane. Sibling of hello-openapi +
// hello-graphql; the gateway dispatches `hello.v1.HelloService.Hello`
// to this server.
//
// Exists primarily so bench traffic grpc --direct has a format-native
// upstream that's parallel to its openapi/graphql counterparts. The
// existing greeter service covers proto Hello + subscriptions for the
// README tutorial; hello-proto is the bench-focused minimal twin.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	hellov1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/hello/v1"
	"github.com/iodesystems/go-api-gateway/gw/controlclient"
)

type helloImpl struct {
	hellov1.UnimplementedHelloServiceServer
}

func (*helloImpl) Hello(_ context.Context, req *hellov1.HelloRequest) (*hellov1.HelloResponse, error) {
	return &hellov1.HelloResponse{Greeting: "Hello, " + req.GetName() + "!"}, nil
}

func main() {
	addr := flag.String("addr", ":50055", "gRPC listen address")
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	advertise := flag.String("advertise", "localhost:50055", "Address to advertise to the gateway")
	namespace := flag.String("namespace", "hello_proto", "Namespace to register under")
	version := flag.String("version", "v1", "Service version (unstable / vN)")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	hellov1.RegisterHelloServiceServer(srv, &helloImpl{})
	go func() {
		log.Printf("hello-proto gRPC listening on %s", *addr)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
		GatewayAddr: *gatewayAddr,
		ServiceAddr: *advertise,
		InstanceID:  "hello-proto@" + *addr,
		Services: []controlclient.Service{{
			Namespace:      *namespace,
			Version:        *version,
			FileDescriptor: hellov1.File_hello_proto,
		}},
	})
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	log.Printf("hello-proto registered with %s as %s:%s", *gatewayAddr, *namespace, *version)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("hello-proto shutting down")
	_ = reg.Close(context.Background())
	srv.GracefulStop()
}
