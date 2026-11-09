package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
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

// A literal body argument is shared by every request that runs the
// same cached document. Concurrent requests must not write to it: the
// in-place version crashed the process ("fatal error: concurrent map
// writes"), seen in examples/cutover; go test -race reports it too.
func TestOpenAPIBodyLiteralConcurrent(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(be.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(int64BodySpec), To(be.URL), As("counts")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	const q = `{"query":"mutation { counts { addCount(body: {label: \"x\", cents: 5, many: [1, 2], inner: {n: 7}}) { ok } } }"}`
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				resp, err := http.Post(srv.URL, "application/json", strings.NewReader(q))
				if err != nil {
					errs <- err.Error()
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(b) != `{"data":{"counts":{"addCount":{"ok":true}}}}` {
					errs <- string(b)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// integersAsNumbers leaves its input as it was.
func TestIntegersAsNumbersCopies(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData([]byte(int64BodySpec))
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Paths.Find("/counts").Post.RequestBody.Value.Content.Get("application/json").Schema.Value
	in := map[string]any{"cents": "7", "many": []any{"1"}, "inner": map[string]any{"n": "2"}}
	out := integersAsNumbers(in, schema).(map[string]any)
	if out["cents"] != json.Number("7") || out["many"].([]any)[0] != json.Number("1") || out["inner"].(map[string]any)["n"] != json.Number("2") {
		t.Fatalf("out %#v", out)
	}
	if in["cents"] != "7" || in["many"].([]any)[0] != "1" || in["inner"].(map[string]any)["n"] != "2" {
		t.Fatalf("input changed: %#v", in)
	}
}
