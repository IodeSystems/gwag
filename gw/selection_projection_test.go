package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/IodeSystems/graphql-go/v2"
)

// A GraphQL response holds exactly the fields the query selected
// (spec §6.3 CollectFields). These tests send the same queries to an
// OpenAPI upstream and a downstream-GraphQL upstream whose responses
// carry more fields than any query selects, with and without a
// Runtime middleware. A Runtime middleware puts OpenAPI and GraphQL
// dispatchers behind wrapCanonicalDispatcherWithChain, a separate
// dispatch path.

const projectionOpenAPISpec = `{
  "openapi": "3.0.0",
  "info": {"title": "proj", "version": "1.0.0"},
  "paths": {
    "/things/{id}": {
      "get": {
        "operationId": "thing",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}
      }
    }
  },
  "components": {"schemas": {
    "Thing": {"type": "object", "properties": {
      "id": {"type": "string"},
      "name": {"type": "string"},
      "owner": {"$ref": "#/components/schemas/Owner"},
      "tags": {"type": "array", "items": {"$ref": "#/components/schemas/Tag"}}
    }},
    "Owner": {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}},
    "Tag": {"type": "object", "properties": {"k": {"type": "string"}, "v": {"type": "string"}}}
  }}
}`

// projectionThing is the upstream object. "extra" and "secret" are not
// in either schema; the other fields are, and no query selects all.
func projectionThing(id string) map[string]any {
	return map[string]any{
		"id":    id,
		"name":  "thing",
		"extra": "not-in-schema",
		"owner": map[string]any{"id": "o1", "name": "olga", "secret": "s"},
		"tags": []any{
			map[string]any{"k": "a", "v": "1", "x": "y"},
			map[string]any{"k": "b", "v": "2", "x": "y"},
		},
	}
}

// projectionGraphQLUpstream is a spec-compliant GraphQL server
// (graphql-go) for the same shape. Arguments on nested fields are not
// covered: ir.Field has no arguments, so the gateway's mirror of a
// downstream schema drops them.
func projectionGraphQLUpstream(t *testing.T) (*httptest.Server, *atomic.Pointer[string]) {
	t.Helper()
	owner := graphql.NewObject(graphql.ObjectConfig{Name: "Owner", Fields: graphql.Fields{
		"id":   &graphql.Field{Type: graphql.String},
		"name": &graphql.Field{Type: graphql.String},
	}})
	tag := graphql.NewObject(graphql.ObjectConfig{Name: "Tag", Fields: graphql.Fields{
		"k": &graphql.Field{Type: graphql.String},
		"v": &graphql.Field{Type: graphql.String},
	}})
	thing := graphql.NewObject(graphql.ObjectConfig{Name: "Thing", Fields: graphql.Fields{
		"id":    &graphql.Field{Type: graphql.String},
		"name":  &graphql.Field{Type: graphql.String},
		"owner": &graphql.Field{Type: owner},
		"tags":  &graphql.Field{Type: graphql.NewList(tag)},
	}})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{"thing": &graphql.Field{
			Type: thing,
			Args: graphql.FieldConfigArgument{"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
			Resolve: func(p graphql.ResolveParams) (any, error) {
				id, _ := p.Args["id"].(string)
				return projectionThing(id), nil
			},
		}},
	})})
	if err != nil {
		t.Fatal(err)
	}
	var last atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !strings.Contains(req.Query, "__schema") {
			last.Store(&req.Query)
		}
		res := graphql.Do(graphql.Params{Schema: schema, RequestString: req.Query, VariableValues: req.Variables, Context: r.Context()})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

type projectionCase struct {
	name     string
	query    string // "%NS%" stands for the namespace
	vars     map[string]any
	want     string // data under %NS%.thing (or the whole data when wantRoot)
	wantRoot bool
}

