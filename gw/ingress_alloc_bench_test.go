package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Per-request cost of the gateway alone, by upstream kind. The
// upstream is a canned http.RoundTripper, so no socket and no backend
// work lands in the numbers: allocs/op is what gwag itself allocates
// from GraphQL ingress to response bytes. Allocation counts do not
// depend on host load, so these numbers are usable on a busy box.

type cannedRT struct{ body func(req []byte) []byte }

func (c cannedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	var req []byte
	if r.Body != nil {
		req, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(c.body(req))),
		Request:    r,
	}, nil
}

func benchIngress(b *testing.B, gw *Gateway, query string, want string) {
	h := gw.Handler()
	body := []byte(`{"query":` + jsonQuote(query) + `}`)
	run := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := run(); !strings.Contains(rr.Body.String(), want) {
		b.Fatalf("response %s, want %s", rr.Body.String(), want)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		run()
	}
}

func BenchmarkIngressAlloc_OpenAPI(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func([]byte) []byte { return []byte(`{"id":"x","name":"thing"}`) }}}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://upstream.invalid"), As("test"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ test { getThing(id:"x") { id name } } }`, `"name":"thing"`)
}

func BenchmarkIngressAlloc_GraphQL(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func(req []byte) []byte {
		if bytes.Contains(req, []byte("IntrospectionQuery")) {
			return []byte(petsIntrospection)
		}
		return []byte(`{"data":{"user":{"id":"x","name":"thing"}}}`)
	}}}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddGraphQL("http://upstream.invalid/graphql", As("pets"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ pets { user(id:"x") { id name } } }`, `"name":"thing"`)
}

// Nested-namespace GraphQL upstream: the request lands on
// graphqlGroupDispatcher, which forwards the whole sub-selection.
func BenchmarkIngressAlloc_GraphQLGroup(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func(req []byte) []byte {
		if bytes.Contains(req, []byte("IntrospectionQuery")) {
			return []byte(nestedGreeterIntrospection)
		}
		return []byte(`{"data":{"greeter":{"hello":{"greeting":"hi"},"echo":"ack"}}}`)
	}}}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddGraphQL("http://upstream.invalid/graphql", As("svc"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ svc { greeter { hello(name:"alice") { greeting } echo(msg:"hi") } } }`, `"greeting":"hi"`)
}

// The same requests with a Runtime middleware registered. Any Runtime
// transform wraps OpenAPI and GraphQL dispatchers in the canonical
// chain (wrapCanonicalDispatcherWithChain), which is a separate
// dispatch path from the ones above.
func withHeaderInjector(gw *Gateway) {
	gw.Use(InjectPath("other.op.arg", func(context.Context, any) (any, error) { return nil, nil }))
}

func BenchmarkIngressAlloc_OpenAPIChain(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func([]byte) []byte { return []byte(`{"id":"x","name":"thing"}`) }}}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	withHeaderInjector(gw)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://upstream.invalid"), As("test"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ test { getThing(id:"x") { id name } } }`, `"name":"thing"`)
}

func BenchmarkIngressAlloc_GraphQLChain(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func(req []byte) []byte {
		if bytes.Contains(req, []byte("IntrospectionQuery")) {
			return []byte(petsIntrospection)
		}
		return []byte(`{"data":{"user":{"id":"x","name":"thing"}}}`)
	}}}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	withHeaderInjector(gw)
	if err := gw.AddGraphQL("http://upstream.invalid/graphql", As("pets"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ pets { user(id:"x") { id name } } }`, `"name":"thing"`)
}

// Proto upstream over loopback gRPC (the projection fixture). Plain is
// a selection with no fragments or directives; Collected needs field
// collection: a named fragment, @skip and a repeated field.
func benchIngressProto(b *testing.B, query, want string) {
	addr := startProjectionProtoUpstream(b)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddProtoBytes("proj.proto", []byte(projectionProtoSource), To(addr), As("proj")); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, query, want)
}

func BenchmarkIngressAlloc_ProtoPlain(b *testing.B) {
	benchIngressProto(b, `{ proj { thing(id:"x") { id name owner { name } tags { k } } } }`, `"tags":[{"k":"a"},{"k":"b"}]`)
}

func BenchmarkIngressAlloc_ProtoCollected(b *testing.B) {
	benchIngressProto(b, `query { proj { thing(id:"x") { ...F name @skip(if: true) id owner { name } tags { k } } } } fragment F on Proj_V1_Thing { id }`, `"tags":[{"k":"a"},{"k":"b"}]`)
}
