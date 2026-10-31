package gat_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

// An OpenAPI 3.2 QUERY operation: gat dispatches it with the QUERY
// method and the JSON body.
const gatQuerySpec = `{
  "openapi": "3.2.0",
  "info": {"title": "things", "version": "1.0.0"},
  "paths": {
    "/search": {
      "query": {
        "operationId": "searchThings",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object", "properties": {"name": {"type": "string"}}
        }}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object", "properties": {"id": {"type": "string"}}
        }}}}}
      }
    }
  }
}`

func TestGatOpenAPI32_QueryMethod(t *testing.T) {
	var gotMethod, gotName string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		gotName = body["name"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer backend.Close()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(gatQuerySpec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "things", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: backend.URL})
	if err != nil {
		t.Fatal(err)
	}
	q := `{ things { v1 { searchThings(body: {name: "w"}) { id } } } }`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":`+jsonString(q)+`}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"id":"x"`) {
		t.Fatalf("response %s", rr.Body.String())
	}
	if gotMethod != "QUERY" || gotName != "w" {
		t.Fatalf("backend got method=%q name=%q, want QUERY w", gotMethod, gotName)
	}
}
