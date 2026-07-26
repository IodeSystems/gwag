package ir

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/descriptorpb"
)

// proto3 has no standalone map type: `map<K,V> foo` is sugar for a nested
// `FooEntry` message marked map_entry plus `repeated FooEntry foo`. Before this
// was rendered, a map field emitted no type at all and protocompile failed with
// `invalid name reference: ""` — a hard startup failure rather than the lossy
// degradation every other unrepresentable shape gets.
func mustEntry(e *descriptorpb.DescriptorProto, _ []*descriptorpb.DescriptorProto) *descriptorpb.DescriptorProto {
	return e
}

func mapField(name string, val TypeRef) *Field {
	return &Field{Name: name, Type: TypeRef{Map: &MapType{
		KeyType: TypeRef{Builtin: ScalarString}, ValueType: val}}}
}

func TestRenderProtoFieldEmitsMapEntry(t *testing.T) {
	f := mapField("current", TypeRef{Builtin: ScalarString})
	fd, nested := renderProtoField(f, 3, "kgraph.v1", "RenderResponse")
	var entry *descriptorpb.DescriptorProto
	if len(nested) > 0 {
		entry = nested[0]
	}

	if entry == nil {
		t.Fatal("a map field must produce a nested entry message")
	}
	if fd.GetType() != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		t.Fatalf("map field must be a message, got %v", fd.GetType())
	}
	if fd.GetLabel() != descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		t.Fatalf("map field must be repeated, got %v", fd.GetLabel())
	}
	if got, want := fd.GetTypeName(), ".kgraph.v1.RenderResponse.CurrentEntry"; got != want {
		t.Fatalf("type name: got %q want %q", got, want)
	}
	if !entry.GetOptions().GetMapEntry() {
		t.Fatal("entry message must set map_entry")
	}
	if len(entry.Field) != 2 {
		t.Fatalf("entry needs key and value, got %d fields", len(entry.Field))
	}
	k, v := entry.Field[0], entry.Field[1]
	if k.GetName() != "key" || k.GetNumber() != 1 || k.GetType() != descriptorpb.FieldDescriptorProto_TYPE_STRING {
		t.Fatalf("key field wrong: %v", k)
	}
	if v.GetName() != "value" || v.GetNumber() != 2 || v.GetType() != descriptorpb.FieldDescriptorProto_TYPE_STRING {
		t.Fatalf("value field wrong: %v", v)
	}
}

// The regression that motivated this: no arm of renderProtoField may return a
// field with no type. That is what protocompile rejects.
func TestRenderProtoFieldNeverEmitsUntypedField(t *testing.T) {
	cases := map[string]*Field{
		"map of string":  mapField("m", TypeRef{Builtin: ScalarString}),
		"map of message": mapField("m", TypeRef{Named: "Thing"}),
		"map of map": {Name: "m", Type: TypeRef{Map: &MapType{
			KeyType: TypeRef{Builtin: ScalarString},
			ValueType: TypeRef{Map: &MapType{
				KeyType: TypeRef{Builtin: ScalarString}, ValueType: TypeRef{Builtin: ScalarString}}}}}},
		"builtin": {Name: "s", Type: TypeRef{Builtin: ScalarString}},
		"named":   {Name: "n", Type: TypeRef{Named: "Thing"}},
		"unknown": {Name: "u", Type: TypeRef{}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			fd, _ := renderProtoField(f, 1, "pkg.v1", "Parent")
			if fd.Type == nil {
				t.Fatalf("field %q has no type — protocompile rejects this as an invalid name reference", f.Name)
			}
			if fd.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE && fd.GetTypeName() == "" {
				t.Fatalf("field %q is a message with no type name", f.Name)
			}
		})
	}
}

func TestRenderProtoMapEntryValueKinds(t *testing.T) {
	named := mustEntry(renderProtoMapEntry(mapField("m", TypeRef{Named: "Thing"}), "pkg.v1", "P"))
	v := named.Field[1]
	if v.GetType() != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		t.Fatalf("message value: got %v", v.GetType())
	}
	if got, want := v.GetTypeName(), ".pkg.v1.Thing"; got != want {
		t.Fatalf("message value type name: got %q want %q", got, want)
	}
	// Already-qualified names must not be double-prefixed.
	q := mustEntry(renderProtoMapEntry(mapField("m", TypeRef{Named: "pkg.v1.Thing"}), "pkg.v1", "P"))
	if got, want := q.Field[1].GetTypeName(), ".pkg.v1.Thing"; got != want {
		t.Fatalf("qualified value type name: got %q want %q", got, want)
	}
	num := mustEntry(renderProtoMapEntry(mapField("m", TypeRef{Builtin: ScalarInt32}), "pkg.v1", "P"))
	if num.Field[1].GetType() != scalarToProtoKind(ScalarInt32) {
		t.Fatalf("int value: got %v", num.Field[1].GetType())
	}
}

