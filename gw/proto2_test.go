package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const proto2Source = `syntax = "proto2";
package p2.v1;
enum Kind { KIND_A = 1; KIND_B = 2; }
message Req {
  required string name = 1;
  optional int32 n = 2 [default = 7];
  repeated int32 xs = 3;
  optional group Grp = 4 { optional string g = 5; }
}
message Resp {
  required string name = 1;
  optional int32 n = 2 [default = 7];
  optional Kind kind = 3 [default = KIND_B];
  repeated int32 xs = 4;
  optional group Grp = 5 { optional string g = 6; }
}
service Svc { rpc Do(Req) returns (Resp); }
`

// A proto2 upstream: required fields are non-null args, repeated
// fields are list args, groups convert like messages, and an unset
// optional field reads null (proto2 enums need not have a 0 value).
func TestProto2Upstream(t *testing.T) {
	fd, err := compileProtoBytes("p2.proto", []byte(proto2Source), nil)
	if err != nil {
		t.Fatal(err)
	}
	reqMD, respMD := fd.Messages().ByName("Req"), fd.Messages().ByName("Resp")
	field := func(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
		return md.Fields().ByName(protoreflect.Name(name))
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, ss grpc.ServerStream) error {
		req := dynamicpb.NewMessage(reqMD)
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		resp := dynamicpb.NewMessage(respMD)
		resp.Set(field(respMD, "name"), req.Get(field(reqMD, "name")))
		if req.Has(field(reqMD, "n")) {
			resp.Set(field(respMD, "n"), req.Get(field(reqMD, "n")))
		}
		xs := resp.Mutable(field(respMD, "xs")).List()
		in := req.Get(field(reqMD, "xs")).List()
		for i := 0; i < in.Len(); i++ {
			xs.Append(in.Get(i))
		}
		if req.Has(field(reqMD, "grp")) {
			reqGrp := req.Get(field(reqMD, "grp")).Message()
			grp := dynamicpb.NewMessage(field(respMD, "grp").Message())
			grp.Set(field(grp.Descriptor(), "g"), reqGrp.Get(field(reqGrp.Descriptor(), "g")))
			resp.Set(field(respMD, "grp"), protoreflect.ValueOfMessage(grp))
		}
		return ss.SendMsg(resp)
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)
	if err := gw.AddProtoBytes("p2.proto", []byte(proto2Source), To(lis.Addr().String()), As("p2")); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	post := func(q string) string {
		body, _ := json.Marshal(map[string]any{"query": q})
		rr := httptest.NewRecorder()
		gw.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)))
		b, _ := io.ReadAll(rr.Body)
		return string(b)
	}
	for q, want := range map[string]string{
		`{ p2 { do(name: "x", xs: [1, 2], grp: {g: "gg"}) { name n kind xs grp { g } } } }`: `{"data":{"p2":{"do":{"name":"x","n":null,"kind":null,"xs":[1,2],"grp":{"g":"gg"}}}}}`,
		`{ p2 { do(name: "x", n: 3) { n grp { g } } } }`:                                    `{"data":{"p2":{"do":{"n":3,"grp":null}}}}`,
	} {
		if got := post(q); got != want {
			t.Errorf("%s\n got %s\nwant %s", q, got, want)
		}
	}
	if got := post(`{ p2 { do(n: 3) { name } } }`); !strings.Contains(got, `argument \"name\" of type \"String!\" is required`) {
		t.Errorf("required arg not enforced: %s", got)
	}
}
