package ir

import (
	"fmt"

	"google.golang.org/protobuf/types/descriptorpb"
)

// RenderProtoFiles emits a FileDescriptorSet representing svcs.
// Same-kind round-trip (KindProto Origin present) is byte-stable
// modulo source-code info — the renderer just hands the original
// FileDescriptorProto through. Cross-kind round-trip (Origin from
// OpenAPI / GraphQL or unset) synthesizes a FileDescriptorProto
// from the canonical fields, which is necessarily lossy:
//
//   - field numbers default to 1..N in declaration order; existing
//     ProtoNumber values are honored when set.
//   - OpenAPI parameter location / GraphQL directives drop on the
//     floor.
//   - JSON Schema constraints (pattern, min/max, format) drop —
//     proto has no native equivalent.
//   - Operation HTTP method / path drop unless rendered as a
//     google.api.http annotation (TODO; not implemented in v1).
//
// For svcs that mix origin kinds, each is rendered independently:
// proto-origin services emit one .proto file (the original);
// non-proto services emit one synthesized file each.
func RenderProtoFiles(svcs []*Service) (*descriptorpb.FileDescriptorSet, error) {
	fds := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{} // dedupe by file path

	for _, svc := range svcs {
		fp, err := renderProtoService(svc)
		if err != nil {
			return nil, fmt.Errorf("render %s/%s: %w", svc.Namespace, svc.Version, err)
		}
		if fp == nil {
			continue
		}
		path := fp.GetName()
		if seen[path] {
			continue
		}
		seen[path] = true
		fds.File = append(fds.File, fp)
	}
	return fds, nil
}

func renderProtoService(svc *Service) (*descriptorpb.FileDescriptorProto, error) {
	if svc.OriginKind == KindProto {
		// Same-kind shortcut: emit the captured source file.
		fp, ok := svc.Origin.(*descriptorpb.FileDescriptorProto)
		if ok && fp != nil {
			return fp, nil
		}
	}
	// Synthesize a FileDescriptorProto from the canonical fields.
	// Proto3 syntax; package name follows the gateway's namespace
	// model (`<namespace>.<version>`) so registries don't collide.
	pkg := svc.Namespace
	if svc.Version != "" {
		pkg = svc.Namespace + "." + svc.Version
	}
	fileName := fmt.Sprintf("%s/%s.proto", svc.Namespace, svc.Version)
	syntax := "proto3"
	fp := &descriptorpb.FileDescriptorProto{
		Name:    &fileName,
		Package: &pkg,
		Syntax:  &syntax,
	}

	// Emit messages + enums for every Type in the service. Ordering:
	// declaration order (we don't track it explicitly; iterate by
	// looking at Operations' references first then the rest).
	for _, t := range stableTypeOrder(svc) {
		switch t.TypeKind {
		case TypeObject, TypeInput:
			fp.MessageType = append(fp.MessageType, renderProtoMessage(t, pkg))
		case TypeEnum:
			fp.EnumType = append(fp.EnumType, renderProtoEnum(t))
		}
		// TypeUnion / TypeInterface / TypeScalar drop — proto3 has
		// no direct equivalent. (oneof is per-message, not a
		// top-level type.)
	}

	// Build one ServiceDescriptorProto with all operations. For
	// non-proto-origin operations (no Origin MethodDescriptor) we
	// synthesize <Op>Request / <Op>Response messages from Args /
	// Output and add them to the file's MessageType. proto3 needs
	// concrete input/output types — we don't import
	// google.protobuf.Empty just to point at it. Service.Groups
	// flattens via FlatOperations so nested namespaces from a
	// GraphQL ingest project to dotted-prefix proto method names.
	flatOps := svc.FlatOperations()
	if len(flatOps) > 0 {
		serviceName := svc.ServiceName
		if serviceName == "" {
			serviceName = "Service"
		}
		sp := &descriptorpb.ServiceDescriptorProto{
			Name: &serviceName,
		}
		for _, op := range flatOps {
			if op.OriginKind == KindProto {
				if mp, ok := op.Origin.(*descriptorpb.MethodDescriptorProto); ok && mp != nil {
					sp.Method = append(sp.Method, mp)
					continue
				}
			}
			reqName := sanitizeProtoIdentifier(op.Name) + "Request"
			respName := sanitizeProtoIdentifier(op.Name) + "Response"
			fp.MessageType = append(fp.MessageType, synthRequestMessage(reqName, op.Args, pkg))
			fp.MessageType = append(fp.MessageType, synthResponseMessage(respName, op.Output, op.OutputRepeated, pkg))
			fp.Service = append(fp.Service, &descriptorpb.ServiceDescriptorProto{Name: &serviceName, Method: []*descriptorpb.MethodDescriptorProto{}})
			lastSP := fp.Service[len(fp.Service)-1]
			fullReq := pkg + "." + reqName
			fullResp := pkg + "." + respName
			streamServer := op.Kind == OpSubscription
			streamClient := op.StreamingClient
			lastSP.Method = append(lastSP.Method, &descriptorpb.MethodDescriptorProto{
				Name:            stringPtr(op.Name),
				InputType:       stringPtr("." + fullReq),
				OutputType:      stringPtr("." + fullResp),
				ClientStreaming: &streamClient,
				ServerStreaming: &streamServer,
			})
		}
		// Collapse the per-op service-list entries into the canonical
		// single-Service shape: stash methods on sp[0] and drop the
		// rest. (We added one sp per op above so the above
		// append+lastSP shorthand worked without coordinating index;
		// merge here.)
		merged := sp
		for _, extra := range fp.Service {
			merged.Method = append(merged.Method, extra.Method...)
		}
		fp.Service = []*descriptorpb.ServiceDescriptorProto{merged}
	}
	return fp, nil
}

