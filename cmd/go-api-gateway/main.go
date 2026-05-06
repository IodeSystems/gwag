// Command go-api-gateway runs a GraphQL gateway over a list of .proto
// files and gRPC destinations supplied on the command line, or talks
// to a running gateway via subcommands.
//
// Usage:
//
//	# Run a gateway:
//	go-api-gateway --proto path/to/foo.proto=foo-svc:50051 \
//	               --proto path/to/bar.proto=billing@bar-svc:50051 \
//	               --addr :8080
//
//	# Talk to a running gateway:
//	go-api-gateway peer list   --gateway localhost:50090
//	go-api-gateway peer forget --gateway localhost:50090 NODE_ID
//
// Each --proto flag takes PATH=[NAMESPACE@]ADDR. Without a namespace,
// the proto's filename stem is used. Without TLS configuration, dialing
// is insecure — fine for inside a service mesh, not the public network.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gateway "github.com/iodesystems/go-api-gateway"
	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
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
	if len(os.Args) >= 2 && os.Args[1] == "peer" {
		os.Exit(peerCmd(os.Args[2:]))
	}
	runGateway()
}

func runGateway() {
	var protos protoFlag
	flag.Var(&protos, "proto", "PATH=[NS@]ADDR (repeatable)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: go-api-gateway [--addr :8080] --proto PATH=[NS@]ADDR ...")
		fmt.Fprintln(flag.CommandLine.Output(), "       go-api-gateway peer (list|forget) ...")
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

// splitFlagsAndPositionals walks a free-form argv tail and returns
// (flags, positionals) so the flags can be parsed even when they're
// interleaved with positional args. Recognises --flag, --flag=val,
// -f, -f=val, and -f val (the last requires foreknowledge that the
// flag takes a value, which we approximate by always consuming the
// next token unless it starts with '-').
func splitFlagsAndPositionals(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		// flag in --foo / -foo form; take next token as its value if
		// it doesn't itself look like a flag.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, positionals
}

func peerCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: go-api-gateway peer (list|forget NODE_ID) [--gateway HOST:PORT]")
		return 2
	}
	verb := args[0]
	rest := args[1:]
	// Reorder so all flags precede positionals — Go's flag.Parse stops
	// at the first non-flag, which makes `forget NODE_ID --gateway X`
	// otherwise drop the flag.
	flags, positionals := splitFlagsAndPositionals(rest)
	fs := flag.NewFlagSet("peer "+verb, flag.ContinueOnError)
	gwAddr := fs.String("gateway", "localhost:50090", "Gateway control-plane address")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: go-api-gateway peer "+verb+" [--gateway HOST:PORT] [args...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	rest = positionals

	conn, err := grpc.NewClient(*gwAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *gwAddr, err)
		return 1
	}
	defer conn.Close()
	client := cpv1.NewControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch verb {
	case "list":
		resp, err := client.ListPeers(ctx, &cpv1.ListPeersRequest{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ListPeers: %v\n", err)
			return 1
		}
		if len(resp.GetPeers()) == 0 {
			fmt.Println("(no peers)")
			return 0
		}
		fmt.Printf("%-58s %-10s %s\n", "NODE_ID", "NAME", "JOINED")
		for _, p := range resp.GetPeers() {
			joined := time.UnixMilli(p.GetJoinedUnixMs()).Format(time.RFC3339)
			fmt.Printf("%-58s %-10s %s\n", p.GetNodeId(), p.GetName(), joined)
		}
		return 0

	case "forget":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: go-api-gateway peer forget NODE_ID [--gateway HOST:PORT]")
			return 2
		}
		resp, err := client.ForgetPeer(ctx, &cpv1.ForgetPeerRequest{NodeId: rest[0]})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ForgetPeer: %v\n", err)
			return 1
		}
		fmt.Printf("removed=%v new_replicas=%d\n", resp.GetRemoved(), resp.GetNewReplicas())
		return 0

	default:
		fs.Usage()
		return 2
	}
}
