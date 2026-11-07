//go:build e2e

// fixture: one configurable upstream service for the e2e stories.
// It serves an Echo operation as proto (gRPC), OpenAPI (HTTP/JSON) or
// GraphQL, self-registers with a gateway control plane, and injects
// faults on request: latency, an error rate, a hang, a crash after N
// calls. See e2e/README.md for the flags.
//
// The proto kind needs no generated code: fixture.proto is compiled at
// boot with protocompile and served through grpc.UnknownServiceHandler
// with dynamicpb messages, so a story can pass any proto with --proto.
//
// Status lines on stderr that the harness waits for:
//
//	FIXTURE listening addr=... control=...
//	FIXTURE ready
//	FIXTURE draining
//	FIXTURE crash call=N
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

//go:embed fixture.proto
var protoTemplate string

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, " ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// faults is the live fault configuration. Flags set the start values;
// POST /faults on the control listener changes them.
type faults struct {
	LatencyMs  int64   `json:"latencyMs"`
	ErrorRate  float64 `json:"errorRate"`
	Hang       bool    `json:"hang"`
	CrashAfter int64   `json:"crashAfter"`
}

type fixture struct {
	kind, namespace, instance string

	mu     sync.Mutex
	faults faults
	calls  atomic.Int64
	errs   atomic.Int64
}

func (f *fixture) snapshot() faults {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faults
}

// errInjected is the error a call returns when --error-rate selects it.
var errInjected = errors.New("fixture: injected error")

// before runs the fault injection for one call and returns its call
// number. A non-nil error fails the call. The error rate is
// deterministic: with rate r, call n fails when floor(n*r) >
// floor((n-1)*r), so 0.25 fails calls 4, 8, 12, ...
func (f *fixture) before(ctx context.Context) (int64, error) {
	n := f.calls.Add(1)
	fl := f.snapshot()
	if fl.CrashAfter > 0 && n > fl.CrashAfter {
		fmt.Fprintf(os.Stderr, "FIXTURE crash call=%d\n", n)
		os.Exit(3)
	}
	if fl.Hang {
		<-ctx.Done()
		return n, ctx.Err()
	}
	if fl.LatencyMs > 0 {
		t := time.NewTimer(time.Duration(fl.LatencyMs) * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return n, ctx.Err()
		}
	}
	if fl.ErrorRate > 0 && math.Floor(float64(n)*fl.ErrorRate) > math.Floor(float64(n-1)*fl.ErrorRate) {
		f.errs.Add(1)
		return n, errInjected
	}
	return n, nil
}

