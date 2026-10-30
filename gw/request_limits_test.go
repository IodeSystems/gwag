package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// limitsGateway is gatewayWithMinimalOpenAPI with a backend that
// counts the calls it receives.
func limitsGateway(t *testing.T, opts ...Option) (http.Handler, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(be.Close)
	all := append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored"))}, opts...)
	gw := New(all...)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	return gw.Handler(), &calls
}

type limitsResp struct {
	Code   int
	Data   map[string]any
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	}
}

func postGraphQL(t *testing.T, h http.Handler, body any) limitsResp {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	out := limitsResp{Code: rr.Code}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d %q: %v", rr.Code, rr.Body.String(), err)
	}
	return out
}

func (r limitsResp) message() string {
	if len(r.Errors) == 0 {
		return ""
	}
	return r.Errors[0].Message
}

func (r limitsResp) code() string {
	if len(r.Errors) == 0 {
		return ""
	}
	s, _ := r.Errors[0].Extensions["code"].(string)
	return s
}

func aliased(n int) string {
	var b strings.Builder
	b.WriteString("{ test {")
	for i := range n {
		fmt.Fprintf(&b, ` a%d: getThing(id:"x") { id }`, i)
	}
	b.WriteString(" } }")
	return b.String()
}

func TestMaxOperations_DefaultRefusesBeforeDispatch(t *testing.T) {
	h, calls := limitsGateway(t)
	if r := postGraphQL(t, h, map[string]any{"query": aliased(DefaultMaxOperations)}); len(r.Errors) > 0 {
		t.Fatalf("at the cap: %v", r.Errors)
	}
	calls.Store(0)
	r := postGraphQL(t, h, map[string]any{"query": aliased(DefaultMaxOperations + 1)})
	if !strings.Contains(r.message(), "over the limit of 64") {
		t.Fatalf("over the cap: %+v", r)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("backend got %d calls for a refused request", n)
	}
}

func TestMaxOperations_CountsFragmentSpreads(t *testing.T) {
	h, _ := limitsGateway(t, WithMaxOperations(3))
	q := `fragment F on TestQueryNamespace { getThing(id:"x") { id } b: getThing(id:"y") { id } }
	      { test { ...F c: getThing(id:"z") { id } } other: test { ...F } }`
	r := postGraphQL(t, h, map[string]any{"query": q})
	if !strings.Contains(r.message(), "selects 5 operations") {
		t.Fatalf("got %+v", r)
	}
	h, _ = limitsGateway(t, WithMaxOperations(0))
	if r := postGraphQL(t, h, map[string]any{"query": aliased(200)}); len(r.Errors) > 0 {
		t.Fatalf("cap off: %v", r.Errors)
	}
}

func TestMaxOperations_WithoutDocCache(t *testing.T) {
	h, _ := limitsGateway(t, WithoutDocCache(), WithMaxOperations(1))
	r := postGraphQL(t, h, map[string]any{"query": aliased(2)})
	if !strings.Contains(r.message(), "over the limit of 1") {
		t.Fatalf("got %+v", r)
	}
}

func TestMaxDepth(t *testing.T) {
	h, _ := limitsGateway(t, WithMaxDepth(2))
	r := postGraphQL(t, h, map[string]any{"query": `{ test { getThing(id:"x") { id } } }`})
	if !strings.Contains(r.message(), "query depth 3 exceeds the limit of 2") {
		t.Fatalf("got %+v", r)
	}
}

func TestWithoutIntrospection(t *testing.T) {
	h, _ := limitsGateway(t, WithoutIntrospection())
	r := postGraphQL(t, h, map[string]any{"query": `{ __schema { queryType { name } } }`})
	if r.message() != "introspection is disabled" {
		t.Fatalf("got %+v", r)
	}
	if r := postGraphQL(t, h, map[string]any{"query": `{ __typename }`}); len(r.Errors) > 0 {
		t.Fatalf("__typename: %v", r.Errors)
	}
}

func TestMaxRequestBytes(t *testing.T) {
	h, _ := limitsGateway(t, WithMaxRequestBytes(64))
	big := map[string]any{"query": `{ test { getThing(id:"` + strings.Repeat("x", 100) + `") { id } } }`}
	if r := postGraphQL(t, h, big); r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length: %+v", r)
	}

	// Chunked: no Content-Length, so the limiting reader catches it.
	b, _ := json.Marshal(big)
	req := httptest.NewRequest(http.MethodPost, "/graphql", io.NopCloser(bytes.NewReader(b)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: %d %s", rr.Code, rr.Body.String())
	}

	if r := postGraphQL(t, h, map[string]any{"query": `{ __typename }`}); r.Code != http.StatusOK {
		t.Fatalf("small body: %+v", r)
	}
}

func TestTrustedDocuments(t *testing.T) {
	docs := map[string]string{"abc": `query Get { test { getThing(id:"42") { id } } }`}
	h, calls := limitsGateway(t, WithTrustedDocuments(docs, true))

	r := postGraphQL(t, h, map[string]any{"documentId": "sha256:abc"})
	if len(r.Errors) > 0 || calls.Load() != 1 {
		t.Fatalf("documentId: %+v calls=%d", r, calls.Load())
	}
	r = postGraphQL(t, h, map[string]any{"extensions": map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": "abc"}}})
	if len(r.Errors) > 0 || calls.Load() != 2 {
		t.Fatalf("APQ: %+v", r)
	}

	// GET by id.
	q := url.Values{"documentId": {"abc"}}
	req := httptest.NewRequest(http.MethodGet, "/graphql?"+q.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}

	if r := postGraphQL(t, h, map[string]any{"documentId": "nope"}); r.code() != "PERSISTED_QUERY_NOT_FOUND" {
		t.Fatalf("unknown id: %+v", r)
	}
	if r := postGraphQL(t, h, map[string]any{"query": `{ __typename }`}); r.Code != http.StatusBadRequest || r.code() != "TRUSTED_DOCUMENT_REQUIRED" {
		t.Fatalf("enforced query text: %+v", r)
	}

	// Not enforced: query text still runs, APQ retry with the query
	// and an unknown hash runs the query.
	h, _ = limitsGateway(t, WithTrustedDocuments(docs, false))
	if r := postGraphQL(t, h, map[string]any{"query": `{ __typename }`}); len(r.Errors) > 0 {
		t.Fatalf("open query: %+v", r)
	}
	r = postGraphQL(t, h, map[string]any{
		"query":      `{ __typename }`,
		"extensions": map[string]any{"persistedQuery": map[string]any{"sha256Hash": "unknown"}},
	})
	if len(r.Errors) > 0 {
		t.Fatalf("APQ retry: %+v", r)
	}
}

func TestTrustedDocuments_NotEnabled(t *testing.T) {
	h, _ := limitsGateway(t)
	if r := postGraphQL(t, h, map[string]any{"documentId": "abc"}); r.code() != "PERSISTED_QUERY_NOT_SUPPORTED" {
		t.Fatalf("got %+v", r)
	}
}

// A browser link to the GraphiQL page must not run the query it
// carries: that path would skip the limits and the GET-mutation rule.
func TestGraphiQLDoesNotExecuteLinkedQuery(t *testing.T) {
	h, calls := limitsGateway(t)
	q := url.Values{"query": {`{ test { getThing(id:"42") { id } } }`}}
	req := httptest.NewRequest(http.MethodGet, "/graphql?"+q.Encode(), nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "graphiql") {
		t.Fatalf("page: %d", rr.Code)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("GraphiQL page dispatched %d calls", n)
	}
}
