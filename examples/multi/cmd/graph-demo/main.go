// Graph demo: fills the admin UI's dependency graph. Runs one gRPC
// server (greeter + library services) and self-registers it under
// several namespaces and lines with declared depends and owners, then
// sends traffic through the gateway with an X-Caller-Id header.
//
// Run the example gateway with --caller-id-public so the header is
// read:
//
//	cd examples/multi
//	go run ./cmd/gateway --caller-id-public --environment staging
//	go run ./cmd/graph-demo
//
// What the graph shows:
//   - greeter v1 is sunset-able: its dependents also accept v2, and
//     the demo sends no traffic to v1.
//   - library v1 is not: orders accepts only [v1].
//   - billing waits on payments, which nobody registers (unmet edge).
//   - mobile-app is an external caller (not a registered namespace).
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
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	libraryv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/library/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

type greeter struct {
	greeterv1.UnimplementedGreeterServiceServer
}

func (greeter) Hello(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	return &greeterv1.HelloResponse{Greeting: "Hello, " + req.GetName() + "!"}, nil
}

type library struct {
	libraryv1.UnimplementedLibraryServiceServer
}

func (library) ListBooks(context.Context, *libraryv1.ListBooksRequest) (*libraryv1.ListBooksResponse, error) {
	return &libraryv1.ListBooksResponse{Books: []*libraryv1.Book{{Id: "1", Title: "Go", Author: "Donovan", Year: 2015}}}, nil
}

type svc struct {
	instance string
	ns, ver  string
	proto    string // file under protos/
	owner    string
	depends  []controlclient.Dependency
	wait     bool // register in the background: depends are never met
}

func dep(service string, lines ...string) controlclient.Dependency {
	return controlclient.Dependency{Service: service, Lines: lines}
}

func main() {
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	httpURL := flag.String("http", "http://localhost:8080", "Gateway HTTP base URL (for traffic)")
	every := flag.Duration("every", 500*time.Millisecond, "Traffic interval")
	flag.Parse()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, greeter{})
	libraryv1.RegisterLibraryServiceServer(srv, library{})
	go func() { _ = srv.Serve(lis) }()
	addr := lis.Addr().String()

	// Providers first: SelfRegister blocks until depends are met.
	services := []svc{
		{instance: "library-v1-a", ns: "library", ver: "v1", proto: "library.proto", owner: "books"},
		{instance: "library-v2-a", ns: "library", ver: "v2", proto: "library.proto", owner: "books"},
		{instance: "library-v2-b", ns: "library", ver: "v2", proto: "library.proto", owner: "books"},
		{instance: "greeter-v1-a", ns: "greeter", ver: "v1", proto: "greeter.proto", owner: "greetings", depends: []controlclient.Dependency{dep("library", "v1", "v2")}},
		{instance: "greeter-v2-a", ns: "greeter", ver: "v2", proto: "greeter.proto", owner: "greetings", depends: []controlclient.Dependency{dep("library", "v2")}},
		{instance: "orders-v1-a", ns: "orders", ver: "v1", proto: "library.proto", owner: "commerce", depends: []controlclient.Dependency{dep("library", "v1"), dep("greeter", "v1", "v2")}},
		{instance: "web-v1-a", ns: "web", ver: "v1", proto: "greeter.proto", owner: "frontend", depends: []controlclient.Dependency{dep("greeter", "v1", "v2"), dep("orders", "v1")}},
		{instance: "billing-v1-a", ns: "billing", ver: "v1", proto: "library.proto", owner: "finance", depends: []controlclient.Dependency{dep("orders", "v1"), dep("payments", "v1")}, wait: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var regs []*controlclient.Registration
	for _, s := range services {
		src, err := os.ReadFile("protos/" + s.proto)
		if err != nil {
			log.Fatalf("read %s (run from examples/multi/): %v", s.proto, err)
		}
		opts := controlclient.Options{
			GatewayAddr: *gatewayAddr,
			ServiceAddr: addr,
			InstanceID:  s.instance,
			Services: []controlclient.Service{{
				Namespace:   s.ns,
				Version:     s.ver,
				ProtoSource: src,
				Depends:     s.depends,
				Owner:       &controlclient.Owner{Team: s.owner, Contact: "#" + s.owner},
			}},
		}
		if s.wait {
			go func() {
				// Blocks while waiting; the registration is listed
				// as waiting in the meantime.
				if _, err := controlclient.SelfRegister(ctx, opts); err != nil && ctx.Err() == nil {
					log.Printf("%s: %v", s.instance, err)
				}
			}()
			log.Printf("%s registering (will wait on its depends)", s.instance)
			continue
		}
		reg, err := controlclient.SelfRegister(ctx, opts)
		if err != nil {
			log.Fatalf("%s: %v", s.instance, err)
		}
		regs = append(regs, reg)
		log.Printf("%s registered (%s/%s)", s.instance, s.ns, s.ver)
	}

	// Traffic: caller → query. Callers named after a namespace map to
	// that service in the graph; "greeter/v2" maps to the line.
	calls := []struct{ caller, query string }{
		{"web", `{ greeter { v2 { hello(name: "web") { greeting } } } }`},
		{"orders", `{ library { v1 { listBooks { books { id } } } } }`},
		{"orders", `{ greeter { v2 { hello(name: "orders") { greeting } } } }`},
		{"greeter/v2", `{ library { v2 { listBooks { books { id } } } } }`},
		{"mobile-app", `{ web { v1 { hello(name: "app") { greeting } } } }`},
	}
	go func() {
		t := time.NewTicker(*every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			for _, c := range calls {
				if err := post(ctx, *httpURL+"/api/graphql", c.caller, c.query); err != nil {
					log.Printf("%s: %v", c.caller, err)
				}
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	cancel()
	for _, r := range regs {
		_ = r.Close(context.Background())
	}
	srv.GracefulStop()
}

func post(ctx context.Context, url, caller, query string) error {
	body := fmt.Sprintf(`{"query":%q}`, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
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