var projectionCases = []projectionCase{
	{name: "scalar", query: `{ %NS% { thing(id:"1") { id } } }`, want: `{"id":"1"}`},
	{name: "alias", query: `{ %NS% { thing(id:"1") { a: id b: name } } }`, want: `{"a":"1","b":"thing"}`},
	{name: "nested", query: `{ %NS% { thing(id:"1") { owner { name } } } }`, want: `{"owner":{"name":"olga"}}`},
	{name: "list", query: `{ %NS% { thing(id:"1") { tags { k } } } }`, want: `{"tags":[{"k":"a"},{"k":"b"}]}`},
	{name: "typename_requested", query: `{ %NS% { thing(id:"1") { __typename id owner { __typename } } } }`, want: `{"__typename":"%NS%_Thing","id":"1","owner":{"__typename":"%NS%_Owner"}}`},
	{name: "typename_aliased", query: `{ %NS% { thing(id:"1") { t: __typename } } }`, want: `{"t":"%NS%_Thing"}`},
	{name: "named_fragment", query: `query { %NS% { thing(id:"1") { ...F } } } fragment F on %NS%_Thing { id owner { id } }`, want: `{"id":"1","owner":{"id":"o1"}}`},
	{name: "nested_named_fragment", query: `query { %NS% { thing(id:"1") { owner { ...O } } } } fragment O on %NS%_Owner { name }`, want: `{"owner":{"name":"olga"}}`},
	{name: "inline_fragment", query: `{ %NS% { thing(id:"1") { ... on %NS%_Thing { name } } } }`, want: `{"name":"thing"}`},
	{name: "merged_fields", query: `{ %NS% { thing(id:"1") { owner { id } owner { name } id } } }`, want: `{"owner":{"id":"o1","name":"olga"},"id":"1"}`},
	{name: "fragment_overlap", query: `query { %NS% { thing(id:"1") { id ...F } } } fragment F on %NS%_Thing { id name }`, want: `{"id":"1","name":"thing"}`},
	{name: "skip_include", query: `{ %NS% { thing(id:"1") { id name @skip(if: true) owner @include(if: false) { id } } } }`, want: `{"id":"1"}`},
	{name: "skip_variable", query: `query($s: Boolean!) { %NS% { thing(id:"1") { id name @skip(if: $s) } } }`, vars: map[string]any{"s": true}, want: `{"id":"1"}`},
	{name: "top_arg_variable", query: `query($id: String!) { %NS% { thing(id: $id) { id } } }`, vars: map[string]any{"id": "7"}, want: `{"id":"7"}`},
	{
		name:     "two_dispatches_two_variables",
		query:    `query($a: String!, $b: String!) { x: %NS% { thing(id: $a) { id } } y: %NS% { thing(id: $b) { name } } }`,
		vars:     map[string]any{"a": "1", "b": "2"},
		want:     `{"x":{"thing":{"id":"1"}},"y":{"thing":{"name":"thing"}}}`,
		wantRoot: true,
	},
}

