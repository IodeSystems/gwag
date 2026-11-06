package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const nestedListSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "geo", "version": "1.0.0"},
  "paths": {"/regions": {"post": {
    "operationId": "putRegion",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}},
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}}}
  }}},
  "components": {"schemas": {
    "Point": {"type": "object", "required": ["lon", "lat"], "properties": {"lon": {"type": "number"}, "lat": {"type": "number"}}},
    "Region": {"type": "object", "required": ["points", "rings"], "properties": {
      "points": {"type": "array", "items": {"type": "array", "items": {"type": "number"}}},
      "rings":  {"type": "array", "items": {"type": "array", "items": {"$ref": "#/components/schemas/Point"}}}
    }}
  }}
}`

// Nested lists go upstream as nested JSON arrays, and a nested list of
// objects projects the selection (and __typename) on each element.
func TestOpenAPI_NestedList(t *testing.T) {
	var sent atomic.Pointer[[]byte]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"points":[[1,2],[3,4]],"rings":[[{"lon":1,"lat":2,"extra":true}]]}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(nestedListSpec), To(backend.URL), As("geo")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	query, _ := json.Marshal(map[string]any{
		"query": `mutation { geo { putRegion(body: {points: [[1, 2], [3, 4]], rings: [[{lon: 1, lat: 2}]]}) { points rings { __typename lon } } } }`,
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(query)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	want := `{"data":{"geo":{"putRegion":{"points":[[1,2],[3,4]],"rings":[[{"__typename":"geo_Point","lon":1}]]}}}}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Errorf("response = %s\nwant       %s", got, want)
	}
	body := sent.Load()
	if body == nil {
		t.Fatal("backend not called")
	}
	var up map[string]any
	if err := json.Unmarshal(*body, &up); err != nil {
		t.Fatalf("upstream body %s: %v", *body, err)
	}
	if b, _ := json.Marshal(up["points"]); string(b) != `[[1,2],[3,4]]` {
		t.Errorf("upstream points = %s", b)
	}
}
