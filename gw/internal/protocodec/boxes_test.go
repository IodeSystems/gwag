package protocodec

import (
	"encoding/json"
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// Box and unbox are inverses, through two levels of nesting and a
// list as a map value.
func TestListRowsRoundTrip(t *testing.T) {
	list2 := ir.TypeRef{List: &ir.ListType{Elem: ir.TypeRef{List: &ir.ListType{Elem: ir.TypeRef{Builtin: ir.ScalarInt32}}}}}
	svc := &ir.Service{
		Namespace: "t", Version: "v1", ServiceName: "Service",
		Types: map[string]*ir.Type{"Thing": {Name: "Thing", TypeKind: ir.TypeObject, Fields: []*ir.Field{
			{Name: "cube", Type: list2, Repeated: true, OneofIndex: -1},
			{Name: "byKey", OneofIndex: -1, Type: ir.TypeRef{Map: &ir.MapType{
				KeyType:   ir.TypeRef{Builtin: ir.ScalarString},
				ValueType: ir.TypeRef{List: &ir.ListType{Elem: ir.TypeRef{Builtin: ir.ScalarString}}},
			}}},
			{Name: "flat", Type: ir.TypeRef{Builtin: ir.ScalarInt32}, Repeated: true, OneofIndex: -1},
		}}},
	}
	fds, err := ir.RenderProtoFiles([]*ir.Service{svc})
	if err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	var md protoreflect.MessageDescriptor
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if m := f.Messages().ByName("Thing"); m != nil {
			md = m
		}
		return true
	})
	if md == nil || !HasBoxes(md) {
		t.Fatalf("Thing = %v, want list rows", md)
	}

	const plain = `{"cube":[[[1,2],[3]],[]],"byKey":{"a":["x","y"]},"flat":[1,2]}`
	const boxed = `{"cube":[{"value":[{"value":[1,2]},{"value":[3]}]},{"value":[]}],"byKey":{"a":{"value":["x","y"]}},"flat":[1,2]}`
	var v, want any
	_ = json.Unmarshal([]byte(plain), &v)
	_ = json.Unmarshal([]byte(boxed), &want)
	if got := Box(md, v); !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("box = %s\nwant  %s", b, boxed)
	}
	_ = json.Unmarshal([]byte(plain), &want)
	if got := Unbox(md, v); !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("unbox = %s\nwant    %s", b, plain)
	}
}

// Map of map (two levels) and map of map of list: each inner map is a
// `<Field>Value` box, each inner list a `<Field>Row` box.
func TestMapBoxesRoundTrip(t *testing.T) {
	str := ir.TypeRef{Builtin: ir.ScalarString}
	i32 := ir.TypeRef{Builtin: ir.ScalarInt32}
	mapOf := func(v ir.TypeRef) ir.TypeRef {
		return ir.TypeRef{Map: &ir.MapType{KeyType: str, ValueType: v}}
	}
	listOf := func(v ir.TypeRef) ir.TypeRef { return ir.TypeRef{List: &ir.ListType{Elem: v}} }
	svc := &ir.Service{
		Namespace: "t", Version: "v1", ServiceName: "Service",
		Types: map[string]*ir.Type{"Thing": {Name: "Thing", TypeKind: ir.TypeObject, Fields: []*ir.Field{
			{Name: "grid", Type: mapOf(mapOf(i32)), OneofIndex: -1},
			{Name: "deep", Type: mapOf(mapOf(listOf(str))), OneofIndex: -1},
			{Name: "cube", Type: mapOf(mapOf(mapOf(i32))), OneofIndex: -1},
		}}},
	}
	fds, err := ir.RenderProtoFiles([]*ir.Service{svc})
	if err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	var md protoreflect.MessageDescriptor
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if m := f.Messages().ByName("Thing"); m != nil {
			md = m
		}
		return true
	})
	if md == nil || !HasBoxes(md) {
		t.Fatalf("Thing = %v, want boxes", md)
	}
	const plain = `{"grid":{"a":{"k":1}},"deep":{"a":{"b":["x","y"]}},"cube":{"a":{"b":{"c":3}}}}`
	var v, want any
	_ = json.Unmarshal([]byte(plain), &v)
	boxedJSON := `{"grid":{"a":{"value":{"k":1}}},"deep":{"a":{"value":{"b":{"value":["x","y"]}}}},"cube":{"a":{"value":{"b":{"value":{"c":3}}}}}}`
	_ = json.Unmarshal([]byte(boxedJSON), &want)
	if got := Box(md, v); !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("box = %s\nwant  %s", b, boxedJSON)
	}
	_ = json.Unmarshal([]byte(plain), &want)
	if got := Unbox(md, v); !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("unbox = %s\nwant    %s", b, plain)
	}
}
