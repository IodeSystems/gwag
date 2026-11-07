// demo-orders: the consumer process for demo.sh. Registers namespace
// "orders" declaring depends library:[--depends], blocks until the
// gateway reports the dependency ready, then sends a steady stream of
// queries to library through the gateway with X-Caller-Id: orders/v1.
// Each tick it calls the newest line of library that the gateway
// reports present (Registration.PresentLines) and that --depends
// accepts.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

func main() {
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	httpURL := flag.String("http", "http://localhost:8080", "Gateway HTTP base URL")
	instance := flag.String("instance", "orders-a", "Instance id")
	depends := flag.String("depends", "v1", "Comma-separated library lines this release accepts")
	every := flag.Duration("every", 300*time.Millisecond, "Traffic interval")
	flag.Parse()

	// The orders namespace needs a proto and an address; the demo
	// registers greeter.proto and never receives calls.
	src, err := os.ReadFile("protos/greeter.proto")
	if err != nil {
		log.Fatalf("read greeter.proto (run from examples/multi/): %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, greeterv1.UnimplementedGreeterServiceServer{})
	go func() { _ = srv.Serve(lis) }()

	accepted := strings.Split(*depends, ",")
	log.Printf("%s starting; depends library:[%s] (waits until ready)", *instance, *depends)
	reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
		GatewayAddr: *gatewayAddr,
		ServiceAddr: lis.Addr().String(),
		InstanceID:  *instance,
		Services: []controlclient.Service{{
			Namespace:   "orders",
			Version:     "v1",
			ProtoSource: src,
			Depends:     []controlclient.Dependency{{Service: "library", Lines: accepted}},
			Owner:       &controlclient.Owner{Team: "commerce", Contact: "#commerce"},
		}},
		DrainOnSIGTERM: true,
		OnDrained:      func() { os.Exit(0) },
	})
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	log.Printf("%s ready; library lines present: %v", *instance, reg.PresentLines("library"))

	var last string
	for range time.Tick(*every) {
		line := pick(reg.PresentLines("library"), accepted)
		if line == "" {
			continue
		}
		if line != last {
			log.Printf("%s now calling library %s", *instance, line)
			last = line
		}
		q := fmt.Sprintf(`{ library { %s { listBooks { books { title } } } } }`, line)
		if err := post(*httpURL+"/api/graphql", "orders/v1", q); err != nil {
			log.Printf("library %s: %v", line, err)
		}
	}
}

// pick returns the highest accepted line that is present.
func pick(present, accepted []string) string {
	var ok []string
	for _, p := range present {
		for _, a := range accepted {
			if p == a {
				ok = append(ok, p)
			}
		}
	}
	if len(ok) == 0 {
		return ""
	}
	sort.Strings(ok)
	return ok[len(ok)-1]
}

func post(url, caller, query string) error {
	body := fmt.Sprintf(`{"query":%q}`, query)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caller-Id", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
