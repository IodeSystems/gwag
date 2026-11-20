package gateway

import (
	"fmt"
	"testing"
)

// TestCompat_* pin graphql-go behaviour through the real gateway. They
// recorded v2.1.1 (c5a2bb15) and were flipped case by case to v3.1.0;
// each flipped entry in the *Want maps carries a "v3:" comment with the
// v2.1.1 value. Each observation is what the upstream actually received
// (or what the client got back).
//
// A missing map entry fails with "GOT|name|value" so new cases can be
// pinned by copying the observed value. The per-case v3 notes live as
// comments in compat_v3_want_test.go.

func compatCheck(t *testing.T, want map[string]string, name, got string) {
	t.Helper()
	w, ok := want[name]
	if !ok {
		t.Errorf("GOT|%s|%s", name, got)
		return
	}
	if got != w {
		t.Errorf("%s:\n  got  %s\n  want %s", name, got, w)
	}
}

func compatNote(errs string, seen string) string {
	return fmt.Sprintf("error=%q | %s", errs, seen)
}

// ---------------------------------------------------------------------
// 1. JSON null / absent variable for a nullable argument.
// ---------------------------------------------------------------------

type compatNullCase struct {
	name  string
	query string
	vars  string
	v3    string // v3 note, "" when no change expected
}

// graphql-go v3: a null variable reaches resolvers as a present nil
// (args[key] exists with a nil value); v2.1.1 dropped it from args.
// Cases where the absent-vs-null difference is observable in the
// upstream request carry this note.
const compatNullV3 = "graphql-go v3: null variable reaches resolvers as present nil"

func TestCompat_NullVariable_Proto(t *testing.T) {
	f := newGRPCE2EFixture(t)
	q := `query($v: String){ greeter { hello(name: $v) { greeting } } }`
	cases := []compatNullCase{
		{"null", q, `{"v":null}`, compatNullV3},
		{"absent", q, `{}`, ""},
		{"literal-null", `{ greeter { hello(name: null) { greeting } } }`, ``, compatNullV3},
		{"value", q, `{"v":"bob"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.greeter.lastReq.Store(nil)
			before := f.greeter.helloCalls.Load()
			vars := c.vars
			status, body := compatPost(t, f.server.Config.Handler, c.query, vars)
			seen := "upstream not called"
			if f.greeter.helloCalls.Load() > before {
				seen = fmt.Sprintf("name=%q", f.greeter.lastReq.Load().GetName())
			}
			compatCheck(t, compatProtoNullWant, c.name, compatNote(compatErrs(body), fmt.Sprintf("http=%d %s", status, seen)))
		})
	}
}

func TestCompat_NullVariable_OpenAPI(t *testing.T) {
	f := newCompatOpenAPI(t)
	cases := []compatNullCase{
		// query param
		{"query/null", `query($v: String){ t { qry(s: $v){ id } } }`, `{"v":null}`, compatNullV3},
		{"query/absent", `query($v: String){ t { qry(s: $v){ id } } }`, `{}`, ""},
		{"query/literal-null", `{ t { qry(s: null){ id } } }`, ``, compatNullV3},
		{"query/value", `query($v: String){ t { qry(s: $v){ id } } }`, `{"v":"a"}`, ""},
		// path param (String!, so null is rejected before dispatch)
		{"path/null", `query($v: String!){ t { pth(id: $v){ id } } }`, `{"v":null}`, ""},
		{"path/absent", `query($v: String!){ t { pth(id: $v){ id } } }`, `{}`, ""},
		// JSON body field
		{"body-field/null", `mutation($v: String){ t { bdy(body:{name: $v}){ id } } }`, `{"v":null}`, compatNullV3},
		{"body-field/absent", `mutation($v: String){ t { bdy(body:{name: $v}){ id } } }`, `{}`, ""},
		{"body-field/literal-null", `mutation { t { bdy(body:{name: null}){ id } } }`, ``, compatNullV3},
		{"body-field/value", `mutation($v: String){ t { bdy(body:{name: $v}){ id } } }`, `{"v":"a"}`, ""},
		// nested body field
		{"body-nested-field/null", `mutation($v: String){ t { bdy(body:{nested:{x: $v}}){ id } } }`, `{"v":null}`, compatNullV3},
		{"body-nested-field/absent", `mutation($v: String){ t { bdy(body:{nested:{x: $v}}){ id } } }`, `{}`, ""},
		// nested object variable null
		{"body-nested-object/null", `mutation($v: t_BodyNestedInput){ t { bdy(body:{nested: $v}){ id } } }`, `{"v":null}`, compatNullV3},
		// whole body null
		{"body/null", `mutation($v: t_BodyInput){ t { bdy(body: $v){ id } } }`, `{"v":null}`, compatNullV3},
		{"body/absent", `mutation($v: t_BodyInput){ t { bdy(body: $v){ id } } }`, `{}`, ""},
		// Int field null
		{"body-int/null", `mutation($v: Int){ t { bdy(body:{count: $v}){ id } } }`, `{"v":null}`, compatNullV3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.reset()
			status, body := compatPost(t, f.h, c.query, c.vars)
			req, rb := f.seen()
			if req == "" {
				req = "upstream not called"
			}
			compatCheck(t, compatOpenAPINullWant, c.name, compatNote(compatErrs(body), fmt.Sprintf("http=%d %s body=%s", status, req, rb)))
		})
	}
}

func TestCompat_NullVariable_GraphQL(t *testing.T) {
	f := newCompatGraphQL(t)
	cases := []compatNullCase{
		{"scalar/null", `query($v: String){ g { echo(s: $v) } }`, `{"v":null}`, compatNullV3},
		{"scalar/absent", `query($v: String){ g { echo(s: $v) } }`, `{}`, ""},
		{"scalar/literal-null", `{ g { echo(s: null) } }`, ``, compatNullV3},
		{"scalar/value", `query($v: String){ g { echo(s: $v) } }`, `{"v":"a"}`, ""},
		{"input-field/null", `query($v: String){ g { echo(inp:{a: $v}) } }`, `{"v":null}`, compatNullV3},
		{"input-field/absent", `query($v: String){ g { echo(inp:{a: $v}) } }`, `{}`, ""},
		{"input-nested-field/null", `query($v: String){ g { echo(inp:{sub:{a: $v}}) } }`, `{"v":null}`, compatNullV3},
		{"input-object/null", `query($v: g_In){ g { echo(inp: $v) } }`, `{"v":null}`, compatNullV3},
		{"input-object/absent", `query($v: g_In){ g { echo(inp: $v) } }`, `{}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.reset()
			status, body := compatPost(t, f.h, c.query, c.vars)
			vars, q := f.seen()
			seen := "upstream not called"
			if q != "" {
				seen = fmt.Sprintf("upstream vars=%s query=%s", vars, compatFlat(q))
			}
			compatCheck(t, compatGraphQLNullWant, c.name, compatNote(compatErrs(body), fmt.Sprintf("http=%d %s", status, seen)))
		})
	}
}

