package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// Enum values that are not GraphQL enum names get a valid name; the
// wire value stays the upstream's in both directions.

const enumRenameSpec = `{"openapi":"3.0.0","info":{"title":"e","version":"1"},
 "paths":{"/e":{"get":{"operationId":"pick",
  "parameters":[{"name":"c","in":"query","schema":{"$ref":"#/components/schemas/Color"}}],
  "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{
   "c":{"$ref":"#/components/schemas/Color"},
   "cs":{"type":"array","items":{"$ref":"#/components/schemas/Color"}},
   "inline":{"type":"string","enum":["x y","ok"]}}}}}}}}}},
 "components":{"schemas":{"Color":{"type":"string","enum":["a-b","a_b","true","1st","ok","__x",""]}}}}`

func TestEnumRename_OpenAPI(t *testing.T) {
	var mu sync.Mutex
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"c":"a-b","cs":["true","1st","ok","__x",""],"inline":"x y"}`))
	}))
	t.Cleanup(srv.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(enumRenameSpec), To(srv.URL), As("e")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	h := gw.Handler()

	_, body := compatPost(t, h, `{ __type(name: "e_Color") { enumValues { name description } } }`, "")
	want := `{"data":{"__type":{"enumValues":[` +
		`{"name":"TRUE","description":"Upstream value: \"true\""},` +
		`{"name":"_","description":"Upstream value: \"\""},` +
		`{"name":"_1st","description":"Upstream value: \"1st\""},` +
		`{"name":"_x","description":"Upstream value: \"__x\""},` +
		`{"name":"a_b","description":""},` +
		`{"name":"a_b_2","description":"Upstream value: \"a-b\""},` +
		`{"name":"ok","description":""}]}}}`
	if got := sortedEnumValues(t, body); got != want {
		t.Errorf("enum values:\n got %s\nwant %s", got, want)
	}

	for _, c := range []struct{ lit, wire string }{
		{"a_b_2", "c=a-b"},
		{"TRUE", "c=true"},
		{"_1st", "c=1st"},
		{"a_b", "c=a_b"},
	} {
		_, body = compatPost(t, h, `{ e { pick(c: `+c.lit+`) { c cs inline } } }`, "")
		wantBody := `{"data":{"e":{"pick":{"c":"a_b_2","cs":["TRUE","_1st","ok","_x","_"],"inline":"x_y"}}}}`
		if strings.TrimSpace(body) != wantBody {
			t.Errorf("pick(c: %s):\n got %s\nwant %s", c.lit, body, wantBody)
		}
		mu.Lock()
		q := gotQuery
		mu.Unlock()
		if q != c.wire {
			t.Errorf("pick(c: %s): upstream query %q, want %q", c.lit, q, c.wire)
		}
	}

	// Variables take the GraphQL name too.
	_, body = compatPost(t, h, `query($c: e_Color){ e { pick(c: $c) { c } } }`, `{"c":"TRUE"}`)
	if strings.TrimSpace(body) != `{"data":{"e":{"pick":{"c":"a_b_2"}}}}` {
		t.Errorf("variable: %s", body)
	}
	mu.Lock()
	q := gotQuery
	mu.Unlock()
	if q != "c=true" {
		t.Errorf("variable: upstream query %q, want c=true", q)
	}
}

// The names depend only on the set of values, not their order.
func TestEnumRename_OrderIndependent(t *testing.T) {
	a := strings.Replace(enumRenameSpec, `["a-b","a_b","true","1st","ok","__x",""]`, `["","ok","1st","true","a_b","a-b","__x"]`, 1)
	sdl := func(spec string) string {
		gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
		t.Cleanup(gw.Close)
		if err := gw.AddOpenAPIBytes([]byte(spec), To("http://127.0.0.1:1"), As("e")); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		gw.SchemaHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/schema/graphql", nil))
		return rr.Body.String()
	}
	if x, y := sdl(enumRenameSpec), sdl(a); x != y {
		t.Errorf("SDL depends on enum value order:\n%s\n---\n%s", x, y)
	}
}

const enumRenameProto = `syntax = "proto3";
package en.v1;
service S { rpc Get(R) returns (R); }
enum E { true = 0; false = 1; null = 2; ok = 3; }
message R { E e = 1; repeated E es = 2; }`

// enumRenameConn echoes the request enum back and records it.
type enumRenameConn struct {
	mu  sync.Mutex
	got int32
}

func (c *enumRenameConn) Invoke(_ context.Context, _ string, args, reply any, _ ...grpc.CallOption) error {
	in := args.(proto.Message).ProtoReflect()
	m := reply.(proto.Message).ProtoReflect()
	fds := m.Descriptor().Fields()
	e := in.Get(in.Descriptor().Fields().ByName("e")).Enum()
	c.mu.Lock()
	c.got = int32(e)
	c.mu.Unlock()
	m.Set(fds.ByName("e"), protoreflect.ValueOfEnum(e))
	l := m.Mutable(fds.ByName("es")).List()
	for _, n := range []protoreflect.EnumNumber{0, 1, 2, 3} {
		l.Append(protoreflect.ValueOfEnum(n))
	}
	return nil
}

func (c *enumRenameConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("no streams")
}

func TestEnumRename_Proto(t *testing.T) {
	conn := &enumRenameConn{}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
	t.Cleanup(gw.Close)
	if err := gw.AddProtoBytes("en.proto", []byte(enumRenameProto), To(conn), As("en")); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	h := gw.Handler()
	_, body := compatPost(t, h, `{ __type(name: "En_V1_E") { enumValues { name } } }`, "")
	if got, want := sortedEnumValues(t, body), `{"data":{"__type":{"enumValues":[{"name":"FALSE"},{"name":"NULL"},{"name":"TRUE"},{"name":"ok"}]}}}`; got != want {
		t.Errorf("enum values:\n got %s\nwant %s", got, want)
	}
	for _, c := range []struct {
		lit string
		num int32
	}{{"TRUE", 0}, {"FALSE", 1}, {"NULL", 2}, {"ok", 3}} {
		_, body = compatPost(t, h, `{ en { get(e: `+c.lit+`) { e es } } }`, "")
		want := `{"data":{"en":{"get":{"e":"` + c.lit + `","es":["TRUE","FALSE","NULL","ok"]}}}}`
		if strings.TrimSpace(body) != want {
			t.Errorf("get(e: %s):\n got %s\nwant %s", c.lit, body, want)
		}
		conn.mu.Lock()
		got := conn.got
		conn.mu.Unlock()
		if got != c.num {
			t.Errorf("get(e: %s): upstream got %d, want %d", c.lit, got, c.num)
		}
	}
}

// sortedEnumValues sorts __type.enumValues by name so the comparison
// does not depend on graphql-go's value order.
func sortedEnumValues(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		Data struct {
			Type struct {
				EnumValues []map[string]any `json:"enumValues"`
			} `json:"__type"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	vs := r.Data.Type.EnumValues
	sort.Slice(vs, func(i, j int) bool { return vs[i]["name"].(string) < vs[j]["name"].(string) })
	var b strings.Builder
	b.WriteString(`{"data":{"__type":{"enumValues":[`)
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}
		if d, ok := v["description"]; ok {
			nb, _ := json.Marshal(v["name"])
			db, _ := json.Marshal(d)
			fmt.Fprintf(&b, `{"name":%s,"description":%s}`, nb, db)
		} else {
			nb, _ := json.Marshal(v["name"])
			fmt.Fprintf(&b, `{"name":%s}`, nb)
		}
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// The MCP schema corpus describes the GraphQL schema: enum values carry
// their GraphQL names.
func TestEnumRename_MCPCorpus(t *testing.T) {
	out := expandTypePtr(&ir.Type{Name: "C", TypeKind: ir.TypeEnum, Enum: []ir.EnumValue{{Name: "a-b"}, {Name: "ok"}, {Name: "null"}}})
	var got []string
	for _, v := range out.EnumValues {
		got = append(got, v.Name)
	}
	if strings.Join(got, ",") != "a_b,ok,NULL" {
		t.Errorf("corpus enum values %v, want [a_b ok NULL]", got)
	}
}
