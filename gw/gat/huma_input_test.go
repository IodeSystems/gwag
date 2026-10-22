package gat_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// checkedInput carries schema constraints and a Resolver that rejects
// on an argument, the way dund checks an HMAC carried in the args.
type checkedInput struct {
	Name  string `query:"name" maxLength:"3"`
	Count int    `query:"count" minimum:"1"`
	Sig   string `query:"sig"`
}

func (in *checkedInput) Resolve(huma.Context) []error {
	if in.Sig == "deny" {
		return []error{huma.Error401Unauthorized("bad signature")}
	}
	return nil
}

type checkedTag struct {
	Label string `json:"label"`
}

// A nested ResolverWithPath: huma finds it inside the body and passes
// its location.
func (t *checkedTag) Resolve(_ huma.Context, pb *huma.PathBuffer) []error {
	if t.Label == "x" {
		return []error{&huma.ErrorDetail{Message: "label x is reserved", Location: pb.String(), Value: t.Label}}
	}
	return nil
}

type checkedBodyInput struct {
	Body struct {
		Code string      `json:"code" pattern:"^[a-z]+$"`
		Note string      `json:"note,omitempty" minLength:"2"`
		Tag  *checkedTag `json:"tag,omitempty"`
	}
}

type checkedOutput struct {
	Body struct {
		V string `json:"v"`
	}
}

type checkedStreamInput struct {
	Sig string `query:"sig"`
}

func (in *checkedStreamInput) Resolve(huma.Context) []error {
	if in.Sig == "deny" {
		return []error{huma.Error401Unauthorized("bad signature")}
	}
	return nil
}

type CheckedTick struct {
	N int `json:"n"`
}

// newCheckedGat registers a query op, a body op, and a RegisterSSE op,
// each with input checks. reached counts handler entries.
func newCheckedGat(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	reached := &atomic.Int32{}

	gat.Register(api, g, huma.Operation{OperationID: "checked", Method: http.MethodGet, Path: "/checked"},
		func(ctx context.Context, in *checkedInput) (*checkedOutput, error) {
			reached.Add(1)
			out := &checkedOutput{}
			out.Body.V = in.Name
			return out, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "checkedBody", Method: http.MethodPost, Path: "/checked"},
		func(ctx context.Context, in *checkedBodyInput) (*checkedOutput, error) {
			reached.Add(1)
			out := &checkedOutput{}
			out.Body.V = in.Body.Code
			return out, nil
		})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "checkedTicks", Method: http.MethodGet, Path: "/ticks"},
		map[string]any{"tick": CheckedTick{}},
		func(ctx context.Context, in *checkedStreamInput, send sse.Sender) {
			reached.Add(1)
			_ = send.Data(CheckedTick{N: 1})
		})

	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatalf("RegisterGRPC: %v", err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, reached
}

// Pins: huma's input checks (parameter and body schema validation,
// then Resolvers) run on GraphQL dispatch. Before, GraphQL skipped
// them and the handler ran with input REST rejects.
func TestInputCheck_GraphQL(t *testing.T) {
	srv, reached := newCheckedGat(t)
	url := srv.URL + "/api/graphql"

	for _, tc := range []struct {
		name, query string
		status      float64 // 0: must succeed
		msg         string
	}{
		{"valid", `{ Demo { checked(name: "abc", count: 2) { v } } }`, 0, ""},
		{"optional_absent", `{ Demo { checked(name: "abc") { v } } }`, 0, ""},
		{"maxLength", `{ Demo { checked(name: "toolong") { v } } }`, 422, "expected length <= 3"},
		{"minimum", `{ Demo { checked(count: 0) { v } } }`, 422, "expected number >= 1"},
		{"resolver", `{ Demo { checked(name: "abc", sig: "deny") { v } } }`, 401, "bad signature"},
		{"body_valid", `mutation { Demo { checkedBody(body: {code: "abc"}) { v } } }`, 0, ""},
		{"body_pattern", `mutation { Demo { checkedBody(body: {code: "BAD1"}) { v } } }`, 422, "expected string to match pattern"},
		{"body_minLength", `mutation { Demo { checkedBody(body: {code: "abc", note: "x"}) { v } } }`, 422, "expected length >= 2"},
		{"body_nested_resolver", `mutation { Demo { checkedBody(body: {code: "abc", tag: {label: "x"}}) { v } } }`, 422, "label x is reserved (body.tag: x)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reached.Load()
			resp := graphQLAs(t, url, "", tc.query)
			errs, _ := resp["errors"].([]any)
			if tc.status == 0 {
				if len(errs) != 0 || reached.Load() != before+1 {
					t.Fatalf("want success, got %v", resp)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("want 1 error, got %v", resp)
			}
			if reached.Load() != before {
				t.Errorf("handler ran on rejected input")
			}
			e := errs[0].(map[string]any)
			if msg, _ := e["message"].(string); !strings.Contains(msg, tc.msg) {
				t.Errorf("message = %q, want it to contain %q", msg, tc.msg)
			}
			if st := digPath(e, "extensions", "status"); st != tc.status {
				t.Errorf("extensions.status = %v, want %v", st, tc.status)
			}
		})
	}

	// REST already ran the checks; the same inputs fail there the same way.
	for path, want := range map[string]int{
		"/checked?name=toolong":       http.StatusUnprocessableEntity,
		"/checked?name=abc&sig=deny":  http.StatusUnauthorized,
		"/checked?name=abc&count=2":   http.StatusOK,
		"/ticks?sig=deny":             http.StatusUnauthorized,
		"/checked?name=abc&count=0":   http.StatusUnprocessableEntity,
		"/checked?name=abc&sig=allow": http.StatusOK,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("REST %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func checkedFiles(t *testing.T, srvURL string) *protoregistry.Files {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srvURL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func grpcClient(srv *httptest.Server, md protoreflect.MethodDescriptor) *connect.Client[dynamicpb.Message, dynamicpb.Message] {
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	return connect.NewClient[dynamicpb.Message, dynamicpb.Message](
		srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}),
	)
}

