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
	"io"
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
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "peer":
			os.Exit(peerCmd(os.Args[2:]))
		case "services":
			os.Exit(servicesCmd(os.Args[2:]))
		case "schema":
			os.Exit(schemaCmd(os.Args[2:]))
		}
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

func servicesCmd(args []string) int {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(os.Stderr, "Usage: go-api-gateway services list [--gateway HOST:PORT]")
		return 2
	}
	flags, _ := splitFlagsAndPositionals(args[1:])
	fs := flag.NewFlagSet("services list", flag.ContinueOnError)
	gwAddr := fs.String("gateway", "localhost:50090", "Gateway control-plane address")
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	conn, err := grpc.NewClient(*gwAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *gwAddr, err)
		return 1
	}
	defer conn.Close()
	client := cpv1.NewControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.ListServices(ctx, &cpv1.ListServicesRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ListServices: %v\n", err)
		return 1
	}
	if env := resp.GetEnvironment(); env != "" {
		fmt.Printf("# environment: %s\n", env)
	}
	if len(resp.GetServices()) == 0 {
		fmt.Println("(no services registered)")
		return 0
	}
	fmt.Printf("%-20s %-6s %-66s %s\n", "NAMESPACE", "VER", "HASH", "REPLICAS")
	for _, s := range resp.GetServices() {
		fmt.Printf("%-20s %-6s %-66s %d\n",
			s.GetNamespace(), s.GetVersion(), s.GetHashHex(), s.GetReplicaCount())
	}
	return 0
}

func schemaCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: go-api-gateway schema (fetch|diff) ...")
		return 2
	}
	switch args[0] {
	case "fetch":
		return schemaFetchCmd(args[1:])
	case "diff":
		return schemaDiffCmd(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "Usage: go-api-gateway schema (fetch|diff) ...")
		return 2
	}
}

func schemaFetchCmd(args []string) int {
	flags, _ := splitFlagsAndPositionals(args)
	fs := flag.NewFlagSet("schema fetch", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://localhost:8080/schema", "Gateway /schema endpoint URL")
	jsonOut := fs.Bool("json", false, "Fetch introspection JSON instead of SDL")
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	url := *endpoint
	if *jsonOut {
		if strings.Contains(url, "?") {
			url += "&format=json"
		} else {
			url += "?format=json"
		}
	}
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GET %s: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "GET %s: %s\n", url, resp.Status)
		return 1
	}
	if env := resp.Header.Get("X-Gateway-Environment"); env != "" {
		fmt.Fprintf(os.Stderr, "# environment: %s\n", env)
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
	return 0
}

func schemaDiffCmd(args []string) int {
	flags, _ := splitFlagsAndPositionals(args)
	fs := flag.NewFlagSet("schema diff", flag.ContinueOnError)
	from := fs.String("from", "", "Old schema source (URL or file path; required)")
	to := fs.String("to", "", "New schema source (URL or file path; required)")
	strict := fs.Bool("strict", false, "Exit non-zero if any breaking changes are reported")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: go-api-gateway schema diff --from OLD --to NEW [--strict]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if *from == "" || *to == "" {
		fs.Usage()
		return 2
	}
	oldSDL, err := loadSchemaSource(*from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load --from %s: %v\n", *from, err)
		return 1
	}
	newSDL, err := loadSchemaSource(*to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load --to %s: %v\n", *to, err)
		return 1
	}
	oldM, err := parseSchemaModel(oldSDL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse --from: %v\n", err)
		return 1
	}
	newM, err := parseSchemaModel(newSDL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse --to: %v\n", err)
		return 1
	}
	changes := diffModels(oldM, newM)
	breaking := 0
	for _, c := range changes {
		fmt.Printf("[%s] %s\n", strings.ToUpper(c.severity), c.msg)
		if c.severity == "breaking" {
			breaking++
		}
	}
	if len(changes) == 0 {
		fmt.Println("(no differences)")
	} else {
		fmt.Printf("\n%d change(s); %d breaking\n", len(changes), breaking)
	}
	if *strict && breaking > 0 {
		return 1
	}
	return 0
}

// loadSchemaSource fetches an SDL from either an HTTP(S) URL or a
// local file path. URLs are detected by the http:// or https:// prefix.
func loadSchemaSource(src string) (string, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := http.Get(src)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("status %s", resp.Status)
		}
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	b, err := os.ReadFile(src)
	return string(b), err
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