// synthRequestMessage builds a DescriptorProto whose fields mirror
// the operation's Args. Used when an Operation has no proto Origin
// (OpenAPI / GraphQL ingest); proto3 needs a concrete input message.
func synthRequestMessage(name string, args []*Arg, pkg string) *descriptorpb.DescriptorProto {
	mp := &descriptorpb.DescriptorProto{Name: &name}
	num := int32(0)
	for _, a := range args {
		if !isValidProtoIdentifier(a.Name) {
			continue
		}
		num++
		f := &Field{
			Name:     a.Name,
			Type:     a.Type,
			Repeated: a.Repeated,
			Required: a.Required,
		}
		mp.Field = append(mp.Field, renderProtoField(f, num, pkg))
	}
	return mp
}

// synthResponseMessage wraps a single Output TypeRef in a
// `<Op>Response` message with one field "value" — an artifact of
// the lossy cross-kind translation since proto methods always
// return a message, never a primitive.
func synthResponseMessage(name string, output *TypeRef, repeated bool, pkg string) *descriptorpb.DescriptorProto {
	mp := &descriptorpb.DescriptorProto{Name: &name}
	if output == nil {
		return mp
	}
	num := int32(1)
	f := &Field{
		Name:     "value",
		Type:     *output,
		Repeated: repeated,
	}
	mp.Field = append(mp.Field, renderProtoField(f, num, pkg))
	return mp
}

func renderProtoMessage(t *Type, pkg string) *descriptorpb.DescriptorProto {
	if t.OriginKind == KindProto {
		if mp, ok := t.Origin.(*descriptorpb.DescriptorProto); ok && mp != nil {
			return mp
		}
	}
	name := sanitizeProtoIdentifier(localName(t.Name))
	mp := &descriptorpb.DescriptorProto{Name: &name}
	num := int32(0)
	for _, f := range t.Fields {
		// Cross-kind sources can carry field names proto can't
		// express — JSON Schema's `$schema` meta-property, OpenAPI
		// extension keys with `x-`, etc. Skip them here rather
		// than fabricating a sanitized name; the lossy translation
		// is a stripped field, not a renamed one.
		if !isValidProtoIdentifier(f.Name) {
			continue
		}
		num++
		mp.Field = append(mp.Field, renderProtoField(f, num, pkg))
	}
	return mp
}

