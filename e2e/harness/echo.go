//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const defaultCallTimeout = 30 * time.Second

// Echo is a fixture's answer to one Echo call, whatever the ingress.
type Echo struct {
	Msg       string
	Instance  string
	Namespace string
	Version   string
	Call      int64
}

// echoWire decodes the answer shapes: proto int64 arrives as a JSON
// string, OpenAPI and GraphQL as a number.
type echoWire struct {
	Msg       string          `json:"msg"`
	Instance  string          `json:"instance"`
	Namespace string          `json:"namespace"`
	Version   string          `json:"version"`
	Call      json.RawMessage `json:"call"`
}

func (w echoWire) echo() (Echo, error) {
	e := Echo{Msg: w.Msg, Instance: w.Instance, Namespace: w.Namespace, Version: w.Version}
	s := strings.Trim(string(w.Call), `"`)
	if s != "" && s != "null" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return e, fmt.Errorf("echo call %q: %w", w.Call, err)
		}
		e.Call = n
	}
	return e, nil
}

// EchoQuery returns the GraphQL document that calls this fixture's
// Echo on line ("" = the namespace's latest line, the flat fields) and
// the data path of the answer. The message is variable $msg.
func (f *Fixture) EchoQuery(line string) (query, path string) {
	ns := f.Opts.Namespace
	sel := "{ msg instance namespace version call }"
	op, call := "query", "echo(msg: $msg) "+sel
	if f.Opts.Kind == KindOpenAPI {
		// POST operations surface as mutations taking a body input.
		op, call = "mutation", "echo(body: {msg: $msg}) "+sel
	}
	path = ns
	if line != "" {
		call = line + " { " + call + " }"
		path += "." + line
	}
	return fmt.Sprintf("%s E2EEcho($msg: String!) { %s { %s } }", op, ns, call), path + ".echo"
}

// Echo calls the fixture's Echo through the gateway's GraphQL
// endpoint. line "" = latest. A GraphQL error is returned as an error.
func (f *Fixture) Echo(ctx context.Context, line, msg string, opts ...ReqOpt) (Echo, error) {
	return f.EchoVia(ctx, f.Gateway, line, msg, opts...)
}

// EchoVia is Echo through another gateway, e.g. a different cluster
// node.
func (f *Fixture) EchoVia(ctx context.Context, gw *Gateway, line, msg string, opts ...ReqOpt) (Echo, error) {
	q, path := f.EchoQuery(line)
	resp, err := gw.GraphQL(ctx, q, map[string]any{"msg": msg}, opts...)
	if err != nil {
		return Echo{}, err
	}
	if err := resp.Err(); err != nil {
		return Echo{}, err
	}
	v := resp.Path(path)
	if v == nil {
		return Echo{}, fmt.Errorf("echo: no data at %s: %s", path, resp.Data)
	}
	b, _ := json.Marshal(v)
	var w echoWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Echo{}, err
	}
	return w.echo()
}

// MustEcho is Echo that fails the test on error.
func (f *Fixture) MustEcho(line, msg string, opts ...ReqOpt) Echo {
	t := f.env.t
	t.Helper()
	e, err := f.Echo(Ctx(t, defaultCallTimeout), line, msg, opts...)
	if err != nil {
		t.Fatalf("echo %s/%s: %v", f.Opts.Namespace, line, err)
	}
	return e
}

// EchoREST calls Echo on line through the gateway's REST ingress
// (/api/ingress): POST /<package>.FixtureService/Echo for proto, the
// spec path /<ns>/<line>/echo for OpenAPI.
func (f *Fixture) EchoREST(ctx context.Context, line, msg string, opts ...ReqOpt) (Echo, error) {
	return f.EchoRESTVia(ctx, f.Gateway, line, msg, opts...)
}

