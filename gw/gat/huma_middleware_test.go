package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type principalKey struct{}

// requireUser is the shape adopters write: reject without a header,
// otherwise attach the principal for the handler.
func requireUser(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		user := ctx.Header("X-User")
		if user == "" {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "login required")
			return
		}
		next(huma.WithValue(ctx, principalKey{}, user))
	}
}

// newMiddlewareGat registers getProject guarded by an op middleware
// (requireUser) and an API middleware that tags the response, so both
// layers are observable. The handler echoes the principal it received.
func newMiddlewareGat(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	var order []string
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		order = append(order, "api")
		next(ctx)
	})
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{
		OperationID: "getProject",
		Method:      http.MethodGet,
		Path:        "/projects/{id}",
		Middlewares: huma.Middlewares{func(ctx huma.Context, next func(huma.Context)) {
			order = append(order, "op")
			requireUser(api)(ctx, next)
		}},
	}, func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
		user, _ := ctx.Value(principalKey{}).(string)
		if user == "" {
			t.Errorf("handler reached without a principal")
		}
		return &getProjectOutput{Body: project{ID: in.ID, Name: user}}, nil
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
	return srv, &order
}

func graphQLAs(t *testing.T, url, user, query string) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"query": query})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, b)
	}
	return out
}

// Pins: huma middlewares (API stack, then op) run on GraphQL dispatch.
// Before the fix the handler ran with no principal.
func TestHumaMiddleware_GraphQL(t *testing.T) {
	srv, order := newMiddlewareGat(t)
	const q = `query { Demo { getProject(id: "p1") { id name } } }`

	t.Run("rejected", func(t *testing.T) {
		*order = nil
		resp := graphQLAs(t, srv.URL+"/api/graphql", "", q)
		if digPath(resp, "data", "Demo", "getProject") != nil {
			t.Fatalf("op resolved without auth: %v", resp)
		}
		errs, _ := resp["errors"].([]any)
		if len(errs) != 1 {
			t.Fatalf("want 1 error, got %v", resp)
		}
		e := errs[0].(map[string]any)
		if !strings.Contains(e["message"].(string), "login required") {
			t.Errorf("message = %v", e["message"])
		}
		if st := digPath(e, "extensions", "status"); st != float64(http.StatusUnauthorized) {
			t.Errorf("extensions.status = %v, want 401", st)
		}
		if strings.Join(*order, ",") != "api,op" {
			t.Errorf("middleware order = %v, want api,op", *order)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		resp := graphQLAs(t, srv.URL+"/api/graphql", "alice", q)
		if got := digPath(resp, "data", "Demo", "getProject", "name"); got != "alice" {
			t.Fatalf("principal not carried to handler: %v", resp)
		}
	})

	t.Run("rest_unchanged", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/projects/p1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("REST status = %d, want 401", resp.StatusCode)
		}
	})
}

// Pins: huma middlewares run on gRPC ingress, reading call headers.
func TestHumaMiddleware_GRPCIngress(t *testing.T) {
	srv, _ := newMiddlewareGat(t)

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "getProject")
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
		srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}),
	)
	call := func(user string) (*connect.Response[dynamicpb.Message], error) {
		in := dynamicpb.NewMessage(md.Input())
		in.Set(md.Input().Fields().ByName("id"), protoreflect.ValueOfString("p1"))
		req := connect.NewRequest(in)
		if user != "" {
			req.Header().Set("X-User", user)
		}
		return client.CallUnary(context.Background(), req)
	}

	if _, err := call(""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no user: err = %v, want Unauthenticated", err)
	}
	resp, err := call("bob")
	if err != nil {
		t.Fatalf("with user: %v", err)
	}
	if got := dynamicAsJSON(resp.Msg)["name"]; got != "bob" {
		t.Errorf("name = %v, want bob", got)
	}
}
