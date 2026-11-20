package gat_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

// TestEnumRename_Gat: on the gat lite path an enum value that is not a
// GraphQL name gets one (a-b → a_b, true → TRUE); the upstream still
// sends and receives the original value.
func TestEnumRename_Gat(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"e","version":"1"},
 "paths":{"/e":{"get":{"operationId":"pick",
  "parameters":[{"name":"c","in":"query","schema":{"$ref":"#/components/schemas/Color"}}],
  "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
   "c":{"$ref":"#/components/schemas/Color"},
   "cs":{"type":"array","items":{"$ref":"#/components/schemas/Color"}}}}}}}}}}},
 "components":{"schemas":{"Color":{"type":"string","enum":["a-b","true","ok"]}}}}`
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"c":"a-b","cs":["true","ok"]}`)
	}))
	t.Cleanup(srv.Close)
	doc, err := openapi3.NewLoader().LoadFromData([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "e", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/"); err != nil {
		t.Fatal(err)
	}
	q, _ := json.Marshal(`{ e { pick(c: TRUE) { c cs } } }`)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":`+string(q)+`}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if got, want := strings.TrimSpace(rr.Body.String()), `{"data":{"e":{"pick":{"c":"a_b","cs":["TRUE","ok"]}}}}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if last != "c=true" {
		t.Errorf("upstream query %q, want c=true", last)
	}
}