func (f *fixture) echo(msg, version string, n int64) map[string]any {
	return map[string]any{
		"msg":       msg,
		"instance":  f.instance,
		"namespace": f.namespace,
		"version":   version,
		"call":      n,
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	kind := flag.String("kind", "proto", "Upstream kind: proto, openapi or graphql")
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control-plane address")
	var gatewayAlts listFlag
	flag.Var(&gatewayAlts, "gateway-alt", "Another control-plane address of the same cluster (repeatable); the fixture moves its registration there when --gateway stops answering (controlclient GatewayAddrs)")
	namespace := flag.String("namespace", "fixture", "Namespace to register under")
	versions := flag.String("versions", "v1", "Comma-separated version lines to serve, e.g. v1,v2 or unstable")
	var depends listFlag
	flag.Var(&depends, "depends", "Dependency SERVICE:LINE[,LINE...] (repeatable), e.g. library:v1,v2")
	owner := flag.String("owner", "", "Owning team (empty = no owner in the manifest)")
	public := flag.Bool("public", false, "Register with public=true: shown to admin readers without a token")
	instance := flag.String("instance", "", "Instance id (default: <namespace>@<addr>)")
	addr := flag.String("addr", "127.0.0.1:0", "Service listen address")
	controlAddr := flag.String("control", "127.0.0.1:0", "Control HTTP listen address (GET /stats, POST /faults)")
	var protoFiles listFlag
	flag.Var(&protoFiles, "proto", "LINE=PATH: register this .proto for LINE instead of fixture.proto (repeatable). The text PACKAGE is replaced as in fixture.proto.")
	latency := flag.Duration("latency", 0, "Delay every call by this long")
	errorRate := flag.Float64("error-rate", 0, "Fail this fraction of calls, 0..1 (deterministic: every 1/rate-th call)")
	hang := flag.Bool("hang", false, "Never answer a call; block until the caller gives up")
	crashAfter := flag.Int64("crash-after", 0, "Exit with status 3, without deregistering, on call N+1 (0 = never)")
	drain := flag.Bool("drain-on-sigterm", false, "On SIGTERM, drain through the gateway, deregister, then exit (controlclient DrainOnSIGTERM)")
	drainTimeout := flag.Duration("drain-timeout", 30*time.Second, "With --drain-on-sigterm: max wait for safe_to_stop")
	ttl := flag.Duration("ttl", 0, "Registration TTL (0 = gateway default, 30s)")
	maxConc := flag.Uint("max-concurrency", 0, "Per-binding MaxConcurrency (0 = gateway default)")
	flag.Parse()

	f := &fixture{kind: *kind, namespace: *namespace}
	f.faults = faults{LatencyMs: latency.Milliseconds(), ErrorRate: *errorRate, Hang: *hang, CrashAfter: *crashAfter}
	lines := strings.Split(*versions, ",")

	deps, err := parseDepends(depends)
	if err != nil {
		log.Fatalf("--depends: %v", err)
	}
	overrides := map[string]string{}
	for _, p := range protoFiles {
		line, path, ok := strings.Cut(p, "=")
		if !ok {
			log.Fatalf("--proto %q: want LINE=PATH", p)
		}
		overrides[line] = path
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	ctlLis, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		log.Fatalf("listen control: %v", err)
	}
	if *instance == "" {
		*instance = *namespace + "@" + lis.Addr().String()
	}
	f.instance = *instance

	var svcs []controlclient.Service
	var serviceAddr string
	var stopServe func()
	switch *kind {
	case "proto":
		svcs, stopServe, err = f.serveProto(lis, lines, overrides)
		serviceAddr = lis.Addr().String()
	case "openapi":
		svcs, stopServe, err = f.serveOpenAPI(lis, lines)
		serviceAddr = "http://" + lis.Addr().String()
	case "graphql":
		svcs, stopServe, err = f.serveGraphQL(lis, lines)
		serviceAddr = "http://" + lis.Addr().String()
	default:
		err = fmt.Errorf("unknown --kind %q", *kind)
	}
	if err != nil {
		log.Fatalf("%v", err)
	}
	go f.serveControl(ctlLis)
	fmt.Fprintf(os.Stderr, "FIXTURE listening addr=%s control=%s\n", lis.Addr(), ctlLis.Addr())

	var own *controlclient.Owner
	if *owner != "" {
		own = &controlclient.Owner{Team: *owner, Contact: "#" + *owner}
	}
	for i := range svcs {
		svcs[i].Namespace = *namespace
		svcs[i].Depends = deps
		svcs[i].Owner = own
		svcs[i].Public = *public
		svcs[i].MaxConcurrency = uint32(*maxConc)
	}

	opts := controlclient.Options{
		GatewayAddr:    *gatewayAddr,
		GatewayAddrs:   gatewayAlts,
		ServiceAddr:    serviceAddr,
		InstanceID:     *instance,
		Services:       svcs,
		TTL:            *ttl,
		DrainOnSIGTERM: *drain,
		DrainTimeout:   *drainTimeout,
	}
	var reg atomic.Pointer[controlclient.Registration]
	if *drain {
		opts.OnDrained = func() {
			log.Printf("%s drained and deregistered; exiting", *instance)
			stopServe()
			os.Exit(0)
		}
		go func() {
			// controlclient catches SIGTERM itself; log it so a story
			// can wait for the drain to start.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM)
			<-sig
			fmt.Fprintln(os.Stderr, "FIXTURE draining")
		}()
	} else {
		// Without DrainOnSIGTERM the fixture still exits on SIGTERM,
		// deregistering first (a polite shutdown with no drain).
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
			<-sig
			log.Printf("%s: SIGTERM, deregistering", *instance)
			if r := reg.Load(); r != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = r.Close(ctx)
				cancel()
			}
			stopServe()
			os.Exit(0)
		}()
	}
	r, err := controlclient.SelfRegister(context.Background(), opts)
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	reg.Store(r)
	log.Printf("%s registered %s lines [%s]", *instance, *namespace, strings.Join(lines, ","))
	fmt.Fprintln(os.Stderr, "FIXTURE ready")
	select {}
}