// EchoRESTVia is EchoREST through another gateway, e.g. a different
// cluster node.
func (f *Fixture) EchoRESTVia(ctx context.Context, gw *Gateway, line, msg string, opts ...ReqOpt) (Echo, error) {
	var path string
	switch f.Opts.Kind {
	case KindProto:
		path = "/api/ingress" + EchoMethod(f.Opts.Namespace, line)
	case KindOpenAPI:
		path = "/api/ingress/" + f.Opts.Namespace + "/" + line + "/echo"
	default:
		return Echo{}, fmt.Errorf("EchoREST: no REST route for kind %s", f.Opts.Kind)
	}
	var w echoWire
	if err := gw.Post(ctx, path, map[string]any{"msg": msg}, &w, opts...); err != nil {
		return Echo{}, err
	}
	return w.echo()
}

// EchoConnect calls the proto fixture's Echo on line with the Connect
// protocol (unary, JSON) at /api/connect.
func (f *Fixture) EchoConnect(ctx context.Context, line, msg string, opts ...ReqOpt) (Echo, error) {
	if f.Opts.Kind != KindProto {
		return Echo{}, fmt.Errorf("EchoConnect: proto fixtures only")
	}
	var w echoWire
	if err := f.Gateway.Post(ctx, "/api/connect"+EchoMethod(f.Opts.Namespace, line), map[string]any{"msg": msg}, &w, opts...); err != nil {
		return Echo{}, err
	}
	return w.echo()
}

// EchoGRPC calls the proto fixture's Echo on line over native gRPC on
// the gateway's control-plane port (GRPCUnknownHandler). caller ""
// sends DefaultCaller as caller-id metadata.
func (f *Fixture) EchoGRPC(ctx context.Context, line, msg, caller string) (Echo, error) {
	return f.EchoGRPCVia(ctx, f.Gateway, line, msg, caller)
}

// EchoGRPCVia is EchoGRPC through another gateway's gRPC port.
func (f *Fixture) EchoGRPCVia(ctx context.Context, gw *Gateway, line, msg, caller string) (Echo, error) {
	if f.Opts.Kind != KindProto {
		return Echo{}, fmt.Errorf("EchoGRPC: proto fixtures only")
	}
	md, err := FixtureMethod(f.Opts.Namespace, line)
	if err != nil {
		return Echo{}, err
	}
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("msg"), protoreflect.ValueOfString(msg))
	out := dynamicpb.NewMessage(md.Output())
	if caller == "" {
		caller = DefaultCaller
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "caller-id", caller)
	if err := gw.grpcConn().Invoke(ctx, EchoMethod(f.Opts.Namespace, line), in, out); err != nil {
		return Echo{}, err
	}
	b, err := protojson.Marshal(out)
	if err != nil {
		return Echo{}, err
	}
	var w echoWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Echo{}, err
	}
	return w.echo()
}

var (
	fixtureProtoMu sync.Mutex
	fixtureProtos  = map[string]protoreflect.MethodDescriptor{}
)

// FixtureMethod compiles e2e/fixture/fixture.proto for (namespace,
// line), the way the fixture does, and returns its Echo method.
func FixtureMethod(namespace, line string) (protoreflect.MethodDescriptor, error) {
	pkg := ProtoPackage(namespace, line)
	fixtureProtoMu.Lock()
	defer fixtureProtoMu.Unlock()
	if md, ok := fixtureProtos[pkg]; ok {
		return md, nil
	}
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	src, err := os.ReadFile(filepath.Join(root, "e2e", "fixture", "fixture.proto"))
	if err != nil {
		return nil, err
	}
	src = bytes.Replace(src, []byte("package PACKAGE;"), []byte("package "+pkg+";"), 1)
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: func(p string) (io.ReadCloser, error) {
			if p == "fixture.proto" {
				return io.NopCloser(bytes.NewReader(src)), nil
			}
			return nil, fmt.Errorf("not found: %s", p)
		},
	})}
	fds, err := c.Compile(context.Background(), "fixture.proto")
	if err != nil {
		return nil, err
	}
	md := fds[0].Services().ByName("FixtureService").Methods().ByName("Echo")
	if md == nil {
		return nil, fmt.Errorf("fixture.proto: no FixtureService.Echo")
	}
	fixtureProtos[pkg] = md
	return md, nil
}
