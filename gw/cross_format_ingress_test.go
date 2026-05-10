package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// TestCrossFormatIngressMatrix asserts the (ingress × source) matrix
// promised by the IR-as-equalizer architecture: every registered
// service should be reachable through every ingress, regardless of
// the source's native format. Sources: proto (greeter), openapi
// (test), graphql (pets). Ingresses: GraphQL (gw.Handler), HTTP
// (gw.IngressHandler), gRPC (gw.GRPCUnknownHandler).
//
// The two gRPC×{openapi,graphql} cells are intentionally skipped —
// they're the open todo "gRPC ingress: second pass for slotKindOpenAPI
// + slotKindGraphQL" in docs/plan.md §2 (Cross-kind ingress
// completeness). Skipping with a referenced reason makes them
// visible-but-not-failing so the matrix doubles as a forcing function.
func TestCrossFormatIngressMatrix(t *testing.T) {
	f := newCrossFormatFixture(t)

	t.Run("graphql_x_proto", func(t *testing.T) {
		body := f.postGraphQL(t, `{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}`)
		if !strings.Contains(body, "hello world") {
			t.Fatalf("body=%s", body)
		}
	})
	t.Run("graphql_x_openapi", func(t *testing.T) {
		body := f.postGraphQL(t, `{"query":"{ test { getThing(id:\"42\") { id name } } }"}`)
		if !strings.Contains(body, `"abc"`) || !strings.Contains(body, `"thing"`) {
			t.Fatalf("body=%s", body)
		}
	})
	t.Run("graphql_x_graphql", func(t *testing.T) {
		body := f.postGraphQL(t, `{"query":"{ pets { users { id name role } } }"}`)
		if !strings.Contains(body, "alice") || !strings.Contains(body, "MEMBER") {
			t.Fatalf("body=%s", body)
		}
	})

	t.Run("http_x_proto", func(t *testing.T) {
		resp, err := http.Post(f.httpServer.URL+"/greeter.v1.GreeterService/Hello",
			"application/json", strings.NewReader(`{"name":"world"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "hello world") {
			t.Fatalf("body=%s", body)
		}
	})
	t.Run("http_x_openapi", func(t *testing.T) {
		resp, err := http.Get(f.httpServer.URL + "/things/42")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), `"id":"abc"`) {
			t.Fatalf("body=%s", body)
		}
	})
	t.Run("http_x_graphql", func(t *testing.T) {
		// Synthesized REST route per ir.RenderOpenAPI(svc) for a
		// graphql-origin service: /<ns>.<ver>.Service/<op>, GET for
		// OpQuery. Canonical-args dispatch synthesizes the upstream
		// query at construction.
		resp, err := http.Get(f.httpServer.URL + "/pets.v1.Service/users")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "alice") || !strings.Contains(string(body), "MEMBER") {
			t.Fatalf("body=%s", body)
		}
	})

	t.Run("grpc_x_proto", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := f.grpcClient.Hello(ctx, &greeterv1.HelloRequest{Name: "world"})
		if err != nil {
			t.Fatalf("Hello: %v", err)
		}
		if resp.GetGreeting() != "hello world" {
			t.Fatalf("greeting=%q", resp.GetGreeting())
		}
	})
	t.Run("grpc_x_openapi", func(t *testing.T) {
		t.Skip("plan §2: gRPC ingress second pass for slotKindOpenAPI not yet implemented (docs/plan.md tier 2)")
	})
	t.Run("grpc_x_graphql", func(t *testing.T) {
		t.Skip("plan §2: gRPC ingress second pass for slotKindGraphQL not yet implemented (docs/plan.md tier 2)")
	})
}

// crossFormatFixture is a single Gateway that has registered one of
// each source kind (proto / openapi / graphql) plus all three ingress
// servers — so the same gateway state is exercised across every cell.
// Single-fixture is deliberate: a per-cell fixture wouldn't catch
// cross-kind interference (e.g. ingress route table collisions).
type crossFormatFixture struct {
	gw         *Gateway
	httpServer *httptest.Server
	gqlServer  *httptest.Server
	grpcServer *grpc.Server
	grpcConn   *grpc.ClientConn
	grpcClient greeterv1.GreeterServiceClient
}

func (f *crossFormatFixture) postGraphQL(t *testing.T, query string) string {
	t.Helper()
	resp, err := http.Post(f.gqlServer.URL+"/graphql", "application/json", strings.NewReader(query))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Errors []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(body, &env)
	if len(env.Errors) > 0 {
		t.Fatalf("graphql errors: %s", body)
	}
	return string(body)
}

func newCrossFormatFixture(t *testing.T) *crossFormatFixture {
	t.Helper()

	// proto backend: in-process gRPC greeter on ephemeral port.
	protoLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proto listen: %v", err)
	}
	protoSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(protoSrv, &fakeGreeterServer{})
	go func() { _ = protoSrv.Serve(protoLis) }()
	t.Cleanup(protoSrv.Stop)

	// openapi backend: httptest server returning a fixed thing.
	openAPIBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(openAPIBackend.Close)

	// graphql backend: pets fixture from graphql_ingest_test (canned
	// introspection + scripted users response).
	rf := newRemoteFixture(t)
	rf.queryHandler = func(string, map[string]any) any {
		return map[string]any{
			"users": []map[string]any{
				{"id": "1", "name": "alice", "role": "ADMIN"},
				{"id": "2", "name": "bob", "role": "MEMBER"},
			},
		}
	}

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)

	if err := gw.AddProtoDescriptor(greeterv1.File_greeter_proto, To(protoLis.Addr().String()), As("greeter")); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(openAPIBackend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}

	gqlServer := httptest.NewServer(gw.Handler())
	t.Cleanup(gqlServer.Close)
	httpServer := httptest.NewServer(gw.IngressHandler())
	t.Cleanup(httpServer.Close)

	// Frontend gRPC server hosting the gateway's unknown handler.
	feLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("frontend listen: %v", err)
	}
	feSrv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = feSrv.Serve(feLis) }()
	t.Cleanup(feSrv.Stop)

	conn, err := grpc.NewClient(feLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial frontend: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &crossFormatFixture{
		gw:         gw,
		httpServer: httpServer,
		gqlServer:  gqlServer,
		grpcServer: feSrv,
		grpcConn:   conn,
		grpcClient: greeterv1.NewGreeterServiceClient(conn),
	}
}