// proto restricts map keys to integral and string types.
func TestRenderProtoMapEntryKeyFallsBackToString(t *testing.T) {
	f := &Field{Name: "m", Type: TypeRef{Map: &MapType{
		KeyType:   TypeRef{Builtin: ScalarFloat},
		ValueType: TypeRef{Builtin: ScalarString}}}}
	if k := mustEntry(renderProtoMapEntry(f, "pkg.v1", "P")).Field[0]; k.GetType() != descriptorpb.FieldDescriptorProto_TYPE_STRING {
		t.Fatalf("a non-integral key must degrade to string, got %v", k.GetType())
	}
	f.Type.Map.KeyType = TypeRef{Builtin: ScalarInt32}
	if k := mustEntry(renderProtoMapEntry(f, "pkg.v1", "P")).Field[0]; k.GetType() != scalarToProtoKind(ScalarInt32) {
		t.Fatalf("an integral key must be preserved, got %v", k.GetType())
	}
}

func TestProtoMapEntryName(t *testing.T) {
	for in, want := range map[string]string{
		"current":     "CurrentEntry",
		"output_text": "OutputTextEntry",
		"Current":     "CurrentEntry",
		"a":           "AEntry",
	} {
		if got := protoMapEntryName(in); got != want {
			t.Errorf("protoMapEntryName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The entry must be nested inside the message that owns the field, or the
// qualified type name does not resolve.
func TestMessageNestsItsMapEntries(t *testing.T) {
	ty := &Type{Name: "RenderResponse", Fields: []*Field{
		{Name: "prompt", Type: TypeRef{Builtin: ScalarString}},
		mapField("current", TypeRef{Builtin: ScalarString}),
	}}
	mp := renderProtoMessage(ty, "kgraph.v1")
	if len(mp.NestedType) != 1 || mp.NestedType[0].GetName() != "CurrentEntry" {
		t.Fatalf("entry not nested in the parent: %v", mp.NestedType)
	}
	var mapFd *descriptorpb.FieldDescriptorProto
	for _, f := range mp.Field {
		if f.GetName() == "current" {
			mapFd = f
		}
	}
	if mapFd == nil {
		t.Fatal("map field missing from the message")
	}
	if !strings.HasSuffix(mapFd.GetTypeName(), ".RenderResponse.CurrentEntry") {
		t.Fatalf("field must point at the nested entry, got %q", mapFd.GetTypeName())
	}
}

// proto3 cannot hold a map as a map value, so nesting is expressed by boxing
// each inner map in a wrapper message. This must work to arbitrary depth.
func TestArbitrarilyNestedMaps(t *testing.T) {
	str := TypeRef{Builtin: ScalarString}
	nest := func(depth int) TypeRef {
		ref := str
		for range depth {
			ref = TypeRef{Map: &MapType{KeyType: str, ValueType: ref}}
		}
		return ref
	}
	for _, depth := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			ty := &Type{Name: "Root", Fields: []*Field{{Name: "deep", Type: nest(depth)}}}
			mp := renderProtoMessage(ty, "pkg.v1")
			assertResolvable(t, mp, "pkg.v1", map[string]bool{})

			// One entry per level, and one wrapper per level below the top.
			entries, wrappers := countKinds(mp)
			if entries != depth {
				t.Errorf("depth %d: want %d map entries, got %d", depth, depth, entries)
			}
			if want := depth - 1; wrappers != want {
				t.Errorf("depth %d: want %d wrappers, got %d", depth, want, wrappers)
			}
		})
	}
}

func TestNestedMapOfMessages(t *testing.T) {
	str := TypeRef{Builtin: ScalarString}
	inner := TypeRef{Map: &MapType{KeyType: str, ValueType: TypeRef{Named: "Thing"}}}
	ty := &Type{Name: "Root", Fields: []*Field{
		{Name: "by_group", Type: TypeRef{Map: &MapType{KeyType: str, ValueType: inner}}}}}
	mp := renderProtoMessage(ty, "pkg.v1")
	assertResolvable(t, mp, "pkg.v1", map[string]bool{"pkg.v1.Thing": true})
}

// countKinds counts map_entry messages and wrapper messages, recursively.
func countKinds(mp *descriptorpb.DescriptorProto) (entries, wrappers int) {
	for _, n := range mp.NestedType {
		if n.GetOptions().GetMapEntry() {
			entries++
		} else {
			wrappers++
		}
		e, w := countKinds(n)
		entries += e
		wrappers += w
	}
	return
}

// assertResolvable walks every field and checks that each message-typed field
// names something that actually exists — the class of defect protocompile
// reports as `invalid name reference`.
func assertResolvable(t *testing.T, mp *descriptorpb.DescriptorProto, scope string, external map[string]bool) {
	t.Helper()
	declared := map[string]bool{}
	var collect func(m *descriptorpb.DescriptorProto, prefix string)
	collect = func(m *descriptorpb.DescriptorProto, prefix string) {
		full := prefix + "." + m.GetName()
		declared[full] = true
		for _, n := range m.NestedType {
			collect(n, full)
		}
	}
	collect(mp, scope)

	var check func(m *descriptorpb.DescriptorProto)
	check = func(m *descriptorpb.DescriptorProto) {
		for _, f := range m.Field {
			if f.Type == nil {
				t.Fatalf("field %q has no type", f.GetName())
			}
			if f.GetType() != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
				continue
			}
			ref := strings.TrimPrefix(f.GetTypeName(), ".")
			if ref == "" {
				t.Fatalf("field %q is a message with an empty type name", f.GetName())
			}
			if !declared[ref] && !external[ref] {
				t.Fatalf("field %q references %q, which is not declared (declared: %v)",
					f.GetName(), ref, keysOf(declared))
			}
		}
		for _, n := range m.NestedType {
			check(n)
		}
	}
	check(mp)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
