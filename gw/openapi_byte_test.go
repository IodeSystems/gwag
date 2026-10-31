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

func TestOpenAPI_Validation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     []Option
		rejected bool
	}{
		{"default", nil, true},
		{"strict", []Option{WithValidation("strict")}, true},
		{"warn", []Option{WithValidation("warn")}, false},
		{"off", []Option{WithValidation("off")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":"aGk="}`))
			}))
			t.Cleanup(backend.Close)
			gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token"))}, tc.opts...)...)
			t.Cleanup(gw.Close)
			if err := gw.AddOpenAPIBytes([]byte(openAPIByteSpec), To(backend.URL), As("test")); err != nil {
				t.Fatalf("AddOpenAPIBytes: %v", err)
			}

			graphqlBody, _ := json.Marshal(map[string]any{
				"query": `mutation { test { putBlob(tag: "dGFn", body: {data: "not base64!"}) { data } } }`,
			})
			req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(graphqlBody)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			gw.Handler().ServeHTTP(rr, req)
			if got := strings.Contains(rr.Body.String(), "invalid base64 in body.data"); got != tc.rejected {
				t.Errorf("graphql: rejected=%v: %s", got, rr.Body.String())
			}

			req = httptest.NewRequest(http.MethodPost, "/blobs?tag=not%20base64!", strings.NewReader(`{"data":"aGk="}`))
			req.Header.Set("Content-Type", "application/json")
			rr = httptest.NewRecorder()
			gw.IngressHandler().ServeHTTP(rr, req)
			if got := rr.Code == http.StatusBadRequest; got != tc.rejected {
				t.Errorf("rest: status=%d body=%s", rr.Code, rr.Body.String())
			}

			want := int32(2)
			if tc.rejected {
				want = 0
			}
			if got := calls.Load(); got != want {
				t.Errorf("backend calls = %d, want %d", got, want)
			}
		})
	}
}

func TestWithValidation_PanicsOnUnknownMode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	WithValidation("lenient")
}
