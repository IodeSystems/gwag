package gat_test

// Outbound dispatch benchmarks: a GraphQL request through gat's
// handler to a loopback upstream, gRPC (ProtoSource) and OpenAPI
// (ServiceRegistration.BaseURL). The upstream round trip is in the
// number; read allocs/op as gat's per-call cost on top of it.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

const benchThingSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "things", "version": "1.0.0"},
  "paths": {
    "/things/{id}": {
      "get": {
        "operationId": "getThing",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
        }}}}}
      }
    }
  }
}`

func runOutboundBench(b *testing.B, h http.Handler, query, want string) {
	body := `{"query":` + jsonString(query) + `}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if !strings.Contains(rr.Body.String(), want) {
			b.Fatalf("response %s", rr.Body.String())
		}
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func BenchmarkGatOutbound_Proto(b *testing.B) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, &protoTestGreeter{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	regs, err := gat.ProtoSource("greeter.proto", []byte(greeterProtoSrc), nil, lis.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	g, err := gat.New(regs...)
	if err != nil {
		b.Fatal(err)
	}
	runOutboundBench(b, g.Handler(), `{ greeter { v1 { hello(name: "x") { greeting } } } }`, `"greeting":"hello x"`)
}

func BenchmarkGatOutbound_OpenAPI(b *testing.B) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","name":"thing"}`))
	}))
	defer backend.Close()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(benchThingSpec))
	if err != nil {
		b.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "things", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: backend.URL})
	if err != nil {
		b.Fatal(err)
	}
	runOutboundBench(b, g.Handler(), `{ things { v1 { getThing(id: "x") { id name } } } }`, `"name":"thing"`)
}
