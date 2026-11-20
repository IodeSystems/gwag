package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/server/servertest"
)

// Fixtures for the TestCompat_* tests. These pin graphql-go v2.1.1
// behaviour ahead of the v3 move; every observation is what the upstream
// actually received, captured server-side.

// compatPost posts a raw JSON GraphQL request body (so the test controls
// the exact variable literals: 2, 2.0, "2", null ...) and returns the
// status and response body.
func compatPost(t *testing.T, h http.Handler, query, varsJSON string) (int, string) {
	t.Helper()
	q, _ := json.Marshal(query)
	body := `{"query":` + string(q)
	if varsJSON != "" {
		body += `,"variables":` + varsJSON
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// compatErrs returns the first GraphQL error message of a response, or
// "" when there is none.
func compatErrs(body string) string {
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if len(out.Errors) == 0 {
		return ""
	}
	msg, _, _ := strings.Cut(out.Errors[0].Message, "\n")
	return msg
}

// compatFlat collapses whitespace so a multi-line query fits one line.
func compatFlat(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---- OpenAPI upstream ----

const compatOpenAPISpec = `{
  "openapi": "3.0.0",
  "info": {"title": "compat", "version": "1.0.0"},
  "paths": {
    "/q": {
      "get": {
        "operationId": "qry",
        "parameters": [
          {"name": "s", "in": "query", "schema": {"type": "string"}},
          {"name": "i", "in": "query", "schema": {"type": "integer"}},
          {"name": "f", "in": "query", "schema": {"type": "number"}}
        ],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Res"}}}}}
      }
    },
    "/p/{id}": {
      "get": {
        "operationId": "pth",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Res"}}}}}
      }
    },
    "/b": {
      "post": {
        "operationId": "bdy",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Body"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Res"}}}}}
      }
    },
    "/o": {
      "get": {
        "operationId": "obj",
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Outer"}}}}}
      }
    }
  },
  "components": {"schemas": {
    "Res": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Body": {"type": "object", "properties": {
      "name": {"type": "string"},
      "count": {"type": "integer"},
      "nested": {"type": "object", "properties": {"x": {"type": "string"}}}
    }},
    "Inner": {"type": "object", "properties": {"x": {"type": "string"}}},
    "Outer": {"type": "object", "required": ["innerNN"], "properties": {
      "name": {"type": "string"},
      "inner": {"$ref": "#/components/schemas/Inner"},
      "innerNN": {"$ref": "#/components/schemas/Inner"}
    }}
  }}
}`

type compatOpenAPI struct {
	gw    *Gateway
	h     http.Handler
	mu    sync.Mutex
	last  string // method + " " + RequestURI
	body  string // raw request body
	reply string // response JSON; default {"id":"x"}
}

func newCompatOpenAPI(t *testing.T) *compatOpenAPI {
	t.Helper()
	f := &compatOpenAPI{reply: `{"id":"x"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.last = r.Method + " " + r.URL.RequestURI()
		f.body = string(b)
		reply := f.reply
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(compatOpenAPISpec), To(srv.URL), As("t")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	f.gw = gw
	f.h = gw.Handler()
	return f
}

func (f *compatOpenAPI) seen() (reqLine, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.body
}

func (f *compatOpenAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last, f.body = "", ""
}

// ---- GraphQL upstream ----

const compatGQLIntrospection = `{"data":{"__schema":{
 "queryType":{"name":"Query"},"mutationType":null,"subscriptionType":null,
 "types":[
  {"kind":"OBJECT","name":"Query","fields":[
   {"name":"echo","args":[
     {"name":"s","type":{"kind":"SCALAR","name":"String"}},
     {"name":"i","type":{"kind":"SCALAR","name":"Int"}},
     {"name":"f","type":{"kind":"SCALAR","name":"Float"}},
     {"name":"id","type":{"kind":"SCALAR","name":"ID"}},
     {"name":"inp","type":{"kind":"INPUT_OBJECT","name":"In"}}
    ],"type":{"kind":"SCALAR","name":"String"}},
   {"name":"outer","args":[],"type":{"kind":"OBJECT","name":"Outer"}},
   {"name":"outerNN","args":[],"type":{"kind":"NON_NULL","ofType":{"kind":"OBJECT","name":"Outer"}}}
  ]},
  {"kind":"OBJECT","name":"Outer","fields":[
   {"name":"name","args":[],"type":{"kind":"SCALAR","name":"String"}},
   {"name":"inner","args":[],"type":{"kind":"OBJECT","name":"Inner"}},
   {"name":"innerNN","args":[],"type":{"kind":"NON_NULL","ofType":{"kind":"OBJECT","name":"Inner"}}}
  ]},
  {"kind":"OBJECT","name":"Inner","fields":[
   {"name":"x","args":[],"type":{"kind":"SCALAR","name":"String"}}
  ]},
  {"kind":"INPUT_OBJECT","name":"In","inputFields":[
   {"name":"a","type":{"kind":"SCALAR","name":"String"}},
   {"name":"sub","type":{"kind":"INPUT_OBJECT","name":"In"}}
  ]}
 ]}}}`

type compatGraphQL struct {
	gw   *Gateway
	h    http.Handler
	mu   sync.Mutex
	vars string // raw JSON "variables" the upstream received ("" if none)
	qry  string
	data string // "data" JSON the upstream returns
}

func newCompatGraphQL(t *testing.T) *compatGraphQL {
	t.Helper()
	f := &compatGraphQL{data: `{"echo":"ok"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "IntrospectionQuery") {
			_, _ = w.Write([]byte(compatGQLIntrospection))
			return
		}
		f.mu.Lock()
		f.vars = string(bytes.TrimSpace(req.Variables))
		f.qry = req.Query
		data := f.data
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"data":` + data + `}`))
	}))
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(srv.URL, As("g")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	f.gw = gw
	f.h = gw.Handler()
	return f
}

func (f *compatGraphQL) seen() (vars, query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vars, f.qry
}

func (f *compatGraphQL) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars, f.qry = "", ""
}

// ---- MCP upstream ----

type compatMCP struct {
	gw   *Gateway
	h    http.Handler
	mu   sync.Mutex
	args string // JSON of the arguments the tool received; "" = not called
}

func newCompatMCP(t *testing.T) *compatMCP {
	t.Helper()
	f := &compatMCP{}
	s := mcpserver.NewMCPServer("compat-mcp", "1.0.0", mcpserver.WithToolCapabilities(false))
	s.AddTool(
		mcp.NewTool("probe",
			mcp.WithString("s"),
			mcp.WithNumber("n"),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, _ := json.Marshal(req.GetArguments())
			f.mu.Lock()
			f.args = string(b)
			f.mu.Unlock()
			return mcp.NewToolResultText("ok"), nil
		},
	)
	srv := servertest.NewTestStreamableHTTPServer(s)
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	if err := gw.AddMCP(MCPHTTP, srv.URL, As("m")); err != nil {
		t.Fatalf("AddMCP: %v", err)
	}
	f.gw = gw
	f.h = gw.Handler()
	return f
}

func (f *compatMCP) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args
}

func (f *compatMCP) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args = ""
}
