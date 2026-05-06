// Gateway binary: serves GraphQL on :8080 and a control plane on :50090.
// Boots empty; services register themselves at runtime. Optionally
// embeds a NATS server and forms a cluster with peer gateways.
//
//	# single-node
//	$ go run ./cmd/gateway --nats-data /tmp/gw1
//
//	# 3-node, gateway 1 is the seed
//	$ go run ./cmd/gateway --nats-data /tmp/gw1 \
//	    --nats-listen :14222 --nats-cluster :14248
//	$ go run ./cmd/gateway --nats-data /tmp/gw2 \
//	    --nats-listen :14223 --nats-cluster :14249 \
//	    --http :8081 --control-plane :50091 --nats-peer localhost:14248
//	$ go run ./cmd/gateway --nats-data /tmp/gw3 \
//	    --nats-listen :14224 --nats-cluster :14250 \
//	    --http :8082 --control-plane :50092 --nats-peer localhost:14248
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	gateway "github.com/iodesystems/go-api-gateway"
	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	httpAddr := flag.String("http", ":8080", "GraphQL HTTP listen address")
	cpAddr := flag.String("control-plane", ":50090", "Control-plane gRPC listen address")
	natsListen := flag.String("nats-listen", ":14222", "Embedded NATS client listen address")
	natsCluster := flag.String("nats-cluster", ":14248", "Embedded NATS cluster route listen address")
	natsData := flag.String("nats-data", "", "JetStream storage directory (required to enable cluster mode)")
	nodeName := flag.String("node-name", "", "Cluster node name (defaults to control-plane addr)")
	var natsPeers stringList
	flag.Var(&natsPeers, "nats-peer", "Cluster peer route, repeatable (e.g. localhost:14248)")
	tlsCert := flag.String("tls-cert", "", "Server cert (PEM); enables mTLS on cluster routes + outbound gRPC")
	tlsKey := flag.String("tls-key", "", "Server key (PEM); pair with --tls-cert")
	tlsCA := flag.String("tls-ca", "", "CA bundle (PEM) used to verify peer certs")
	flag.Parse()

	var mtls *tls.Config
	if *tlsCert != "" || *tlsKey != "" || *tlsCA != "" {
		c, err := gateway.LoadMTLSConfig(*tlsCert, *tlsKey, *tlsCA)
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		mtls = c
	}

	var cluster *gateway.Cluster
	if *natsData != "" {
		name := *nodeName
		if name == "" {
			name = "gw" + *cpAddr
		}
		c, err := gateway.StartCluster(gateway.ClusterOptions{
			NodeName:      name,
			ClientListen:  *natsListen,
			ClusterListen: *natsCluster,
			Peers:         natsPeers,
			DataDir:       *natsData,
			TLS:           mtls,
		})
		if err != nil {
			log.Fatalf("start cluster: %v", err)
		}
		cluster = c
		log.Printf("nats client=%s cluster=%s data=%s peers=%v node=%s tls=%v",
			*natsListen, *natsCluster, *natsData, []string(natsPeers), c.NodeID, mtls != nil)
	}

	var gwOpts []gateway.Option
	if cluster != nil {
		gwOpts = append(gwOpts, gateway.WithCluster(cluster))
	}
	if mtls != nil {
		gwOpts = append(gwOpts, gateway.WithTLS(mtls))
	}
	gw := gateway.New(gwOpts...)

	cpLis, err := net.Listen("tcp", *cpAddr)
	if err != nil {
		log.Fatalf("listen control plane: %v", err)
	}
	var grpcOpts []grpc.ServerOption
	if mtls != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(mtls)))
	}
	srv := grpc.NewServer(grpcOpts...)
	cpv1.RegisterControlPlaneServer(srv, gw.ControlPlane())
	go func() {
		log.Printf("control plane listening on %s", *cpAddr)
		if err := srv.Serve(cpLis); err != nil {
			log.Fatalf("control plane serve: %v", err)
		}
	}()

	go func() {
		log.Printf("graphql listening on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, gw.Handler()); err != nil {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	srv.GracefulStop()
	cluster.Close()
}
