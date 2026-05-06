// Gateway binary: serves GraphQL on :8080 and a control plane on :50090.
// Boots empty; services register themselves at runtime.
//
//	$ go run ./cmd/gateway
//	control plane :50090 / graphql :8080
package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"google.golang.org/grpc"

	gateway "github.com/iodesystems/go-api-gateway"
	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

func main() {
	httpAddr := flag.String("http", ":8080", "GraphQL HTTP listen address")
	cpAddr := flag.String("control-plane", ":50090", "Control-plane gRPC listen address")
	flag.Parse()

	gw := gateway.New()

	cpLis, err := net.Listen("tcp", *cpAddr)
	if err != nil {
		log.Fatalf("listen control plane: %v", err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, gw.ControlPlane())
	go func() {
		log.Printf("control plane listening on %s", *cpAddr)
		if err := srv.Serve(cpLis); err != nil {
			log.Fatalf("control plane serve: %v", err)
		}
	}()

	log.Printf("graphql listening on %s", *httpAddr)
	if err := http.ListenAndServe(*httpAddr, gw.Handler()); err != nil {
		log.Fatal(err)
	}
}