func TestCompat_NullVariable_MCP(t *testing.T) {
	f := newCompatMCP(t)
	cases := []compatNullCase{
		{"string/null", `mutation($v: String){ m { probe(s: $v) } }`, `{"v":null}`, compatNullV3},
		{"string/absent", `mutation($v: String){ m { probe(s: $v) } }`, `{}`, ""},
		{"string/literal-null", `mutation { m { probe(s: null) } }`, ``, compatNullV3},
		{"string/value", `mutation($v: String){ m { probe(s: $v) } }`, `{"v":"a"}`, ""},
		{"number/null", `mutation($v: Float){ m { probe(n: $v) } }`, `{"v":null}`, compatNullV3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.reset()
			status, body := compatPost(t, f.h, c.query, c.vars)
			args := f.seen()
			if args == "" {
				args = "upstream not called"
			}
			compatCheck(t, compatMCPNullWant, c.name, compatNote(compatErrs(body), fmt.Sprintf("http=%d args=%s", status, args)))
		})
	}
}

// ---------------------------------------------------------------------
// 2. Scalar variable input into Int / Float / ID arguments.
// ---------------------------------------------------------------------

// compatScalarValues are the JSON literals sent as the variable.
//
// v3 notes: graphql-go v3.1.0 accepts lossless variable values ("2",
// 2.0, 1e2 for Int) and rejects lossy ones (2.5, "2.5", true for Int;
// true for Float; 2.5, true for ID). Rows without a note did not change.
var compatScalarValues = []struct{ name, json, v3 string }{
	{"2", `2`, ""},
	{"2.0", `2.0`, ""},
	{"2.5", `2.5`, "v3: rejected for Int and ID (v2.1.1: 2 / \"2.5\")"},
	{"str2", `"2"`, ""},
	{"str2.5", `"2.5"`, "v3: rejected for Int (v2.1.1: 2)"},
	{"true", `true`, "v3: rejected for Int, Float and ID (v2.1.1: 1 / \"true\")"},
	{"1e2", `1e2`, ""},
	{"3e9", `3000000000`, "v3: ID is \"3000000000\" (v2.1.1: \"3e+09\"); Int still rejects it"},
}

func TestCompat_ScalarVariable_GraphQLUpstream(t *testing.T) {
	f := newCompatGraphQL(t)
	for _, typ := range []struct{ gql, arg string }{{"Int", "i"}, {"Float", "f"}, {"ID", "id"}} {
		for _, v := range compatScalarValues {
			name := typ.gql + "/" + v.name
			t.Run(name, func(t *testing.T) {
				// v3: see compatScalarValues[].v3
				f.reset()
				q := fmt.Sprintf(`query($v: %s){ g { echo(%s: $v) } }`, typ.gql, typ.arg)
				status, body := compatPost(t, f.h, q, `{"v":`+v.json+`}`)
				vars, up := f.seen()
				seen := "upstream not called"
				if up != "" {
					seen = "upstream vars=" + vars
				}
				compatCheck(t, compatScalarGraphQLWant, name, compatNote(compatErrs(body), fmt.Sprintf("http=%d %s", status, seen)))
			})
		}
	}
}

