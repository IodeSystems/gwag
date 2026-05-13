package ir

import (
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// IngestProto walks `fd` and returns one Service per service
// declared in the file. Each returned Service.Origin is the
// FileDescriptorProto for fd, so a same-kind render can reproduce
// the source FDS verbatim. Cross-kind renderers walk the canonical
// fields instead.
//
// The (namespace, version) coordinate isn't carried inside a proto
// file — the gateway derives it from the registration's pool key.
// Caller fills Service.Namespace + Service.Version on the returned
// values.
//
// Stability: stable
func IngestProto(fd protoreflect.FileDescriptor) []*Service {
	fileProto := protodesc.ToFileDescriptorProto(fd)

	// Build the Type registry. Per-file messages first, then walk
	// imports transitively so a service that returns a message from
	// an imported file still has the message's IR Type entry
	// available — necessary for the IR-driven type-builder wiring
	// to resolve cross-file refs without falling back to the
	// descriptor graph.
	types := map[string]*Type{}
	walkMessages(fd.Messages(), types)
	walkEnums(fd.Enums(), types)
	walkImports(fd, types, map[string]bool{string(fd.Path()): true})

	out := []*Service{}
	services := fd.Services()
	for i := 0; i < services.Len(); i++ {
		sd := services.Get(i)
		svc := &Service{
			Description: stringFromComments(sd),
			ServiceName: string(sd.Name()),
			Operations:  []*Operation{},
			Types:       types,
			OriginKind:  KindProto,
			Origin:      fileProto,
		}
		methods := sd.Methods()
		for j := 0; j < methods.Len(); j++ {
			md := methods.Get(j)
			svc.Operations = append(svc.Operations, ingestProtoMethod(md, fileProto))
		}
		out = append(out, svc)
	}
	return out
}

func ingestProtoMethod(md protoreflect.MethodDescriptor, fileProto *descriptorpb.FileDescriptorProto) *Operation {
	// Find the matching MethodDescriptorProto so we can stash it as
	// Origin. Linear-scan is fine — services rarely have hundreds
	// of methods.
	var methodProto *descriptorpb.MethodDescriptorProto
	for _, sp := range fileProto.GetService() {
		if sp.GetName() != string(md.Parent().(protoreflect.ServiceDescriptor).Name()) {
			continue
		}
		for _, mp := range sp.GetMethod() {
			if mp.GetName() == string(md.Name()) {
				methodProto = mp
				break
			}
		}
	}

	op := &Operation{
		Name:            string(md.Name()),
		Description:     stringFromComments(md),
		StreamingClient: md.IsStreamingClient(),
		OriginKind:      KindProto,
		Origin:          methodProto,
		Output:          messageRef(md.Output()),
	}
	if md.IsStreamingServer() {
		op.Kind = OpSubscription
	} else {
		// Proto unary RPCs are reads-or-writes ambiguously; default
		// to OpQuery and let mutating semantics carry through the
		// proto Origin. Cross-kind renders treat all non-streaming
		// proto methods as mutations under OpenAPI (POST) and
		// queries under GraphQL — operators can override per-method
		// in a future pass.
		op.Kind = OpQuery
	}

	// Distill the input message's fields into Args. Lets cross-
	// kind renderers expose them as GraphQL field args / OpenAPI
	// query params without round-tripping the message itself.
	in := md.Input()
	infields := in.Fields()
	for i := 0; i < infields.Len(); i++ {
		f := infields.Get(i)
		op.Args = append(op.Args, &Arg{
			Name:        string(f.Name()),
			Type:        fieldTypeRef(f),
			Required:    !f.HasOptionalKeyword() && f.Cardinality() == protoreflect.Required,
			Description: stringFromComments(f),
		})
	}
	return op
}

// walkImports recurses through fd.Imports(), accumulating Messages
// and Enums from each imported file's top-level into dst. visited
// is keyed by file Path() so dependency diamonds don't cycle. Same
// well-known imports (e.g. google/protobuf/descriptor.proto)
// register their Messages too — harmless, since the type-builder
// only materialises types that are actually referenced.
func walkImports(fd protoreflect.FileDescriptor, dst map[string]*Type, visited map[string]bool) {
	imports := fd.Imports()
	for i := 0; i < imports.Len(); i++ {
		imp := imports.Get(i)
		path := string(imp.Path())
		if visited[path] {
			continue
		}
		visited[path] = true
		walkMessages(imp.FileDescriptor.Messages(), dst)
		walkEnums(imp.FileDescriptor.Enums(), dst)
		walkImports(imp.FileDescriptor, dst, visited)
	}
}

func walkMessages(ms protoreflect.MessageDescriptors, dst map[string]*Type) {
	for i := 0; i < ms.Len(); i++ {
		md := ms.Get(i)
		full := string(md.FullName())
		if _, ok := dst[full]; ok {
			continue
		}
		t := &Type{
			Name:        full,
			TypeKind:    TypeObject,
			Description: stringFromComments(md),
			OriginKind:  KindProto,
		}
		// Pre-register so recursive references resolve.
		dst[full] = t

		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			f := fields.Get(j)
			t.Fields = append(t.Fields, ingestProtoField(f))
		}
		// Nested messages + enums are addressable from outside the
		// containing message (proto FullName carries the parent),
		// so register them at the top level too.
		walkMessages(md.Messages(), dst)
		walkEnums(md.Enums(), dst)
	}
}

