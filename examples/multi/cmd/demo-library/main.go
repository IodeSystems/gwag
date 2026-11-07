// demo-library: one library provider process for demo.sh. Serves the
// lines named by --lines from one gRPC server and registers them in a
// single Register call (one binding per line, one proto package per
// line: library.v1 / library.v2). SIGTERM drains through the gateway
// and exits once the gateway reports safe_to_stop.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	libraryv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/library/v1"
	libraryv2 "github.com/iodesystems/gwag/v2/examples/multi/gen/library/v2"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

type v1Impl struct {
	libraryv1.UnimplementedLibraryServiceServer
	instance string
}

func (s v1Impl) ListBooks(context.Context, *libraryv1.ListBooksRequest) (*libraryv1.ListBooksResponse, error) {
	return &libraryv1.ListBooksResponse{Books: []*libraryv1.Book{{Id: "1", Title: "served by " + s.instance, Author: "Donovan", Year: 2015}}}, nil
}

type v2Impl struct {
	libraryv2.UnimplementedLibraryServiceServer
	instance string
}

func (s v2Impl) ListBooks(context.Context, *libraryv2.ListBooksRequest) (*libraryv2.ListBooksResponse, error) {
	return &libraryv2.ListBooksResponse{Books: []*libraryv2.Book{{Id: "1", Title: "served by " + s.instance, Author: "Donovan", Year: 2015}}}, nil
}

func main() {
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	instance := flag.String("instance", "library-a", "Instance id")
	lines := flag.String("lines", "v1", "Comma-separated lines to serve: v1, v2")
	owner := flag.String("owner", "books", "Owning team")
	flag.Parse()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	var svcs []controlclient.Service
	for _, line := range strings.Split(*lines, ",") {
		file := "protos/library.proto"
		switch line {
		case "v1":
			libraryv1.RegisterLibraryServiceServer(srv, v1Impl{instance: *instance})
		case "v2":
			file = "protos/library_v2.proto"
			libraryv2.RegisterLibraryServiceServer(srv, v2Impl{instance: *instance})
		default:
			log.Fatalf("unknown line %q", line)
		}
		src, err := os.ReadFile(file)
		if err != nil {
			log.Fatalf("read %s (run from examples/multi/): %v", file, err)
		}
		svcs = append(svcs, controlclient.Service{
			Namespace:   "library",
			Version:     line,
			ProtoSource: src,
			Owner:       &controlclient.Owner{Team: *owner, Contact: "#" + *owner},
		})
	}
	go func() { _ = srv.Serve(lis) }()

	_, err = controlclient.SelfRegister(context.Background(), controlclient.Options{
		GatewayAddr:    *gatewayAddr,
		ServiceAddr:    lis.Addr().String(),
		InstanceID:     *instance,
		Services:       svcs,
		DrainOnSIGTERM: true,
		DrainTimeout:   2 * time.Minute,
		OnDrained: func() {
			log.Printf("%s drained and deregistered; exiting", *instance)
			srv.GracefulStop()
			os.Exit(0)
		},
	})
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	log.Printf("%s registered library lines [%s] on %s", *instance, *lines, lis.Addr())
	select {}
}
