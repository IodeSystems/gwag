package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const int64BodySpec = `{
  "openapi": "3.0.3",
  "info": {"title": "counts", "version": "1.0.0"},
  "paths": {
    "/counts": {
      "post": {
        "operationId": "addCount",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "cents": {"type": "integer", "format": "int64"},
            "many":  {"type": "array", "items": {"type": "integer", "format": "int64"}},
            "inner": {"type": "object", "properties": {"n": {"type": "integer", "format": "int64"}}},
            "label": {"type": "string"}
          }
        }}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object", "properties": {"ok": {"type": "boolean"}}
        }}}}}
      }
    }
  }
}`

// A 64-bit integer arrives from the Connect ingress as a decimal
// string (proto JSON); the upstream JSON body must carry a number.
func TestOpenAPIInt64BodyFromConnect(t *testing.T) {
	var got string
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(be.Close)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(int64BodySpec), To(be.URL), As("counts")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.ConnectHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/counts.v1.Service/addCount", "application/json",
		strings.NewReader(`{"body":{"cents":"9007199254740993","many":["1","2"],"inner":{"n":"7"},"label":"12"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("connect: %d %s", resp.StatusCode, b)
	}
	const want = `{"cents":9007199254740993,"inner":{"n":7},"label":"12","many":[1,2]}`
	if got != want {
		t.Errorf("upstream body\n got %s\nwant %s", got, want)
	}
}
