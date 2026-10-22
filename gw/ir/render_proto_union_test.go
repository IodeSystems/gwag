package ir

import (
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A TypeUnion renders as a message with one oneof, so an output or
// field naming the union resolves. Before, the descriptor set failed
// to build ("demo.v1.AB" not found).
func TestRenderProto_UnionAsOneof(t *testing.T) {
	svc := &Service{Namespace: "demo", Version: "v1", OriginKind: KindOpenAPI, Types: map[string]*Type{
		"SessionEvent": {Name: "SessionEvent", TypeKind: TypeObject, Fields: []*Field{{Name: "seq", Type: TypeRef{Builtin: ScalarInt64}}}},
		"HTTPInfo":     {Name: "HTTPInfo", TypeKind: TypeObject, Fields: []*Field{{Name: "code", Type: TypeRef{Builtin: ScalarInt32}}}},
		"AB":           {Name: "AB", TypeKind: TypeUnion, Variants: []string{"SessionEvent", "HTTPInfo"}},
	}, Operations: []*Operation{{Name: "get", Kind: OpQuery, Output: &TypeRef{Named: "AB"}}}}
	PopulateSchemaIDs(svc)
	fds, err := RenderProtoFiles([]*Service{svc})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	d, err := files.FindDescriptorByName("demo.v1.AB")
	if err != nil {
		t.Fatalf("union message missing: %v", err)
	}
	md := d.(protoreflect.MessageDescriptor)
	if md.Oneofs().Len() != 1 || md.Oneofs().Get(0).Name() != "value" {
		t.Fatalf("want one oneof named value, got %d", md.Oneofs().Len())
	}
	for name, want := range map[protoreflect.Name]protoreflect.FullName{
		"session_event": "demo.v1.SessionEvent",
		"http_info":     "demo.v1.HTTPInfo",
	} {
		f := md.Fields().ByName(name)
		if f == nil || f.ContainingOneof() == nil || f.Message().FullName() != want {
			t.Errorf("field %s: want oneof member of %s, got %v", name, want, f)
		}
	}
}