// isValidProtoIdentifier matches proto3's identifier rules:
// [a-zA-Z_][a-zA-Z0-9_]*. Used to skip JSON-Schema-flavored field
// names like `$schema` that came in via OpenAPI ingest.
func isValidProtoIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// sanitizeProtoIdentifier replaces non-identifier characters in s
// with underscores. Used for type names (which can't be skipped —
// dropping the type would orphan refs).
func sanitizeProtoIdentifier(s string) string {
	if isValidProtoIdentifier(s) {
		return s
	}
	out := make([]rune, 0, len(s))
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if i > 0 {
			ok = ok || (r >= '0' && r <= '9')
		}
		if ok {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 || (out[0] >= '0' && out[0] <= '9') {
		out = append([]rune{'_'}, out...)
	}
	return string(out)
}

func renderProtoEnum(t *Type) *descriptorpb.EnumDescriptorProto {
	if t.OriginKind == KindProto {
		if ep, ok := t.Origin.(*descriptorpb.EnumDescriptorProto); ok && ep != nil {
			return ep
		}
	}
	name := localName(t.Name)
	ep := &descriptorpb.EnumDescriptorProto{Name: &name}
	for _, ev := range t.Enum {
		num := ev.Number
		evp := &descriptorpb.EnumValueDescriptorProto{
			Name:   stringPtr(ev.Name),
			Number: &num,
		}
		ep.Value = append(ep.Value, evp)
	}
	return ep
}

func renderProtoField(f *Field, defaultNumber int32, pkg string) *descriptorpb.FieldDescriptorProto {
	num := f.ProtoNumber
	if num == 0 {
		num = defaultNumber
	}
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if f.Repeated || f.Type.IsMap() {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	out := &descriptorpb.FieldDescriptorProto{
		Name:   stringPtr(f.Name),
		Number: &num,
		Label:  &label,
	}
	if f.JSONName != "" && f.JSONName != f.Name {
		out.JsonName = stringPtr(f.JSONName)
	}
	switch {
	case f.Type.IsBuiltin():
		out.Type = scalarToProtoKind(f.Type.Builtin).Enum()
	case f.Type.IsNamed():
		// Synthesize a fully-qualified TypeName. For proto-origin
		// IR the Named field carries the FullName already
		// ("greeter.v1.HelloRequest" — no leading dot). For non-
		// proto origins (OpenAPI components key like "ChannelInfo")
		// we prepend the rendered package so the in-file lookup
		// resolves. We don't know whether Named refers to a
		// message or enum here; default to MESSAGE — protoc
		// resolves both via TypeName, and the IR's Type registry
		// already pre-renders enums under their qualified names so
		// cross-references stay consistent.
		qualified := f.Type.Named
		if pkg != "" && !looksFullyQualified(qualified, pkg) {
			qualified = pkg + "." + qualified
		}
		out.TypeName = stringPtr("." + qualified)
		out.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	case f.Type.IsMap():
		// Maps render as a synthesised entry message — not yet
		// implemented in the canonical render path. Same-kind
		// shortcut handles maps via Origin; cross-kind drops them
		// for v1.
	default:
		// Cross-kind shapes the proto can't represent (unconstrained
		// JSON, oneOf-without-discriminator, etc.) land here.
		// Default to string so the synthesized descriptor at least
		// resolves. Lossy by design — proto3 has no JSON type.
		out.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	}
	return out
}

// looksFullyQualified reports whether `name` already starts with a
// dot-component matching `pkg` (the destination file's package).
// Avoids double-prefixing when the IR's Named was set to
// "<pkg>.<Local>" already (proto-origin paths).
func looksFullyQualified(name, pkg string) bool {
	if pkg == "" {
		return false
	}
	if len(name) < len(pkg)+1 {
		return false
	}
	return name[:len(pkg)] == pkg && name[len(pkg)] == '.'
}

func renderProtoMethod(op *Operation, pkg string) *descriptorpb.MethodDescriptorProto {
	if op.OriginKind == KindProto {
		if mp, ok := op.Origin.(*descriptorpb.MethodDescriptorProto); ok && mp != nil {
			return mp
		}
	}
	// Synthesize input + output messages from Args/Output. v1: skip
	// — non-proto origins don't have a real proto type story until
	// the bench needs it.
	in := "google.protobuf.Empty"
	outName := "google.protobuf.Empty"
	streamServer := op.Kind == OpSubscription
	streamClient := op.StreamingClient
	mp := &descriptorpb.MethodDescriptorProto{
		Name:            stringPtr(op.Name),
		InputType:       stringPtr("." + in),
		OutputType:      stringPtr("." + outName),
		ClientStreaming: &streamClient,
		ServerStreaming: &streamServer,
	}
	return mp
}

// stableTypeOrder iterates types in a deterministic order (sorted
// by Name) so RenderProtoFiles output is reproducible regardless
// of map iteration.
func stableTypeOrder(svc *Service) []*Type {
	out := make([]*Type, 0, len(svc.Types))
	keys := make([]string, 0, len(svc.Types))
	for k := range svc.Types {
		keys = append(keys, k)
	}
	// Use sort.Strings via a tiny copy — avoids importing sort in
	// callers that don't need it.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	for _, k := range keys {
		out = append(out, svc.Types[k])
	}
	return out
}

// localName extracts the unqualified message / enum name from a
// dotted FullName. "greeter.v1.HelloRequest" → "HelloRequest".
func localName(fullName string) string {
	for i := len(fullName) - 1; i >= 0; i-- {
		if fullName[i] == '.' {
			return fullName[i+1:]
		}
	}
	return fullName
}

func stringPtr(s string) *string { return &s }

func scalarToProtoKind(s ScalarKind) descriptorpb.FieldDescriptorProto_Type {
	switch s {
	case ScalarBool:
		return descriptorpb.FieldDescriptorProto_TYPE_BOOL
	case ScalarString, ScalarTimestamp, ScalarID:
		return descriptorpb.FieldDescriptorProto_TYPE_STRING
	case ScalarBytes:
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES
	case ScalarInt32:
		return descriptorpb.FieldDescriptorProto_TYPE_INT32
	case ScalarUInt32:
		return descriptorpb.FieldDescriptorProto_TYPE_UINT32
	case ScalarInt64:
		return descriptorpb.FieldDescriptorProto_TYPE_INT64
	case ScalarUInt64:
		return descriptorpb.FieldDescriptorProto_TYPE_UINT64
	case ScalarFloat:
		return descriptorpb.FieldDescriptorProto_TYPE_FLOAT
	case ScalarDouble:
		return descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	}
	return descriptorpb.FieldDescriptorProto_TYPE_STRING
}
