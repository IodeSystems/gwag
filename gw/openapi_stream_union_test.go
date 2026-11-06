package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAPIStream_UnionVariants(t *testing.T) {
	schemas := `"components": {"schemas": {
    "Event": {"oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}],
      "discriminator": {"propertyName": "kind"}},
    "Cat": {"type": "object", "required": ["kind", "lives"], "properties": {"kind": {"type": "string"}, "lives": {"type": "integer"}}},
    "Dog": {"type": "object", "required": ["kind", "good"], "properties": {"kind": {"type": "string"}, "good": {"type": "boolean"}}}
  }}`
	mk := func(media, item string) string {
		return `{"openapi":"3.2.0","info":{"title":"pets","version":"1.0.0"},"paths":{"/events":{"get":{"operationId":"events","responses":{"200":{"description":"ok","content":{"` + media + `":{"itemSchema":` + item + `}}}}}}},` + schemas + `}`
	}
	ref := `{"$ref": "#/components/schemas/Event"}`
	cases := map[string]struct{ spec, body string }{
		"jsonl identity": {mk("application/jsonl", ref), "{\"kind\":\"Cat\",\"lives\":9}\n{\"kind\":\"Dog\",\"good\":true}\n"},
		"json-seq":       {mk("application/json-seq", ref), "\x1e{\"kind\":\"Cat\",\"lives\":9}\n\x1e{\"kind\":\"Dog\",\"good\":true}\n"},
		"sse data":       {mk("text/event-stream", `{"type":"object","properties":{"data":{"type":"string","contentMediaType":"application/json","contentSchema":`+ref+`}}}`), "data: {\"kind\":\"Cat\",\"lives\":9}\n\ndata: {\"kind\":\"Dog\",\"good\":true}\n\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, c.body) }))
			defer up.Close()
			gw := New(WithoutMetrics(), WithoutBackpressure(), WithoutSubscriptionAuth(), WithAdminToken([]byte("test")))
			defer gw.Close()
			if err := gw.AddOpenAPIBytes([]byte(c.spec), To(up.URL), As("pets")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn := streamWS(t, ctx, gw)
			subscribeWS(t, ctx, conn, "1", `subscription { pets_events { __typename ... on pets_Cat { lives } ... on pets_Dog { good } } }`)
			var got []string
			for {
				m, err := readWS(conn, ctx)
				if err != nil {
					t.Fatal(err)
				}
				if m.Type == msgComplete {
					break
				}
				got = append(got, string(m.Payload))
			}
			if s := strings.Join(got, ""); !strings.Contains(s, `"pets_Cat","lives":9`) || !strings.Contains(s, `"pets_Dog","good":true`) {
				t.Fatalf("got %v", got)
			}
		})
	}
}
