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

const compatSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "compat", "version": "1.0.0"},
  "paths": {
    "/q": {"get": {
      "operationId": "qry",
      "parameters": [
        {"name": "s", "in": "query", "schema": {"type": "string"}},
        {"name": "i", "in": "query", "schema": {"type": "integer"}},
        {"name": "f", "in": "query", "schema": {"type": "number"}}
      ],
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Res"}}}}}
    }}
  },
  "components": {"schemas": {"Res": {"type": "object", "properties": {"id": {"type": "string"}}}}}
}`

// compatGat serves compatSpec through the gat lite path (gwag serve
// --openapi) and records the upstream request line.
type compatGat struct {
	h    http.Handler
	g    *gat.Gateway
	mu   sync.Mutex
	last string
}

func newCompatGat(t *testing.T, opts ...func(*gat.Gateway)) *compatGat {
	t.Helper()
	f := &compatGat{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.last = r.Method + " " + r.URL.RequestURI()
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	t.Cleanup(srv.Close)
	doc, err := openapi3.NewLoader().LoadFromData([]byte(compatSpec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "t", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		o(g)
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/"); err != nil {
		t.Fatal(err)
	}
	f.g, f.h = g, mux
	return f
}

// call posts query with the raw variables JSON and returns the first
// error message (or "") and the upstream request line ("" = not called).
func (f *compatGat) call(t *testing.T, query, vars string) (string, string) {
	t.Helper()
	f.mu.Lock()
	f.last = ""
	f.mu.Unlock()
	q, _ := json.Marshal(query)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":`+string(q)+`,"variables":`+vars+`}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	var out struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	msg := ""
	if len(out.Errors) > 0 {
		msg = out.Errors[0].Message
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return msg, f.last
}

// TestCompat_GatOpenAPIParams: on the gat lite path a null query
// variable sends no parameter (graphql-go v3 hands it to the
// dispatcher as a present nil) and numbers have no exponent form.
func TestCompat_GatOpenAPIParams(t *testing.T) {
	f := newCompatGat(t)
	cases := []struct{ name, query, vars, want string }{
		{"null", `query($v: String){ t { qry(s: $v){ id } } }`, `{"v":null}`, "GET /q"},
		{"literal-null", `{ t { qry(s: null){ id } } }`, `{}`, "GET /q"},
		{"value", `query($v: String){ t { qry(s: $v){ id } } }`, `{"v":"a"}`, "GET /q?s=a"},
		{"float-3e9", `query($v: Float){ t { qry(f: $v){ id } } }`, `{"v":3000000000}`, "GET /q?f=3000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, got := f.call(t, c.query, c.vars)
			if msg != "" || got != c.want {
				t.Fatalf("error=%q upstream=%q, want %q", msg, got, c.want)
			}
		})
	}
}