func parseDepends(in []string) ([]controlclient.Dependency, error) {
	var out []controlclient.Dependency
	for _, d := range in {
		svc, lines, ok := strings.Cut(d, ":")
		if !ok || svc == "" || lines == "" {
			return nil, fmt.Errorf("%q: want SERVICE:LINE[,LINE...]", d)
		}
		out = append(out, controlclient.Dependency{Service: svc, Lines: strings.Split(lines, ",")})
	}
	return out, nil
}

// ---- control listener -----------------------------------------------

func (f *fixture) serveControl(lis net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"calls":  f.calls.Load(),
			"errors": f.errs.Load(),
			"faults": f.snapshot(),
		})
	})
	// POST /faults replaces the whole fault set with the JSON body.
	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var fl faults
		if err := json.NewDecoder(r.Body).Decode(&fl); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.faults = fl
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	_ = http.Serve(lis, mux)
}

// ---- proto ----------------------------------------------------------

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_]`)

func protoPackage(ns, line string) string {
	return "fixture." + nonIdent.ReplaceAllString(ns, "_") + "." + nonIdent.ReplaceAllString(line, "_")
}

type protoMethod struct {
	version string
	in, out protoreflect.MessageDescriptor
}

func (f *fixture) serveProto(lis net.Listener, lines []string, overrides map[string]string) ([]controlclient.Service, func(), error) {
	methods := map[string]protoMethod{}
	var svcs []controlclient.Service
	for _, line := range lines {
		src := protoTemplate
		if p, ok := overrides[line]; ok {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, nil, fmt.Errorf("--proto %s: %w", line, err)
			}
			src = string(b)
		}
		src = strings.Replace(src, "package PACKAGE;", "package "+protoPackage(f.namespace, line)+";", 1)
		fd, err := compileProto(f.namespace+".proto", []byte(src))
		if err != nil {
			return nil, nil, fmt.Errorf("compile proto for %s: %w", line, err)
		}
		for i := 0; i < fd.Services().Len(); i++ {
			sd := fd.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				md := sd.Methods().Get(j)
				methods["/"+string(sd.FullName())+"/"+string(md.Name())] = protoMethod{version: line, in: md.Input(), out: md.Output()}
			}
		}
		svcs = append(svcs, controlclient.Service{Version: line, ProtoSource: []byte(src)})
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		name, _ := grpc.MethodFromServerStream(stream)
		m, ok := methods[name]
		if !ok {
			return status.Errorf(codes.Unimplemented, "fixture: unknown method %s", name)
		}
		in := dynamicpb.NewMessage(m.in)
		if err := stream.RecvMsg(in); err != nil {
			return err
		}
		n, err := f.before(stream.Context())
		if err != nil {
			if errors.Is(err, errInjected) {
				return status.Error(codes.Internal, err.Error())
			}
			return status.FromContextError(err).Err()
		}
		out := dynamicpb.NewMessage(m.out)
		fillProto(out, in, f.echo("", m.version, n))
		return stream.SendMsg(out)
	}))
	go func() { _ = srv.Serve(lis) }()
	return svcs, srv.Stop, nil
}

// fillProto copies same-named scalar fields from the request, then sets
// any of the echo fields (instance, namespace, version, call) the
// response declares.
func fillProto(out, in *dynamicpb.Message, echo map[string]any) {
	fields := out.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() || fd.Message() != nil {
			continue
		}
		if src := in.Descriptor().Fields().ByName(fd.Name()); src != nil && src.Kind() == fd.Kind() && !src.IsList() && !src.IsMap() {
			out.Set(fd, in.Get(src))
		}
		v, ok := echo[string(fd.Name())]
		if !ok || fd.Name() == "msg" {
			continue
		}
		switch fd.Kind() {
		case protoreflect.StringKind:
			if s, ok := v.(string); ok {
				out.Set(fd, protoreflect.ValueOfString(s))
			}
		case protoreflect.Int64Kind:
			if n, ok := v.(int64); ok {
				out.Set(fd, protoreflect.ValueOfInt64(n))
			}
		case protoreflect.Int32Kind:
			if n, ok := v.(int64); ok {
				out.Set(fd, protoreflect.ValueOfInt32(int32(n)))
			}
		}
	}
}

func compileProto(entry string, body []byte) (protoreflect.FileDescriptor, error) {
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) {
				if p == entry {
					return io.NopCloser(bytes.NewReader(body)), nil
				}
				return nil, fmt.Errorf("proto import not found: %s", p)
			},
		}),
	}
	out, err := c.Compile(context.Background(), entry)
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// ---- openapi --------------------------------------------------------

// openAPISpec is the spec for one line. The path carries the namespace
// and the line (/<ns>/v1/echo): one HTTP server serves every line from
// one base URL, and two fixtures do not collide on the REST ingress.
func openAPISpec(ns, line string) []byte {
	return []byte(fmt.Sprintf(`{
  "openapi": "3.0.0",
  "info": {"title": %q, "version": %q},
  "paths": {
    "/%s/%s/echo": {
      "post": {
        "operationId": "echo",
        "summary": "Echo the message and the instance that served it.",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"msg": {"type": "string"}},
          "required": ["msg"]
        }}}},
        "responses": {"200": {"description": "Echo reply.", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "msg": {"type": "string"},
            "instance": {"type": "string"},
            "namespace": {"type": "string"},
            "version": {"type": "string"},
            "call": {"type": "integer", "format": "int64"}
          },
          "required": ["msg", "instance", "namespace", "version", "call"]
        }}}}}
      }
    }
  }
}`, "fixture-"+ns, line, ns, line))
}

func (f *fixture) serveOpenAPI(lis net.Listener, lines []string) ([]controlclient.Service, func(), error) {
	mux := http.NewServeMux()
	var svcs []controlclient.Service
	for _, line := range lines {
		line := line
		mux.HandleFunc("POST /"+f.namespace+"/"+line+"/echo", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Msg string `json:"msg"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			n, err := f.before(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.echo(in.Msg, line, n))
		})
		svcs = append(svcs, controlclient.Service{Version: line, OpenAPISpec: openAPISpec(f.namespace, line)})
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	return svcs, func() { _ = srv.Close() }, nil
}

