package gat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
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
    }},
    "/b": {"post": {
      "operationId": "bdy",
      "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Body"}}}},
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Res"}}}}}
    }}
  },
  "components": {"schemas": {
    "Res": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Body": {"type": "object", "properties": {"name": {"type": "string"}, "n": {"type": "integer"}}}
  }}
}`

// compatGat serves compatSpec through the gat lite path (gwag serve
// --openapi) and records the upstream request line.
type compatGat struct {
	h    http.Handler
	g    *gat.Gateway
	mu   sync.Mutex
	last string
	body string
}

func newCompatGat(t *testing.T, opts ...func(*gat.Gateway)) *compatGat {
	t.Helper()
	f := &compatGat{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.last = r.Method + " " + r.URL.RequestURI()
		f.body = string(b)
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

// TestAllowLossyInputCoercion_Lite: gat's AllowLossyInputCoercion on
// the lite path (gwag serve --openapi): Int 2.5 is rejected by
// default and truncated to 2 with it.
func TestAllowLossyInputCoercion_Lite(t *testing.T) {
	const q = `query($v: Int){ t { qry(i: $v){ id } } }`
	strict := newCompatGat(t)
	if msg, got := strict.call(t, q, `{"v":2.5}`); !strings.HasPrefix(msg, `Variable "$v" got invalid value 2.5.`) || got != "" {
		t.Fatalf("strict: error=%q upstream=%q", msg, got)
	}
	lossy := newCompatGat(t, func(g *gat.Gateway) { g.AllowLossyInputCoercion(true) })
	if msg, got := lossy.call(t, q, `{"v":2.5}`); msg != "" || got != "GET /q?i=2" {
		t.Fatalf("lossy: error=%q upstream=%q", msg, got)
	}
}

type lossyNInput struct {
	N int32 `query:"n"`
}

type lossyNEvent struct {
	N int32 `json:"n"`
}

// TestAllowLossyInputCoercion_WS: graphql-transport-ws subscriptions
// get the same coercion: Int 2.5 is an error frame by default and n=2
// with AllowLossyInputCoercion.
func TestAllowLossyInputCoercion_WS(t *testing.T) {
	for _, lossy := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict", true: "lossy"}[lossy], func(t *testing.T) {
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
			g := mustNewGat(t).AllowLossyInputCoercion(lossy)
			gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
				func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
					return &getProjectOutput{Body: project{ID: in.ID}}, nil
				})
			gat.Subscribe(api, g, huma.Operation{OperationID: "echoN"},
				func(ctx context.Context, in *lossyNInput) (<-chan *lossyNEvent, error) {
					out := make(chan *lossyNEvent, 1)
					out <- &lossyNEvent{N: in.N}
					close(out)
					return out, nil
				})
			if err := gat.RegisterHuma(api, g, "/api"); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "")
			p, _ := json.Marshal(map[string]any{
				"query":     `subscription($v: Int){ Demo_echoN(n: $v) { n } }`,
				"variables": map[string]any{"v": 2.5},
			})
			wsSend(t, ctx, conn, wsMsg{ID: "1", Type: "subscribe", Payload: p})
			m := wsRecv(t, ctx, conn)
			if lossy {
				if m.Type != "next" || !strings.Contains(string(m.Payload), `"n":2`) {
					t.Fatalf("lossy: %s %s", m.Type, m.Payload)
				}
				return
			}
			if m.Type != "error" || !strings.Contains(string(m.Payload), `got invalid value 2.5`) {
				t.Fatalf("strict: %s %s", m.Type, m.Payload)
			}
		})
	}
}

// TestLegacyNullArgs_Lite: on the gat lite path a null JSON body field
// is forwarded by default and dropped with LegacyNullArgs (set after
// New built the schema, as gwag serve does).
func TestLegacyNullArgs_Lite(t *testing.T) {
	const q = `mutation($v: String){ t { bdy(body: {name: $v, n: 1}){ id } } }`
	for _, c := range []struct {
		legacy bool
		want   string
	}{{false, `{"n":1,"name":null}`}, {true, `{"n":1}`}} {
		f := newCompatGat(t, func(g *gat.Gateway) { g.LegacyNullArgs(c.legacy) })
		if msg, _ := f.call(t, q, `{"v":null}`); msg != "" {
			t.Fatalf("legacy=%v: %s", c.legacy, msg)
		}
		f.mu.Lock()
		got := f.body
		f.mu.Unlock()
		if got != c.want {
			t.Errorf("legacy=%v: body %s, want %s", c.legacy, got, c.want)
		}
	}
}

// TestCompat_GatScalarOutput: gat dispatchers return decoded JSON that
// graphql-go serializes, so graphql-go v3.1.0's strict built-in scalar
// output applies on the lite path: a value the type cannot hold
// without loss is a field error (v2.1.1: truncated or null, no error).
var gatScalarOutputWant = map[string]string{
	"Int/2":   `{"data":{"o":{"out":{"i":2,"s":null,"b":null}}}}`,
	"Int/2.0": `{"data":{"o":{"out":{"i":2,"s":null,"b":null}}}}`,
	// v3: field error (v2.1.1: 2, no error).
	"Int/2.5": `{"data":{"o":{"out":{"i":null,"s":null,"b":null}}},"errors":[{"message":"Int cannot represent non-integer value: 2.5","locations":[{"line":1,"column":13}],"path":["o","out","i"]}]}`,
	// v3: field error (v2.1.1: null, no error).
	"Int/3e9": `{"data":{"o":{"out":{"i":null,"s":null,"b":null}}},"errors":[{"message":"Int cannot represent non 32-bit signed integer value: 3000000000","locations":[{"line":1,"column":13}],"path":["o","out","i"]}]}`,
	// Lossless: serialized.
	"String/num":  `{"data":{"o":{"out":{"i":null,"s":"5","b":null}}}}`,
	"Boolean/str": `{"data":{"o":{"out":{"i":null,"s":null,"b":true}}}}`,
}

func TestCompat_GatScalarOutput(t *testing.T) {
	var mu sync.Mutex
	reply := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	spec := `{"openapi":"3.0.0","info":{"title":"o","version":"1"},
 "paths":{"/o":{"get":{"operationId":"out","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Out"}}}}}}}},
 "components":{"schemas":{"Out":{"type":"object","properties":{"i":{"type":"integer"},"s":{"type":"string"},"b":{"type":"boolean"}}}}}}`
	doc, err := openapi3.NewLoader().LoadFromData([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	svc := ir.IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "o", "v1"
	ir.PopulateSchemaIDs(svc)
	g, err := gat.New(gat.ServiceRegistration{Service: svc, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := gat.RegisterHTTP(mux, g, "/"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, reply string }{
		{"Int/2", `{"i":2}`}, {"Int/2.0", `{"i":2.0}`}, {"Int/2.5", `{"i":2.5}`}, {"Int/3e9", `{"i":3000000000}`},
		{"String/num", `{"s":5}`}, {"Boolean/str", `{"b":"true"}`},
	} {
		mu.Lock()
		reply = c.reply
		mu.Unlock()
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ o { out { i s b } } }"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if got := strings.TrimSpace(rr.Body.String()); got != gatScalarOutputWant[c.name] {
			t.Errorf("%s:\n  got  %s\n  want %s", c.name, got, gatScalarOutputWant[c.name])
		}
	}
}
