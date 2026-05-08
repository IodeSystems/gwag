package gateway

import (
	"github.com/graphql-go/graphql"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// newProtoIRTypeBuilder builds a single *IRTypeBuilder shared across
// every proto pool. Proto FullNames are globally unique, so a merged
// Types map is collision-free; sharing the builder keeps cyclic
// references resolving to a single *graphql.Object across pools and
// across the Query / Subscription roots.
//
// Hides is applied to the merged service in-place so the type-builder
// emits Object/Input types without hidden fields. The args walk
// (which doesn't go through Service.Types) carries its own hide
// check via protoMessageHidden.
func newProtoIRTypeBuilder(pools map[poolKey]*pool, hides map[string]bool) *IRTypeBuilder {
	merged := &ir.Service{Types: map[string]*ir.Type{}}
	for _, p := range pools {
		svcs := ir.IngestProto(p.file)
		for _, s := range svcs {
			for k, v := range s.Types {
				if _, ok := merged.Types[k]; !ok {
					merged.Types[k] = v
				}
			}
		}
	}
	if len(hides) > 0 {
		ir.Hides([]*ir.Service{merged}, hides)
	}
	return NewIRTypeBuilder(merged, IRTypeNaming{
		ObjectName: exportedName,
		EnumName:   exportedName,
		UnionName:  exportedName,
		InputName:  func(s string) string { return exportedName(s) + "_Input" },
		FieldName:  lowerCamel,
	}, IRTypeBuilderOptions{})
}

// protoMessageHidden reports whether a proto field's message-typed
// target is in the hides set. Only message kinds participate — the
// args walk uses this to skip flattened sub-message slots whose
// type was hidden.
func protoMessageHidden(fd protoreflect.FieldDescriptor, hides map[string]bool) bool {
	if len(hides) == 0 {
		return false
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return false
	}
	return hides[string(fd.Message().FullName())]
}

// protoArgsFromMessage flattens a proto input message's fields into
// graphql arguments. Mirrors the legacy typeBuilder.argsFromMessage
// behaviour: each field's type resolves through the IRTypeBuilder
// (so Object/Input/Enum/scalar caching is shared with output
// construction); list-cardinality wraps in graphql.NewList; nothing
// is marked NonNull (proto args are always nullable on the gateway
// surface).
func protoArgsFromMessage(b *IRTypeBuilder, md protoreflect.MessageDescriptor, hides map[string]bool) (graphql.FieldConfigArgument, error) {
	out := graphql.FieldConfigArgument{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if protoMessageHidden(fd, hides) {
			continue
		}
		ref := protoFieldTypeRef(fd)
		t, err := b.Input(ref, fd.IsList(), false, false)
		if err != nil {
			return nil, err
		}
		out[lowerCamel(string(fd.Name()))] = &graphql.ArgumentConfig{Type: t}
	}
	return out, nil
}

// protoOutputObject resolves a proto output message to its graphql
// Object via the shared IRTypeBuilder. The Output return type is the
// (possibly thunk-wrapped) Object — never NonNull-wrapped for the
// proto path, matching legacy behaviour.
func protoOutputObject(b *IRTypeBuilder, md protoreflect.MessageDescriptor) (graphql.Output, error) {
	return b.Output(ir.TypeRef{Named: string(md.FullName())}, false, false, false)
}

// protoFieldTypeRef converts a single proto FieldDescriptor's type
// into an ir.TypeRef. Map and list cardinality are handled by the
// caller (b.Input takes a `repeated` flag); this function only looks
// at the leaf type.
func protoFieldTypeRef(fd protoreflect.FieldDescriptor) ir.TypeRef {
	if fd.IsMap() {
		return ir.TypeRef{Map: &ir.MapType{
			KeyType:   protoFieldTypeRef(fd.MapKey()),
			ValueType: protoFieldTypeRef(fd.MapValue()),
		}}
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return ir.TypeRef{Builtin: ir.ScalarBool}
	case protoreflect.StringKind:
		return ir.TypeRef{Builtin: ir.ScalarString}
	case protoreflect.BytesKind:
		return ir.TypeRef{Builtin: ir.ScalarBytes}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return ir.TypeRef{Builtin: ir.ScalarInt32}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return ir.TypeRef{Builtin: ir.ScalarUInt32}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return ir.TypeRef{Builtin: ir.ScalarInt64}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return ir.TypeRef{Builtin: ir.ScalarUInt64}
	case protoreflect.FloatKind:
		return ir.TypeRef{Builtin: ir.ScalarFloat}
	case protoreflect.DoubleKind:
		return ir.TypeRef{Builtin: ir.ScalarDouble}
	case protoreflect.EnumKind:
		return ir.TypeRef{Named: string(fd.Enum().FullName())}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return ir.TypeRef{Named: string(fd.Message().FullName())}
	}
	return ir.TypeRef{Builtin: ir.ScalarString}
}