// Pins: the same checks run on gRPC ingress. A proto3 field left unset
// is not sent, so count's minimum does not reject an absent count.
func TestInputCheck_GRPCIngress(t *testing.T) {
	srv, reached := newCheckedGat(t)
	files := checkedFiles(t, srv.URL)

	for _, tc := range []struct {
		op, in string
		code   connect.Code // 0: must succeed
	}{
		{"checked", `{"name": "abc"}`, 0},
		{"checked", `{"name": "toolong"}`, connect.CodeInvalidArgument},
		{"checked", `{"name": "abc", "sig": "deny"}`, connect.CodeUnauthenticated},
		{"checkedBody", `{"body": {"code": "abc"}}`, 0},
		{"checkedBody", `{"body": {"code": "BAD1"}}`, connect.CodeInvalidArgument},
		{"checkedBody", `{"body": {"code": "abc", "tag": {"label": "x"}}}`, connect.CodeInvalidArgument},
	} {
		md := findMethod(t, files, tc.op)
		in := dynamicpb.NewMessage(md.Input())
		if err := protojson.Unmarshal([]byte(tc.in), in); err != nil {
			t.Fatalf("%s %s: %v", tc.op, tc.in, err)
		}
		before := reached.Load()
		_, err := grpcClient(srv, md).CallUnary(context.Background(), connect.NewRequest(in))
		if tc.code == 0 {
			if err != nil {
				t.Errorf("%s %s: %v", tc.op, tc.in, err)
			}
			continue
		}
		if connect.CodeOf(err) != tc.code {
			t.Errorf("%s %s: err = %v, want %v", tc.op, tc.in, err, tc.code)
		}
		if reached.Load() != before {
			t.Errorf("%s %s: handler ran on rejected input", tc.op, tc.in)
		}
	}
}

// Pins: a RegisterSSE Resolver rejects before the stream starts, on
// every surface: REST 401, a GraphQL error frame carrying 401, gRPC
// Unauthenticated. This is how a handler rejects on its input.
func TestInputCheck_StreamResolverRejects(t *testing.T) {
	srv, reached := newCheckedGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if status, _ := readREST(t, ctx, srv.URL+"/ticks?sig=deny", ""); status != http.StatusUnauthorized {
		t.Errorf("REST status = %d, want 401", status)
	}

	conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "")
	wsSend(t, ctx, conn, subscribeMsg("1", `subscription { Demo_checkedTicks(sig: "deny") { n } }`))
	if m := wsRecv(t, ctx, conn); m.Type != "error" || !strings.Contains(string(m.Payload), `"status":401`) {
		t.Errorf("GraphQL: frame %s %s, want a 401 error frame", m.Type, m.Payload)
	}

	md := findMethod(t, checkedFiles(t, srv.URL), "checkedTicks")
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("sig"), protoreflect.ValueOfString("deny"))
	stream, err := grpcClient(srv, md).CallServerStream(ctx, connect.NewRequest(in))
	if err == nil {
		for stream.Receive() {
			t.Errorf("gRPC: event delivered on rejected input")
		}
		err = stream.Err()
		stream.Close()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("gRPC: err = %v, want Unauthenticated", err)
	}

	if n := reached.Load(); n != 0 {
		t.Errorf("handler ran %d times on rejected input", n)
	}

	// Allowed input still streams.
	conn2 := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "")
	wsSend(t, ctx, conn2, subscribeMsg("1", `subscription { Demo_checkedTicks(sig: "ok") { n } }`))
	if m := wsRecv(t, ctx, conn2); m.Type != "next" {
		t.Errorf("GraphQL allowed: frame %s %s, want next", m.Type, m.Payload)
	}
}