func TestCompat_ScalarVariable_OpenAPIUpstream(t *testing.T) {
	f := newCompatOpenAPI(t)
	for _, typ := range []struct{ gql, arg string }{{"Int", "i"}, {"Float", "f"}} {
		for _, v := range compatScalarValues {
			name := typ.gql + "/" + v.name
			t.Run(name, func(t *testing.T) {
				// v3: see compatScalarValues[].v3
				f.reset()
				q := fmt.Sprintf(`query($v: %s){ t { qry(%s: $v){ id } } }`, typ.gql, typ.arg)
				status, body := compatPost(t, f.h, q, `{"v":`+v.json+`}`)
				req, _ := f.seen()
				if req == "" {
					req = "upstream not called"
				}
				compatCheck(t, compatScalarOpenAPIWant, name, compatNote(compatErrs(body), fmt.Sprintf("http=%d %s", status, req)))
			})
		}
	}
}

func TestCompat_ScalarVariable_MCPUpstream(t *testing.T) {
	f := newCompatMCP(t)
	for _, v := range compatScalarValues {
		name := "Float/" + v.name
		t.Run(name, func(t *testing.T) {
			// v3: see compatScalarValues[].v3
			f.reset()
			status, body := compatPost(t, f.h, `mutation($v: Float){ m { probe(n: $v) } }`, `{"v":`+v.json+`}`)
			args := f.seen()
			if args == "" {
				args = "upstream not called"
			}
			compatCheck(t, compatScalarMCPWant, name, compatNote(compatErrs(body), fmt.Sprintf("http=%d args=%s", status, args)))
		})
	}
}

// ---------------------------------------------------------------------
// 3. Nil-map / null object results.
// ---------------------------------------------------------------------

// graphql-go v3 resolves a nil map for an object field as null. These
// upstream paths assemble the result themselves, so the output did not
// change from v2.1.1.
func TestCompat_NullObjectResult_GraphQLUpstream(t *testing.T) {
	f := newCompatGraphQL(t)
	cases := []struct{ name, data, query string }{
		{"outer-null", `{"outer":null}`, `{ g { outer { name inner { x } innerNN { x } } } }`},
		{"outer-empty", `{"outer":{}}`, `{ g { outer { name inner { x } innerNN { x } } } }`},
		{"inner-null", `{"outer":{"inner":null,"innerNN":{"x":"a"}}}`, `{ g { outer { inner { x } innerNN { x } } } }`},
		{"inner-empty", `{"outer":{"inner":{},"innerNN":{"x":"a"}}}`, `{ g { outer { inner { x } innerNN { x } } } }`},
		{"innerNN-null", `{"outer":{"inner":null,"innerNN":null}}`, `{ g { outer { inner { x } innerNN { x } } } }`},
		{"innerNN-empty", `{"outer":{"inner":null,"innerNN":{}}}`, `{ g { outer { inner { x } innerNN { x } } } }`},
		{"outerNN-null", `{"outerNN":null}`, `{ g { outerNN { name } } }`},
		{"outerNN-empty", `{"outerNN":{}}`, `{ g { outerNN { name } } }`},
		{"data-null", `null`, `{ g { outer { name } } }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.mu.Lock()
			f.data = c.data
			f.mu.Unlock()
			status, body := compatPost(t, f.h, c.query, "")
			compatCheck(t, compatNullObjGraphQLWant, c.name, fmt.Sprintf("http=%d %s", status, body))
		})
	}
}

// Unchanged on graphql-go v3 (see TestCompat_NullObjectResult_GraphQLUpstream).
func TestCompat_NullObjectResult_OpenAPIUpstream(t *testing.T) {
	f := newCompatOpenAPI(t)
	q := `{ t { obj { name inner { x } innerNN { x } } } }`
	cases := []struct{ name, reply string }{
		{"outer-null", `null`},
		{"outer-empty", `{}`},
		{"inner-null", `{"inner":null,"innerNN":{"x":"a"}}`},
		{"inner-empty", `{"inner":{},"innerNN":{"x":"a"}}`},
		{"innerNN-null", `{"inner":null,"innerNN":null}`},
		{"innerNN-empty", `{"inner":null,"innerNN":{}}`},
		{"innerNN-missing", `{"inner":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.mu.Lock()
			f.reply = c.reply
			f.mu.Unlock()
			status, body := compatPost(t, f.h, q, "")
			compatCheck(t, compatNullObjOpenAPIWant, c.name, fmt.Sprintf("http=%d %s", status, body))
		})
	}
}
