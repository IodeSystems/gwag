package gateway

import (
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The proto form of projectionOpenAPISpec, plus a oneof and a map.
const projectionProtoSource = `syntax = "proto3";
package proj.v1;
message ThingRequest { string id = 1; }
message Owner { string id = 1; string name = 2; }
message Tag { string k = 1; string v = 2; }
message Thing {
  string id = 1;
  string name = 2;
  Owner owner = 3;
  repeated Tag tags = 4;
  oneof choice {
    string word = 5;
    int32 num = 6;
  }
  map<string, string> labels = 7;
}
service Things { rpc Thing(ThingRequest) returns (.proj.v1.Thing); }
`

// projectionProtoCases run against the proto upstream only: the oneof
// and the map have no counterpart in the OpenAPI and GraphQL fixtures.
var projectionProtoCases = []projectionCase{
	{name: "oneof_set", query: `{ %NS% { thing(id:"1") { word } } }`, want: `{"word":"w"}`},
	{name: "oneof_unset", query: `{ %NS% { thing(id:"1") { num word } } }`, want: `{"num":null,"word":"w"}`},
	{name: "map", query: `{ %NS% { thing(id:"1") { labels } } }`, want: `{"labels":{"a":"1","b":"2"}}`},
	{name: "map_in_fragment", query: `query { %NS% { thing(id:"1") { ...F labels } } } fragment F on %NS%_Thing { l: labels word }`, want: `{"l":{"a":"1","b":"2"},"word":"w","labels":{"a":"1","b":"2"}}`},
	{name: "list_in_fragment", query: `query { %NS% { thing(id:"1") { tags { ...T k } } } } fragment T on %NS%_Tag { v k }`, want: `{"tags":[{"v":"1","k":"a"},{"v":"2","k":"b"}]}`},
	{name: "list_merged", query: `{ %NS% { thing(id:"1") { tags { k } tags { v __typename } } } }`, want: `{"tags":[{"k":"a","v":"1","__typename":"%NS%_Tag"},{"k":"b","v":"2","__typename":"%NS%_Tag"}]}`},
	{name: "typename_skipped", query: `{ %NS% { thing(id:"1") { id __typename @skip(if: true) } } }`, want: `{"id":"1"}`},
	{name: "include_variable", query: `query($i: Boolean!) { %NS% { thing(id:"1") { id name @include(if: $i) } } }`, vars: map[string]any{"i": false}, want: `{"id":"1"}`},
	{name: "skip_fragment_spread", query: `query($s: Boolean!) { %NS% { thing(id:"1") { id ...F @skip(if: $s) ... @include(if: false) { word } } } } fragment F on %NS%_Thing { name }`, vars: map[string]any{"s": true}, want: `{"id":"1"}`},
	{name: "duplicate_scalar", query: `{ %NS% { thing(id:"1") { id id name id } } }`, want: `{"id":"1","name":"thing"}`},
}

func startProjectionProtoUpstream(t testing.TB) string {
	t.Helper()
	fd, err := compileProtoBytes("proj.proto", []byte(projectionProtoSource), nil)
	if err != nil {
		t.Fatal(err)
	}
	reqMD, thingMD := fd.Messages().ByName("ThingRequest"), fd.Messages().ByName("Thing")
	ownerMD, tagMD := fd.Messages().ByName("Owner"), fd.Messages().ByName("Tag")
	set := func(m *dynamicpb.Message, name string, v protoreflect.Value) {
		m.Set(m.Descriptor().Fields().ByName(protoreflect.Name(name)), v)
	}
	str := protoreflect.ValueOfString
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, ss grpc.ServerStream) error {
		req := dynamicpb.NewMessage(reqMD)
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		thing := dynamicpb.NewMessage(thingMD)
		set(thing, "id", req.Get(reqMD.Fields().ByName("id")))
		set(thing, "name", str("thing"))
		owner := dynamicpb.NewMessage(ownerMD)
		set(owner, "id", str("o1"))
		set(owner, "name", str("olga"))
		set(thing, "owner", protoreflect.ValueOfMessage(owner))
		tags := thing.Mutable(thingMD.Fields().ByName("tags")).List()
		for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}} {
			tag := dynamicpb.NewMessage(tagMD)
			set(tag, "k", str(kv[0]))
			set(tag, "v", str(kv[1]))
			tags.Append(protoreflect.ValueOfMessage(tag))
		}
		set(thing, "word", str("w"))
		labels := thing.Mutable(thingMD.Fields().ByName("labels")).Map()
		labels.Set(protoreflect.ValueOfString("b").MapKey(), str("2"))
		labels.Set(protoreflect.ValueOfString("a").MapKey(), str("1"))
		return ss.SendMsg(thing)
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestSelectionProjection_Proto(t *testing.T) {
	addr := startProjectionProtoUpstream(t)
	for name, gw := range projectionGateways(t, func(t *testing.T, gw *Gateway) {
		if err := gw.AddProtoBytes("proj.proto", []byte(projectionProtoSource), To(addr), As("proj")); err != nil {
			t.Fatal(err)
		}
	}) {
		t.Run(name, func(t *testing.T) {
			runProjectionCases(t, gw, "proj", "Proj_V1_", true, projectionCases)
			runProjectionCases(t, gw, "proj", "Proj_V1_", true, projectionProtoCases)
		})
	}
}
