package gat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

const tickStreamSpec = `{
  "openapi": "3.2.0",
  "info": {"title": "ticks", "version": "1.0.0"},
  "paths": {
    "/sse": {"get": {"operationId": "sse",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"text/event-stream": {"itemSchema": {
        "type": "object", "required": ["data"],
        "properties": {"data": {"type": "string", "contentMediaType": "application/json",
          "contentSchema": {"$ref": "#/components/schemas/Tick"}}}}}}}}}},
    "/jsonl": {"get": {"operationId": "jsonl",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/seq": {"get": {"operationId": "seq",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"application/json-seq": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/forever": {"get": {"operationId": "forever",
      "responses": {"200": {"description": "ok", "content": {"application/x-ndjson": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/denied": {"get": {"operationId": "denied",
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}}
  },
  "components": {"schemas": {"Tick": {"type": "object", "required": ["n"],
    "properties": {"n": {"type": "integer"}}}}}
}`

// tickUpstream streams ticks 1..n in the path's framing, flushing
// after each. /forever streams until the client goes away and reports
// that on gone; /denied answers 403.
func tickUpstream(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	gone := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		frame := map[string]string{
			"/sse":     "data: {\"n\":%d}\n\n",
			"/jsonl":   "{\"n\":%d}\n",
			"/seq":     "\x1e{\"n\":%d}\n",
			"/forever": "{\"n\":%d}\n",
		}[r.URL.Path]
		switch r.URL.Path {
		case "/denied":
			http.Error(w, "nope", http.StatusForbidden)
			return
		case "/forever":
			for i := 1; ; i++ {
				if _, err := fmt.Fprintf(w, frame, i); err != nil {
					break
				}
				fl.Flush()
				select {
				case <-r.Context().Done():
					gone <- struct{}{}
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			return
		}
		var n int
		fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n)
		if r.URL.Path == "/sse" {
			fmt.Fprint(w, ": heartbeat\n\n")
		}
		for i := 1; i <= n; i++ {
			fmt.Fprintf(w, frame, i)
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, gone
}

func newStreamGat(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	up, gone := tickUpstream(t)
	doc, err := openapi3.NewLoader().LoadFromData([]byte(tickStreamSpec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "ticks", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, gone
}

func TestOpenAPIStream_GraphQLSubscription(t *testing.T) {
	srv, _ := newStreamGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, op := range []string{"sse", "jsonl", "seq"} {
		t.Run(op, func(t *testing.T) {
			conn := dialGraphQLWS(t, ctx, srv.URL, "")
			wsSend(t, ctx, conn, subscribeMsg("1", fmt.Sprintf(`subscription { ticks_%s(n: 3) { n } }`, op)))
			var got []int
			for {
				m := wsRecv(t, ctx, conn)
				if m.Type == "complete" {
					break
				}
				if m.Type != "next" {
					t.Fatalf("frame %s %s", m.Type, m.Payload)
				}
				var p struct {
					Data map[string]struct{ N int } `json:"data"`
				}
				if err := json.Unmarshal(m.Payload, &p); err != nil {
					t.Fatal(err)
				}
				got = append(got, p.Data["ticks_"+op].N)
			}
			if fmt.Sprint(got) != "[1 2 3]" {
				t.Fatalf("ticks = %v, want [1 2 3]", got)
			}
		})
	}
}

func TestOpenAPIStream_ClientCompleteClosesUpstream(t *testing.T) {
	srv, gone := newStreamGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL, "")
	wsSend(t, ctx, conn, subscribeMsg("1", `subscription { ticks_forever { n } }`))
	for i := 0; i < 2; i++ {
		if m := wsRecv(t, ctx, conn); m.Type != "next" {
			t.Fatalf("frame %s %s", m.Type, m.Payload)
		}
	}
	wsSend(t, ctx, conn, wsMsg{ID: "1", Type: "complete"})
	select {
	case <-gone:
	case <-ctx.Done():
		t.Fatal("upstream request still open after client complete")
	}
}

func TestOpenAPIStream_UpstreamErrorStatus(t *testing.T) {
	srv, _ := newStreamGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL, "")
	wsSend(t, ctx, conn, subscribeMsg("1", `subscription { ticks_denied { n } }`))
	m := wsRecv(t, ctx, conn)
	if m.Type != "error" {
		t.Fatalf("want error frame, got %s %s", m.Type, m.Payload)
	}
	var errs []struct {
		Message    string
		Extensions map[string]any
	}
	if err := json.Unmarshal(m.Payload, &errs); err != nil || len(errs) != 1 {
		t.Fatalf("payload %s: %v", m.Payload, err)
	}
	e := errs[0]
	if !strings.Contains(e.Message, "403") || e.Extensions["status"] != 403.0 || e.Extensions["code"] != "permission_denied" {
		t.Fatalf("error = %+v", e)
	}
}
