package gatbench_test

// Cross-framework cost matrix for the gat workload.
//
// Every benchmark serves the SAME two operations over the SAME store
// (compare/gatbench/model) and differs only in the framework in front:
//
//	gat/REST       huma's own router               (gat adds nothing here)
//	gat/GraphQL    gat GraphQL ingress
//	gat/Connect    gat Connect handlers
//	gqlgen         hand-written GraphQL, generated resolvers
//	connect-go     hand-written proto service, generated messages
//	grpc-gateway   proto service + REST transcoding, in-process
//
// All of it runs through http.Handler.ServeHTTP against an
// httptest.ResponseRecorder. No sockets, no TLS, no loopback: a
// loopback round trip costs ~300-400µs on this class of host and
// would swamp every delta these benchmarks exist to expose.
//
// Read the results as ratios, not absolutes. What transfers between
// machines is "gat GraphQL costs N× gqlgen on the same workload"; the
// ns/op does not.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iodesystems/gwag/compare/gatbench/connectimpl"
	"github.com/iodesystems/gwag/compare/gatbench/gatimpl"
	"github.com/iodesystems/gwag/compare/gatbench/gqlgenimpl"
)

// serveOK drives one request and fails on a non-2xx or on a GraphQL
// `errors` key. Without the errors check a misspelled field would
// benchmark the (much cheaper) error branch and report a flattering
// number.
func serveOK(tb testing.TB, h http.Handler, req *http.Request) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		tb.Fatalf("%s %s: %d %s", req.Method, req.URL.Path, rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"errors"`) {
		tb.Fatalf("%s %s: errors in body: %s", req.Method, req.URL.Path, rec.Body.String())
	}
}

func graphqlReq(path, query string) *http.Request {
	payload, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// connectReq builds a Connect unary POST. Connect's JSON codec over
// HTTP/1.1 is a plain POST carrying the bare message, so the same
// handler a grpc-go client would reach is exercised without standing
// up an h2c server.
func connectReq(procedure, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	return req
}

// run is the shared benchmark body: one warm-up request to fault in
// caches (gat's schema, gqlgen's query LRU), then the timed loop.
func run(b *testing.B, h http.Handler, next func() *http.Request) {
	serveOK(b, h, next())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serveOK(b, h, next())
	}
}

func gatMux(b *testing.B) http.Handler {
	b.Helper()
	mux, err := gatimpl.New()
	if err != nil {
		b.Fatalf("gatimpl.New: %v", err)
	}
	return mux
}

// --- point lookup ------------------------------------------------------

func BenchmarkGet_GatREST(b *testing.B) {
	h := gatMux(b)
	run(b, h, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/projects/p1", nil)
	})
}

func BenchmarkGet_GatGraphQL(b *testing.B) {
	h := gatMux(b)
	run(b, h, func() *http.Request {
		return graphqlReq("/api/graphql", gatimpl.GetQuery)
	})
}

func BenchmarkGet_GatConnect(b *testing.B) {
	h := gatMux(b)
	proc := gatConnectProcedure(b, h, "getProject")
	run(b, h, func() *http.Request {
		return connectReq(proc, `{"id":"p1"}`)
	})
}

func BenchmarkGet_Gqlgen(b *testing.B) {
	h := gqlgenimpl.New()
	run(b, h, func() *http.Request {
		return graphqlReq("/graphql", gqlgenimpl.GetQuery)
	})
}

func BenchmarkGet_ConnectGo(b *testing.B) {
	h := connectimpl.NewConnect()
	run(b, h, func() *http.Request {
		return connectReq(connectimpl.GetProcedure, `{"id":"p1"}`)
	})
}

func BenchmarkGet_GRPCGateway(b *testing.B) {
	h, err := connectimpl.NewGRPCGateway()
	if err != nil {
		b.Fatalf("NewGRPCGateway: %v", err)
	}
	run(b, h, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/projects/p1", nil)
	})
}

// --- 25-row fan-out ----------------------------------------------------

func BenchmarkList_GatREST(b *testing.B) {
	h := gatMux(b)
	run(b, h, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/projects?limit=25", nil)
	})
}

func BenchmarkList_GatGraphQL(b *testing.B) {
	h := gatMux(b)
	run(b, h, func() *http.Request {
		return graphqlReq("/api/graphql", gatimpl.ListQuery)
	})
}

func BenchmarkList_GatConnect(b *testing.B) {
	h := gatMux(b)
	proc := gatConnectProcedure(b, h, "listProjects")
	run(b, h, func() *http.Request {
		return connectReq(proc, `{"limit":25}`)
	})
}

func BenchmarkList_Gqlgen(b *testing.B) {
	h := gqlgenimpl.New()
	run(b, h, func() *http.Request {
		return graphqlReq("/graphql", gqlgenimpl.ListQuery)
	})
}

func BenchmarkList_ConnectGo(b *testing.B) {
	h := connectimpl.NewConnect()
	run(b, h, func() *http.Request {
		return connectReq(connectimpl.ListProcedure, `{"limit":25}`)
	})
}

func BenchmarkList_GRPCGateway(b *testing.B) {
	h, err := connectimpl.NewGRPCGateway()
	if err != nil {
		b.Fatalf("NewGRPCGateway: %v", err)
	}
	run(b, h, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/projects?limit=25", nil)
	})
}
