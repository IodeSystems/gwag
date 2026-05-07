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
			fp.MessageType = append(fp.MessageType, renderProtoMessage(t))
		case TypeEnum:
			fp.EnumType = append(fp.EnumType, renderProtoEnum(t))
		}
		// TypeUnion / TypeInterface / TypeScalar drop — proto3 has
		// no direct equivalent. (oneof is per-message, not a
		// top-level type.)
	}

	// Build one ServiceDescriptorProto with all operations.
	if len(svc.Operations) > 0 {
		serviceName := svc.ServiceName
		if serviceName == "" {
			serviceName = "Service"
		}
		sp := &descriptorpb.ServiceDescriptorProto{
			Name: &serviceName,
		}
		for _, op := range svc.Operations {
			mp := renderProtoMethod(op, pkg)
			sp.Method = append(sp.Method, mp)
		}
		fp.Service = append(fp.Service, sp)
	}
	return fp, nil
}

func renderProtoMessage(t *Type) *descriptorpb.DescriptorProto {
	if t.OriginKind == KindProto {
		if mp, ok := t.Origin.(*descriptorpb.DescriptorProto); ok && mp != nil {
			return mp
		}
	}
	name := localName(t.Name)
	mp := &descriptorpb.DescriptorProto{Name: &name}
	for i, f := range t.Fields {
		mp.Field = append(mp.Field, renderProtoField(f, int32(i+1)))
	}
	return mp
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

func renderProtoField(f *Field, defaultNumber int32) *descriptorpb.FieldDescriptorProto {
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
		// We don't track here whether Named refers to a message or
		// enum — the renderer must consult Service.Types. The
		// caller is expected to set TypeName so protoc resolves
		// downstream.
		out.TypeName = stringPtr("." + f.Type.Named)
		// Default to MESSAGE; fix below if it's actually an enum.
		out.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	case f.Type.IsMap():
		// Maps render as a synthesised entry message — not yet
		// implemented in the canonical render path. Same-kind
		// shortcut handles maps via Origin; cross-kind drops them
		// for v1.
	}
	return out
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
