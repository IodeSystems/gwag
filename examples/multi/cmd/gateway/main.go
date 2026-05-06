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
	"context"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	environment := flag.String("environment", "", "Deployment environment label (e.g. dev, staging, prod); part of NATS cluster name")
	maxInflight := flag.Int("max-inflight", gateway.DefaultBackpressure.MaxInflight, "Per-pool unary dispatch concurrency cap; 0 disables")
	maxStreams := flag.Int("max-streams", gateway.DefaultBackpressure.MaxStreams, "Per-pool subscription stream cap; 0 disables")
	maxStreamsTotal := flag.Int("max-streams-total", gateway.DefaultBackpressure.MaxStreamsTotal, "Gateway-wide subscription stream cap; 0 disables")
	maxWait := flag.Duration("max-wait", gateway.DefaultBackpressure.MaxWaitTime, "Per-dispatch wait budget; exceeded → backoff reject; 0 disables")
	insecureSubscribe := flag.Bool("insecure-subscribe", false, "Disable HMAC verification on subscriptions (dev only)")
	subscribeSecret := flag.String("subscribe-secret", "", "Hex-encoded shared HMAC secret for subscription verification")
	subscribeSkew := flag.Duration("subscribe-skew", 0, "Accepted timestamp drift on subscribe HMACs; 0 → 5min default")
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
			Environment:   *environment,
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
	gwOpts = append(gwOpts, gateway.WithBackpressure(gateway.BackpressureOptions{
		MaxInflight:     *maxInflight,
		MaxStreams:      *maxStreams,
		MaxStreamsTotal: *maxStreamsTotal,
		MaxWaitTime:     *maxWait,
	}))

	switch {
	case *insecureSubscribe:
		gwOpts = append(gwOpts, gateway.WithoutSubscriptionAuth())
	case *subscribeSecret != "":
		secret, err := hex.DecodeString(*subscribeSecret)
		if err != nil {
			log.Fatalf("subscribe-secret must be hex: %v", err)
		}
		gwOpts = append(gwOpts, gateway.WithSubscriptionAuth(gateway.SubscriptionAuthOptions{
			Secret:     secret,
			SkewWindow: *subscribeSkew,
		}))
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

	// Dogfood: expose the gateway's own ControlPlane service through
	// the GraphQL surface so the admin UI can talk to /graphql only.
	if err := gw.AddProtoDescriptor(cpv1.File_control_proto,
		gateway.As("admin"), gateway.To("localhost"+*cpAddr)); err != nil {
		log.Fatalf("self-register controlplane: %v", err)
	}
	go func() {
		log.Printf("control plane listening on %s", *cpAddr)
		if err := srv.Serve(cpLis); err != nil {
			log.Fatalf("control plane serve: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/graphql", gw.Handler())
	mux.Handle("/schema", gw.SchemaHandler()) // back-compat alias
	mux.Handle("/schema/graphql", gw.SchemaHandler())
	mux.Handle("/schema/proto", gw.SchemaProtoHandler())
	mux.Handle("/schema/openapi", gw.SchemaOpenAPIHandler())
	mux.Handle("/metrics", gw.MetricsHandler())
	mux.Handle("/health", gw.HealthHandler())
	go func() {
		log.Printf("graphql listening on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, mux); err != nil {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Graceful drain: /health flips to 503 immediately so the LB pulls
	// us out, active subscriptions get cancelled, then we wait up to
	// 30s for streams to finish. After that, gRPC and NATS shut down.
	log.Printf("draining...")
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := gw.Drain(drainCtx); err != nil {
		log.Printf("drain returned: %v (proceeding with shutdown)", err)
	} else {
		log.Printf("drained cleanly")
	}
	drainCancel()

	srv.GracefulStop()
	cluster.Close()
}