func walkEnums(es protoreflect.EnumDescriptors, dst map[string]*Type) {
	for i := 0; i < es.Len(); i++ {
		ed := es.Get(i)
		full := string(ed.FullName())
		if _, ok := dst[full]; ok {
			continue
		}
		t := &Type{
			Name:        full,
			TypeKind:    TypeEnum,
			Description: stringFromComments(ed),
			OriginKind:  KindProto,
		}
		values := ed.Values()
		for j := 0; j < values.Len(); j++ {
			v := values.Get(j)
			t.Enum = append(t.Enum, EnumValue{
				Name:        string(v.Name()),
				Number:      int32(v.Number()),
				Description: stringFromComments(v),
			})
		}
		dst[full] = t
	}
}

func ingestProtoField(f protoreflect.FieldDescriptor) *Field {
	out := &Field{
		Name:        string(f.Name()),
		JSONName:    f.JSONName(),
		Description: stringFromComments(f),
		ProtoNumber: int32(f.Number()),
		OneofIndex:  -1,
		Optional:    f.HasOptionalKeyword(),
	}
	if oneof := f.ContainingOneof(); oneof != nil && !f.HasOptionalKeyword() {
		out.OneofIndex = int32(oneof.Index())
	}
	if f.IsMap() {
		out.Type = TypeRef{Map: &MapType{
			KeyType:   fieldTypeRef(f.MapKey()),
			ValueType: fieldTypeRef(f.MapValue()),
		}}
		return out
	}
	if f.IsList() {
		out.Repeated = true
	}
	out.Type = fieldTypeRef(f)
	return out
}

// fieldTypeRef returns a TypeRef for a single proto field's type
// (ignoring repeated / map wrapping — callers handle those).
func fieldTypeRef(f protoreflect.FieldDescriptor) TypeRef {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return TypeRef{Named: string(f.Message().FullName())}
	case protoreflect.EnumKind:
		return TypeRef{Named: string(f.Enum().FullName())}
	case protoreflect.BoolKind:
		return TypeRef{Builtin: ScalarBool}
	case protoreflect.StringKind:
		return TypeRef{Builtin: ScalarString}
	case protoreflect.BytesKind:
		return TypeRef{Builtin: ScalarBytes}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return TypeRef{Builtin: ScalarInt32}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return TypeRef{Builtin: ScalarUInt32}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return TypeRef{Builtin: ScalarInt64}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return TypeRef{Builtin: ScalarUInt64}
	case protoreflect.FloatKind:
		return TypeRef{Builtin: ScalarFloat}
	case protoreflect.DoubleKind:
		return TypeRef{Builtin: ScalarDouble}
	}
	return TypeRef{Builtin: ScalarString}
}

func messageRef(md protoreflect.MessageDescriptor) *TypeRef {
	r := TypeRef{Named: string(md.FullName())}
	return &r
}

// stringFromComments pulls leading comments off a descriptor.
// Empty when the FileDescriptor has no source info — protoc-gen-go's
// embedded raw descriptor strips SourceCodeInfo unconditionally, so
// AddProtoDescriptor / SelfRegister flows always return "" here.
// Path-based ingest via gw.loadProto opts into source info, so
// AddProto(path) carries comments through. Whitespace cleanup
// (leading space + trailing newline that protoc emits) lands on the
// caller — most renderers tolerate it.
func stringFromComments(d protoreflect.Descriptor) string {
	if d == nil {
		return ""
	}
	loc := d.ParentFile().SourceLocations().ByDescriptor(d)
	return loc.LeadingComments
}
