package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// openAPI32Spec declares an OpenAPI 3.2 QUERY operation and a custom
// method under additionalOperations, both with a JSON request body.
const openAPI32Spec = `{
  "openapi": "3.2.0",
  "info": {"title": "test", "version": "1.0.0"},
  "paths": {
    "/things/search": {
      "query": {
        "operationId": "searchThings",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {
            "type": "object",
            "properties": {"name": {"type": "string"}}
          }}}
        },
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
        }}}}}
      }
    },
    "/things/{id}": {
      "additionalOperations": {
        "PURGE": {
          "operationId": "purgeThing",
          "parameters": [
            {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}
          ],
          "requestBody": {
            "content": {"application/json": {"schema": {
              "type": "object",
              "properties": {"reason": {"type": "string"}}
            }}}
          },
          "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
            "type": "object",
            "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
          }}}}}
        }
      }
    }
  }
}`

type openAPI32Fixture struct {
	gw      *Gateway
	lastReq atomic.Pointer[capturedRequest]
}

func newOpenAPI32Fixture(t *testing.T) *openAPI32Fixture {
	t.Helper()
	f := &openAPI32Fixture{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.lastReq.Store(&capturedRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(backend.Close)
	f.gw = New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(f.gw.Close)
	if err := f.gw.AddOpenAPIBytes([]byte(openAPI32Spec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	return f
}

func (f *openAPI32Fixture) postGraphQL(t *testing.T, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(query))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.gw.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	if errs, ok := out["errors"]; ok {
		t.Fatalf("graphql errors: %v", errs)
	}
	return out
}

func (f *openAPI32Fixture) assertBackend(t *testing.T, method, path, bodyKey, bodyVal string) {
	t.Helper()
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Method != method || rec.Path != path {
		t.Fatalf("backend got %s %s, want %s %s", rec.Method, rec.Path, method, path)
	}
	if ct := rec.Headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body, &body); err != nil {
		t.Fatalf("backend body not json: %v: %s", err, rec.Body)
	}
	if body[bodyKey] != bodyVal {
		t.Fatalf("backend body %s=%v want %s", bodyKey, body[bodyKey], bodyVal)
	}
}

// QUERY is a GraphQL Query field (not a Mutation) whose body arg is
// sent as the HTTP QUERY request body.
func TestOpenAPI32E2E_QueryMethod(t *testing.T) {
	f := newOpenAPI32Fixture(t)
	out := f.postGraphQL(t, `{"query":"{ test { searchThings(body:{name:\"widget\"}) { id name } } }"}`)
	got := out["data"].(map[string]any)["test"].(map[string]any)["searchThings"].(map[string]any)
	if got["id"] != "abc" {
		t.Fatalf("id=%v want abc", got["id"])
	}
	f.assertBackend(t, "QUERY", "/things/search", "name", "widget")
}

// Custom methods from additionalOperations are Mutation fields.
func TestOpenAPI32E2E_AdditionalOperation(t *testing.T) {
	f := newOpenAPI32Fixture(t)
	f.postGraphQL(t, `{"query":"mutation { test { purgeThing(id:\"42\", body:{reason:\"stale\"}) { id } } }"}`)
	f.assertBackend(t, "PURGE", "/things/42", "reason", "stale")
}

// The map path (Dispatch) sends the same request as the GraphQL
// byte-splice path (DispatchAppend) exercised above.
func TestOpenAPI32E2E_DispatchMapPath(t *testing.T) {
	f := newOpenAPI32Fixture(t)
	f.gw.Handler() // assemble schema + register dispatchers
	d := f.gw.dispatchers.Get(ir.MakeSchemaID("test", "v1", "searchThings"))
	if d == nil {
		t.Fatal("no dispatcher for searchThings")
	}
	res, err := d.Dispatch(context.Background(), map[string]any{"body": map[string]any{"name": "gadget"}})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if m, _ := res.(map[string]any); m["id"] != "abc" {
		t.Fatalf("result=%v", res)
	}
	f.assertBackend(t, "QUERY", "/things/search", "name", "gadget")
}

// REST ingress routes QUERY and custom methods at their declared
// method + path.
func TestOpenAPI32E2E_HTTPIngress(t *testing.T) {
	f := newOpenAPI32Fixture(t)
	h := f.gw.IngressHandler()
	for _, tc := range []struct{ method, path, key, val string }{
		{"QUERY", "/things/search", "name", "widget"},
		{"PURGE", "/things/7", "reason", "stale"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"`+tc.key+`":"`+tc.val+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		f.assertBackend(t, tc.method, tc.path, tc.key, tc.val)
	}
}

// A 3.2 spec converted to 3.0 keeps QUERY and custom methods in
// x-additional-operations; ingress routes them as it routes the 3.2
// fields.
func TestOpenAPI32_IngressRoutesConvertedSpec(t *testing.T) {
	src := newOpenAPI32Fixture(t)
	spec := specOf(t, getSchemaOpenAPI(t, src.gw, "?openapi=3.0").Body.Bytes())

	var lastReq atomic.Pointer[capturedRequest]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastReq.Store(&capturedRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes(spec, To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	h := gw.IngressHandler()
	for _, tc := range []struct{ method, path, wantPath, body string }{
		{"QUERY", "/things/search", "/things/search", `{"name":"n"}`},
		{"PURGE", "/things/abc", "/things/abc", `{"reason":"r"}`},
	} {
		lastReq.Store(nil)
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s %s: status=%d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
			continue
		}
		if rec := lastReq.Load(); rec == nil || rec.Method != tc.method || rec.Path != tc.wantPath || string(rec.Body) != tc.body {
			t.Errorf("%s %s: backend got %+v", tc.method, tc.path, rec)
		}
	}
}