// ordered: compare key order too. The OpenAPI walker writes keys in
// selection order. A GraphQL upstream's response is spliced as sent,
// and the graphql-go test upstream encodes maps with sorted keys.
func runProjectionCases(t *testing.T, gw *Gateway, ns string, ordered bool) {
	t.Helper()
	h := gw.Handler()
	for _, c := range projectionCases {
		t.Run(c.name, func(t *testing.T) {
			query := strings.ReplaceAll(c.query, "%NS%", ns)
			body, _ := json.Marshal(map[string]any{"query": query, "variables": c.vars})
			req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			var env struct {
				Data   map[string]json.RawMessage `json:"data"`
				Errors []json.RawMessage          `json:"errors"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode %s: %v", rr.Body.String(), err)
			}
			if len(env.Errors) > 0 {
				t.Fatalf("errors: %s", rr.Body.String())
			}
			var got []byte
			if c.wantRoot {
				got = compactData(t, rr.Body.Bytes())
			} else {
				var nsObj map[string]json.RawMessage
				_ = json.Unmarshal(env.Data[ns], &nsObj)
				got = compactJSON(t, nsObj["thing"])
			}
			want := strings.ReplaceAll(c.want, "%NS%", ns)
			if !ordered {
				got, want = sortedJSON(t, got), string(sortedJSON(t, []byte(want)))
			}
			if string(got) != want {
				t.Errorf("query %s\n got  %s\n want %s", query, got, want)
			}
		})
	}
}

func sortedJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, _ := json.Marshal(v)
	return out
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.Bytes()
}

// compactData returns the "data" member of a response body with its
// key order intact.
func compactData(t *testing.T, body []byte) []byte {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	return compactJSON(t, env.Data)
}

func projectionGateways(t *testing.T, add func(*testing.T, *Gateway)) map[string]*Gateway {
	out := map[string]*Gateway{}
	for _, chain := range []bool{false, true} {
		gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
		t.Cleanup(gw.Close)
		name := "direct"
		if chain {
			name = "runtime_chain"
			gw.Use(InjectPath("other.op.arg", func(context.Context, any) (any, error) { return nil, nil }))
		}
		add(t, gw)
		out[name] = gw
	}
	return out
}

func TestSelectionProjection_OpenAPI(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(projectionThing(strings.TrimPrefix(r.URL.Path, "/things/")))
	}))
	t.Cleanup(backend.Close)
	for name, gw := range projectionGateways(t, func(t *testing.T, gw *Gateway) {
		if err := gw.AddOpenAPIBytes([]byte(projectionOpenAPISpec), To(backend.URL), As("proj")); err != nil {
			t.Fatal(err)
		}
	}) {
		t.Run(name, func(t *testing.T) { runProjectionCases(t, gw, "proj", true) })
	}
}

func TestSelectionProjection_GraphQL(t *testing.T) {
	upstream, _ := projectionGraphQLUpstream(t)
	for name, gw := range projectionGateways(t, func(t *testing.T, gw *Gateway) {
		if err := gw.AddGraphQL(upstream.URL, As("proj")); err != nil {
			t.Fatal(err)
		}
	}) {
		t.Run(name, func(t *testing.T) { runProjectionCases(t, gw, "proj", false) })
	}
}

// With a Runtime middleware, the GraphQL dispatcher sends the
// post-chain argument values, not the caller's literals.
func TestSelectionProjection_GraphQLChainRewritesTopArgs(t *testing.T) {
	upstream, last := projectionGraphQLUpstream(t)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	gw.Use(InjectPath("proj.thing.id", func(context.Context, any) (any, error) { return "rewritten", nil }, Hide(false)))
	if err := gw.AddGraphQL(upstream.URL, As("proj")); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"query":     `query($id: String!) { proj { thing(id: $id) { id owner { name } } } }`,
		"variables": map[string]any{"id": "incoming"},
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	if got, want := string(compactData(t, rr.Body.Bytes())), `{"proj":{"thing":{"id":"rewritten","owner":{"name":"olga"}}}}`; got != want {
		t.Errorf("got %s, want %s (body %s, upstream query %v)", got, want, rr.Body.String(), deref(last.Load()))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestScanJSONMembers(t *testing.T) {
	raw := []byte(` { "a" : 1 , "bb": {"x": "}]\"{"}, "c": [1, {"d": [ ]}], "a": "last", "e": null , "f": -1.5e3 } `)
	names := []string{"a", "bb", "c", "e", "f", "missing", "a"}
	vals := make([][]byte, len(names))
	if err := scanJSONMembers(raw, names, vals); err != nil {
		t.Fatal(err)
	}
	want := []string{`"last"`, `{"x": "}]\"{"}`, `[1, {"d": [ ]}]`, `null`, `-1.5e3`, ``, `"last"`}
	for i := range names {
		if string(vals[i]) != want[i] {
			t.Errorf("%s = %q, want %q", names[i], vals[i], want[i])
		}
	}
	for _, bad := range []string{`[1]`, `{"a" 1}`, `{"a": 1`, `{"a": "x}`, `{"a": 1 "b": 2}`} {
		if err := scanJSONMembers([]byte(bad), names, make([][]byte, len(names))); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

// An upstream body that is not JSON is an error, not bytes spliced
// into the response.
func TestSelectionProjection_OpenAPIInvalidJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": "1", "name": `))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(projectionOpenAPISpec), To(backend.URL), As("proj")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ proj { thing(id:\"1\") { id } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	var env struct {
		Data   map[string]any    `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not JSON: %s", rr.Body.String())
	}
	if len(env.Errors) == 0 || !strings.Contains(rr.Body.String(), "not valid JSON") {
		t.Fatalf("want a not-valid-JSON error, got %s", rr.Body.String())
	}
}
