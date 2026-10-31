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

const openAPIByteSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "test", "version": "1.0.0"},
  "paths": {
    "/blobs": {
      "post": {
        "operationId": "putBlob",
        "parameters": [
          {"name": "tag", "in": "query", "schema": {"type": "string", "format": "byte"}}
        ],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {
            "type": "object",
            "properties": {"data": {"type": "string", "format": "byte"}}
          }}}
        },
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"data": {"type": "string", "format": "byte"}}
        }}}}}
      }
    },
    "/files": {
      "post": {
        "operationId": "putFile",
        "requestBody": {
          "required": true,
          "content": {"multipart/form-data": {"schema": {
            "type": "object",
            "properties": {
              "one": {"type": "string", "format": "byte"},
              "many": {"type": "array", "items": {"type": "string", "format": "byte"}}
            }
          }}}
        },
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

// format:byte is base64 text in JSON: it stays a string in both
// directions. Only multipart/form-data makes it a file.
func TestOpenAPI_FormatByte(t *testing.T) {
	var lastReq atomic.Pointer[capturedRequest]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastReq.Store(&capturedRequest{Method: r.Method, Path: r.URL.RequestURI(), Headers: r.Header.Clone(), Body: body})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"aGk="}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(openAPIByteSpec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	query, _ := json.Marshal(map[string]any{
		"query": `mutation { test { putBlob(tag: "dGFn", body: {data: "aGk="}) { data } } }`,
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(query)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	if want := `{"data":{"test":{"putBlob":{"data":"aGk="}}}}`; strings.TrimSpace(rr.Body.String()) != want {
		t.Errorf("response = %s, want %s", rr.Body.String(), want)
	}
	rec := lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Path != "/blobs?tag=dGFn" {
		t.Errorf("backend path = %s", rec.Path)
	}
	if ct := rec.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("backend content type = %s", ct)
	}
	if got := string(rec.Body); got != `{"data":"aGk="}` {
		t.Errorf("backend body = %s", got)
	}

	sdl := getSDL(t, gw)
	for _, want := range []string{"one: Upload", "many: [Upload"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}
}
