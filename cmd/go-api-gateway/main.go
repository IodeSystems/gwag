// Command go-api-gateway runs a GraphQL gateway over a list of .proto
// files and gRPC destinations supplied on the command line.
//
// Usage:
//
//	go-api-gateway --proto path/to/foo.proto=foo-svc:50051 \
//	               --proto path/to/bar.proto=billing@bar-svc:50051 \
//	               --addr :8080
//
// Each --proto flag takes PATH=[NAMESPACE@]ADDR. Without a namespace,
// the proto's filename stem is used. Without TLS configuration, dialing
// is insecure — fine for inside a service mesh, not the public network.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	gateway "github.com/iodesystems/go-api-gateway"
)

type protoSpec struct {
	path string
	ns   string
	addr string
}

type protoFlag []protoSpec

func (p *protoFlag) String() string { return fmt.Sprint(*p) }

func (p *protoFlag) Set(v string) error {
	eq := strings.Index(v, "=")
	if eq < 0 {
		return fmt.Errorf("expected --proto PATH=[NS@]ADDR, got %q", v)
	}
	spec := protoSpec{path: v[:eq]}
	rest := v[eq+1:]
	if at := strings.Index(rest, "@"); at >= 0 {
		spec.ns = rest[:at]
		spec.addr = rest[at+1:]
	} else {
		spec.addr = rest
	}
	if spec.path == "" || spec.addr == "" {
		return fmt.Errorf("--proto: both path and addr are required, got %q", v)
	}
	*p = append(*p, spec)
	return nil
}

func main() {
	var protos protoFlag
	flag.Var(&protos, "proto", "PATH=[NS@]ADDR (repeatable)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: go-api-gateway [--addr :8080] --proto PATH=[NS@]ADDR ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if len(protos) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	gw := gateway.New()
	for _, p := range protos {
		opts := []gateway.ServiceOption{gateway.To(p.addr)}
		if p.ns != "" {
			opts = append(opts, gateway.As(p.ns))
		}
		if err := gw.AddProto(p.path, opts...); err != nil {
			log.Fatalf("register %s: %v", p.path, err)
		}
		log.Printf("registered %s → %s", p.path, p.addr)
	}

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, gw.Handler()); err != nil {
		log.Fatal(err)
	}
}
