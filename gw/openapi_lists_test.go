package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const listsOpenAPISpec = `{
  "openapi": "3.0.0",
  "info": {"title": "test", "version": "1.0.0"},
  "paths": {
    "/things": {
      "get": {
        "operationId": "listThings",
        "parameters": [
          {"name": "ids", "in": "query", "schema": {"type": "array", "items": {"type": "string"}}}
        ],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "array",
          "items": {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}}
        }}}}}
      }
    }
  }
}`

// TestOpenAPIE2E_ArrayQueryParamAndArrayResponse: an array query param
// is a GraphQL list argument sent upstream as repeated keys, and an
// array response is a GraphQL list.
func TestOpenAPIE2E_ArrayQueryParamAndArrayResponse(t *testing.T) {
	var rawQuery atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a","name":"A"},{"id":"b","name":"B"}]`))
	}))
	defer backend.Close()
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	defer gw.Close()
	if err := gw.AddOpenAPIBytes([]byte(listsOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { listThings(ids: [\"a\", \"b\"]) { id name } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)

	var out struct {
		Data struct {
			Test struct {
				ListThings []map[string]any `json:"listThings"`
			} `json:"test"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	if len(out.Errors) > 0 || len(out.Data.Test.ListThings) != 2 || out.Data.Test.ListThings[1]["name"] != "B" {
		t.Fatalf("response: %s", rr.Body.String())
	}
	if got, _ := rawQuery.Load().(string); got != "ids=a&ids=b" {
		t.Errorf("upstream query %q, want ids=a&ids=b", got)
	}
}
