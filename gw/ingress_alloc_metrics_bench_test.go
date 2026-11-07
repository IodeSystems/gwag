package gateway

import (
	"bytes"
	"net/http"
	"testing"
)

// Same requests as BenchmarkIngressAlloc_*, with the Prometheus sink
// on. The difference between the two sets is what metrics cost per
// request.

func BenchmarkIngressAllocMetrics_OpenAPI(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func([]byte) []byte { return []byte(`{"id":"x","name":"thing"}`) }}}
	gw := New(WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://upstream.invalid"), As("test"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ test { getThing(id:"x") { id name } } }`, `"name":"thing"`)
}

func BenchmarkIngressAllocMetrics_GraphQL(b *testing.B) {
	client := &http.Client{Transport: cannedRT{func(req []byte) []byte {
		if bytes.Contains(req, []byte("IntrospectionQuery")) {
			return []byte(petsIntrospection)
		}
		return []byte(`{"data":{"user":{"id":"x","name":"thing"}}}`)
	}}}
	gw := New(WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	b.Cleanup(gw.Close)
	if err := gw.AddGraphQL("http://upstream.invalid/graphql", As("pets"), OpenAPIClient(client)); err != nil {
		b.Fatal(err)
	}
	benchIngress(b, gw, `{ pets { user(id:"x") { id name } } }`, `"name":"thing"`)
}