// ---- graphql --------------------------------------------------------

func (f *fixture) graphQLSchema(line string) (graphql.Schema, error) {
	echoType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Echo",
		Fields: graphql.Fields{
			"msg":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"instance":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"namespace": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"version":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"call":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	return graphql.NewSchema(graphql.SchemaConfig{Query: graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"echo": &graphql.Field{
				Type:        graphql.NewNonNull(echoType),
				Description: "Echo the message and the instance that served it.",
				Args: graphql.FieldConfigArgument{
					"msg": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					n, err := f.before(p.Context)
					if err != nil {
						return nil, err
					}
					msg, _ := p.Args["msg"].(string)
					return f.echo(msg, line, n), nil
				},
			},
		},
	})})
}

func (f *fixture) serveGraphQL(lis net.Listener, lines []string) ([]controlclient.Service, func(), error) {
	mux := http.NewServeMux()
	var svcs []controlclient.Service
	for _, line := range lines {
		schema, err := f.graphQLSchema(line)
		if err != nil {
			return nil, nil, err
		}
		mux.HandleFunc("POST /"+line+"/graphql", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Query         string         `json:"query"`
				Variables     map[string]any `json:"variables"`
				OperationName string         `json:"operationName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			res := graphql.Do(graphql.Params{
				Schema:         schema,
				RequestString:  req.Query,
				VariableValues: req.Variables,
				OperationName:  req.OperationName,
				Context:        r.Context(),
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		})
		svcs = append(svcs, controlclient.Service{
			Version:         line,
			GraphQLEndpoint: "http://" + lis.Addr().String() + "/" + line + "/graphql",
		})
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()
	return svcs, func() { _ = srv.Close() }, nil
}
