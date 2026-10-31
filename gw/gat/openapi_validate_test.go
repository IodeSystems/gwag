package gat_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

const gatByteSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "things", "version": "1.0.0"},
  "paths": {
    "/blobs": {
      "post": {
        "operationId": "putBlob",
        "parameters": [{"name": "tag", "in": "query", "schema": {"type": "string", "format": "byte"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object", "properties": {"id": {"type": "string"}}
        }}}}}
      }
    }
  }
}`

func TestGatValidation(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		rejected bool
	}{{"", true}, {"strict", true}, {"warn", false}, {"off", false}} {
		t.Run(tc.mode, func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x"}`))
			}))
			defer backend.Close()
			doc, err := openapi3.NewLoader().LoadFromData([]byte(gatByteSpec))
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
			g.Validation(tc.mode)
			q := `mutation { things { v1 { putBlob(tag: "not base64!") { id } } } }`
			req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":`+jsonString(q)+`}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			g.Handler().ServeHTTP(rr, req)
			if got := strings.Contains(rr.Body.String(), "invalid base64 in tag"); got != tc.rejected {
				t.Errorf("rejected=%v: %s", got, rr.Body.String())
			}
			if got := calls.Load() == 0; got != tc.rejected {
				t.Errorf("backend calls = %d", calls.Load())
			}
		})
	}
}
