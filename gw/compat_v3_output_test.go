package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// graphql-go v3.1.0 behaviour changes outside variable input:
// (a) built-in scalar output is strict, (b) a null variable for a
// non-null argument is a field error, (c) enum values named true /
// false / null are rejected. These pin where they reach gwag.

var (
	// (a) gwag's OpenAPI, GraphQL and proto dispatchers write the
	// response bytes themselves (AppendDispatcher), so graphql-go never
	// serializes these fields: no v3 field error, and the upstream
	// value reaches the client as sent, even when the type cannot hold
	// it (2.5 for Int, 5 for String). Unchanged from v2.1.1. The gat
	// lite path does serialize: see gw/gat TestCompat_GatScalarOutput.
	compatOutOpenAPIWant = map[string]string{
		"Int/2.5":     `{"data":{"o":{"out":{"i":2.5,"f":null,"s":null,"b":null}}}}`,
		"Int/3e9":     `{"data":{"o":{"out":{"i":3000000000,"f":null,"s":null,"b":null}}}}`,
		"Int/str":     `{"data":{"o":{"out":{"i":"abc","f":null,"s":null,"b":null}}}}`,
		"Int/2.0":     `{"data":{"o":{"out":{"i":2.0,"f":null,"s":null,"b":null}}}}`,
		"Float/str":   `{"data":{"o":{"out":{"i":null,"f":"abc","s":null,"b":null}}}}`,
		"String/num":  `{"data":{"o":{"out":{"i":null,"f":null,"s":5,"b":null}}}}`,
		"Boolean/str": `{"data":{"o":{"out":{"i":null,"f":null,"s":null,"b":"true"}}}}`,
	}
	compatOutGraphQLWant = map[string]string{
		"Int/2.5":    `{"data":{"g":{"i":2.5,"s":null}}}`,
		"Int/3e9":    `{"data":{"g":{"i":3000000000,"s":null}}}`,
		"String/num": `{"data":{"g":{"i":null,"s":5}}}`,
	}
	// proto uint32 / fixed32 render as Int; values >= 2^31 are not a
	// GraphQL Int but pass on the append path.
	compatOutProtoWant = map[string]string{
		"u=5":          `{"data":{"nums":{"get":{"u":5,"i":7,"x":5}}}}`,
		"u=2147483647": `{"data":{"nums":{"get":{"u":2147483647,"i":7,"x":2147483647}}}}`,
		"u=2147483648": `{"data":{"nums":{"get":{"u":2147483648,"i":7,"x":2147483648}}}}`,
		"u=4294967295": `{"data":{"nums":{"get":{"u":4294967295,"i":7,"x":4294967295}}}}`,
	}

	// (b) v3.1.0: field error, upstream not called (v2.1.1: the
	// variable's default "dflt" was used).
	compatNullNonNullWant = map[string]string{
		"null":   `{"data":{"t":{"pth":null}},"errors":[{"message":"Argument \"id\" of non-null type \"String!\" must not be null.","locations":[{"line":1,"column":33}],"path":["t","pth"]}]} | upstream=`,
		"absent": `{"data":{"t":{"pth":{"id":"x"}}}} | upstream=GET /p/dflt`,
		"value":  `{"data":{"t":{"pth":{"id":"x"}}}} | upstream=GET /p/a`,
	}

	// (c) v3.1.0: an enum value named true / false / null fails the
	// schema build. Registration returns nil, and every request to the
	// gateway then answers the build error (v2.1.1: the enum had a
	// value named "true"). A name that is not a GraphQL name ("a-b")
	// fails the same way on v2.1.1 too.
	compatEnumWant = map[string]string{
		`openapi ["true","false","null"]`: `err=<nil> | graphql.NewSchema: Enum type T cannot include value: V.`,
		`openapi ["yes","no"]`:            `err=<nil> | {"data":{"__type":{"enumValues":[{"name":"yes"},{"name":"no"}]}}}`,
		`openapi ["a-b"]`:                 `err=<nil> | graphql.NewSchema: Names must match /^[_a-zA-Z][_a-zA-Z0-9]*$/ but "a-b" does not.`,
		"proto true/false/null":           `err=<nil> | graphql.NewSchema: Enum type En_V1_E cannot include value: V.`,
	}
)

// ---- (a) scalar output, OpenAPI upstream ----

const compatOutSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "out", "version": "1.0.0"},
  "paths": {"/o": {"get": {
    "operationId": "out",
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Out"}}}}}
  }}},
  "components": {"schemas": {"Out": {"type": "object", "properties": {
    "i": {"type": "integer"}, "f": {"type": "number"}, "s": {"type": "string"}, "b": {"type": "boolean"}
  }}}}
}`

func TestCompat_ScalarOutput_OpenAPI(t *testing.T) {
	var mu sync.Mutex
	reply := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(compatOutSpec), To(srv.URL), As("o")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	for _, c := range []struct{ name, reply string }{
		{"Int/2.5", `{"i":2.5}`}, {"Int/3e9", `{"i":3000000000}`}, {"Int/str", `{"i":"abc"}`}, {"Int/2.0", `{"i":2.0}`},
		{"Float/str", `{"f":"abc"}`}, {"String/num", `{"s":5}`}, {"Boolean/str", `{"b":"true"}`},
	} {
		mu.Lock()
		reply = c.reply
		mu.Unlock()
		_, body := compatPost(t, h, `{ o { out { i f s b } } }`, "")
		compatCheck(t, compatOutOpenAPIWant, c.name, body)
	}
}

// ---- (a) scalar output, GraphQL upstream ----

const compatOutIntrospection = `{"data":{"__schema":{
 "queryType":{"name":"Query"},"mutationType":null,"subscriptionType":null,
 "types":[{"kind":"OBJECT","name":"Query","fields":[
   {"name":"i","args":[],"type":{"kind":"SCALAR","name":"Int"}},
   {"name":"s","args":[],"type":{"kind":"SCALAR","name":"String"}}
 ]}]}}}`

func TestCompat_ScalarOutput_GraphQL(t *testing.T) {
	var mu sync.Mutex
	data := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "IntrospectionQuery") {
			_, _ = io.WriteString(w, compatOutIntrospection)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(w, `{"data":`+data+`}`)
	}))
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(srv.URL, As("g")); err != nil {
		t.Fatal(err)
	}
	h := gw.Handler()
	for _, c := range []struct{ name, data string }{
		{"Int/2.5", `{"i":2.5}`}, {"Int/3e9", `{"i":3000000000}`}, {"String/num", `{"s":5}`},
	} {
		mu.Lock()
		data = c.data
		mu.Unlock()
		_, body := compatPost(t, h, `{ g { i s } }`, "")
		compatCheck(t, compatOutGraphQLWant, c.name, body)
	}
}

// ---- (a) scalar output, proto upstream (uint32 -> Int) ----

const compatNumsProto = `syntax = "proto3";
package nums.v1;
service Nums { rpc Get(GetRequest) returns (GetResponse); }
message GetRequest {}
message GetResponse { uint32 u = 1; int32 i = 2; fixed32 x = 3; }
`

type compatNumsConn struct{ u uint32 }

func (c *compatNumsConn) Invoke(_ context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	m := reply.(proto.Message).ProtoReflect()
	fds := m.Descriptor().Fields()
	m.Set(fds.ByName("u"), protoreflect.ValueOfUint32(c.u))
	m.Set(fds.ByName("x"), protoreflect.ValueOfUint32(c.u))
	m.Set(fds.ByName("i"), protoreflect.ValueOfInt32(7))
	return nil
}

func (c *compatNumsConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("no streams")
}

func TestCompat_ScalarOutput_Proto(t *testing.T) {
	for _, u := range []uint32{5, 1<<31 - 1, 1 << 31, 4294967295} {
		gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
		t.Cleanup(gw.Close)
		if err := gw.AddProtoBytes("nums.proto", []byte(compatNumsProto), To(&compatNumsConn{u: u}), As("nums")); err != nil {
			t.Fatal(err)
		}
		_, body := compatPost(t, gw.Handler(), `{ nums { get { u i x } } }`, "")
		compatCheck(t, compatOutProtoWant, fmt.Sprintf("u=%d", u), body)
	}
}

// ---- (c) enum values named true / false / null ----

func TestCompat_EnumReservedNames(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"e","version":"1"},
 "paths":{"/e":{"get":{"operationId":"e",
 "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
   "c":{"$ref":"#/components/schemas/Color"},
   "inline":{"type":"string","enum":ENUM}}}}}}}}}},
 "components":{"schemas":{"Color":{"type":"string","enum":ENUM}}}}`
	for _, e := range []string{`["true","false","null"]`, `["yes","no"]`, `["a-b"]`} {
		gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
		t.Cleanup(gw.Close)
		err := gw.AddOpenAPIBytes([]byte(strings.ReplaceAll(spec, "ENUM", e)), To("http://127.0.0.1:1"), As("e"))
		_, body := compatPost(t, gw.Handler(), `{ __type(name: "e_Color") { enumValues { name } } }`, "")
		compatCheck(t, compatEnumWant, "openapi "+e, compatEnumNorm(fmt.Sprintf("err=%v | %s", err, body)))
	}
	p := `syntax = "proto3"; package en.v1; service S { rpc Get(R) returns (R); } enum E { true = 0; false = 1; null = 2; } message R { E e = 1; }`
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	err := gw.AddProtoBytes("en.proto", []byte(p), To(&compatNumsConn{}), As("en"))
	_, body := compatPost(t, gw.Handler(), `{ __typename }`, "")
	compatCheck(t, compatEnumWant, "proto true/false/null", compatEnumNorm(fmt.Sprintf("err=%v | %s", err, body)))
}

// compatEnumNorm trims the body and masks which enum and which reserved
// value the build error names (map order picks them).
func compatEnumNorm(s string) string {
	s = strings.TrimSpace(s)
	s = regexp.MustCompile(`cannot include value: (true|false|null)\.`).ReplaceAllString(s, "cannot include value: V.")
	return regexp.MustCompile(`Enum type e_\w+ `).ReplaceAllString(s, "Enum type T ")
}

// ---- (b) null variable for a non-null argument ----

func TestCompat_NullForNonNullArg(t *testing.T) {
	f := newCompatOpenAPI(t)
	q := `query($v: String = "dflt"){ t { pth(id: $v){ id } } }`
	for _, c := range []struct{ name, vars string }{{"null", `{"v":null}`}, {"absent", `{}`}, {"value", `{"v":"a"}`}} {
		f.reset()
		_, body := compatPost(t, f.h, q, c.vars)
		req, _ := f.seen()
		compatCheck(t, compatNullNonNullWant, c.name, fmt.Sprintf("%s | upstream=%s", body, req))
	}
}
