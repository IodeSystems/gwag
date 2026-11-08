package schemaregistry

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Problem is one difference between a shadow message and the
// registry's. Path is the field path from the shadow message, e.g.
// "items.sku".
//
// Stability: experimental
type Problem struct {
	Path string
	Text string
	// Warning problems do not fail a check (a renamed field: the wire
	// does not carry names).
	Warning bool
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Text
	}
	return p.Path + ": " + p.Text
}

// CompareConsumer checks a consumer-side shadow: shadow may leave
// fields of reg out, but every field it keeps must match reg's field of
// the same number in type, cardinality and (for enums) values, and a
// field number reg does not have is an error. Nested messages and
// enums are compared the same way, by structure, so the shadow may
// live in another package.
//
// Stability: experimental
func CompareConsumer(shadow, reg protoreflect.MessageDescriptor) []Problem {
	c := &comparer{seen: map[[2]protoreflect.FullName]bool{}}
	c.message("", shadow, reg)
	return c.out
}

type comparer struct {
	out  []Problem
	seen map[[2]protoreflect.FullName]bool
}

func (c *comparer) add(path string, warn bool, format string, args ...any) {
	c.out = append(c.out, Problem{Path: path, Text: fmt.Sprintf(format, args...), Warning: warn})
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func kindName(f protoreflect.FieldDescriptor) string {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if f.IsMap() {
			return fmt.Sprintf("map<%s, %s>", kindName(f.MapKey()), kindName(f.MapValue()))
		}
		return "message " + string(f.Message().FullName())
	case protoreflect.EnumKind:
		return "enum " + string(f.Enum().FullName())
	}
	return f.Kind().String()
}

func cardinality(f protoreflect.FieldDescriptor) string {
	switch {
	case f.IsMap():
		return "map"
	case f.IsList():
		return "repeated"
	}
	return "singular"
}

func wellKnown(n protoreflect.FullName) bool {
	return strings.HasPrefix(string(n), "google.protobuf.")
}

func (c *comparer) message(path string, shadow, reg protoreflect.MessageDescriptor) {
	k := [2]protoreflect.FullName{shadow.FullName(), reg.FullName()}
	if c.seen[k] {
		return
	}
	c.seen[k] = true
	fs := shadow.Fields()
	for i := 0; i < fs.Len(); i++ {
		f := fs.Get(i)
		p := join(path, string(f.Name()))
		rf := reg.Fields().ByNumber(f.Number())
		if rf == nil {
			c.add(p, false, "field %d is not in registry message %s", f.Number(), reg.FullName())
			continue
		}
		if f.Name() != rf.Name() {
			c.add(p, true, "registry names field %d %q", f.Number(), rf.Name())
		}
		if cs, cr := cardinality(f), cardinality(rf); cs != cr {
			c.add(p, false, "field %d is %s, registry has %s", f.Number(), cs, cr)
			continue
		}
		c.field(p, f, rf)
	}
}

func (c *comparer) field(p string, f, rf protoreflect.FieldDescriptor) {
	if f.IsMap() {
		c.field(p+"<key>", f.MapKey(), rf.MapKey())
		c.field(p+"<value>", f.MapValue(), rf.MapValue())
		return
	}
	if f.Kind() != rf.Kind() {
		c.add(p, false, "field %d is %s, registry has %s", f.Number(), kindName(f), kindName(rf))
		return
	}
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		sm, rm := f.Message(), rf.Message()
		if wellKnown(sm.FullName()) || wellKnown(rm.FullName()) {
			if sm.FullName() != rm.FullName() {
				c.add(p, false, "field %d is %s, registry has %s", f.Number(), sm.FullName(), rm.FullName())
			}
			return
		}
		c.message(p, sm, rm)
	case protoreflect.EnumKind:
		se, re := f.Enum(), rf.Enum()
		vs := se.Values()
		for i := 0; i < vs.Len(); i++ {
			v := vs.Get(i)
			rv := re.Values().ByNumber(v.Number())
			switch {
			case rv == nil:
				c.add(p, false, "enum value %s = %d is not in registry enum %s", v.Name(), v.Number(), re.FullName())
			case rv.Name() != v.Name():
				c.add(p, false, "enum value %d is %s, registry has %s", v.Number(), v.Name(), rv.Name())
			}
		}
	}
}

// ProducerCandidate returns reg's file with message target replaced by
// shadow, renamed to target's name and moved into reg's package, for
// the registry's compatibility API. A field the registry message has
// keeps the registry's message or enum type: the registry compares the
// shadow's fields and nested declarations, not the shadow's copies of
// other messages. Other type references into the shadow's package
// move to reg's package. Message options are dropped (they hold the
// gwag annotations).
//
// Stability: experimental
func ProducerCandidate(regFile protoreflect.FileDescriptor, target protoreflect.MessageDescriptor, shadow protoreflect.MessageDescriptor) (*descriptorpb.FileDescriptorProto, error) {
	fdp := protodesc.ToFileDescriptorProto(regFile)
	fdp.SourceCodeInfo = nil
	dp := protodesc.ToDescriptorProto(shadow)
	shadowName, targetName := "."+string(shadow.FullName()), "."+string(target.FullName())
	shadowPkg, regPkg := "."+string(shadow.ParentFile().Package()), "."+string(regFile.Package())
	var rewrite func(d *descriptorpb.DescriptorProto, reg protoreflect.MessageDescriptor)
	rewrite = func(d *descriptorpb.DescriptorProto, reg protoreflect.MessageDescriptor) {
		d.Options = nil
		for _, f := range d.Field {
			tn := f.GetTypeName()
			if tn == "" {
				continue
			}
			// A field the registry has keeps the registry's type.
			if reg != nil {
				if rf := reg.Fields().ByNumber(protoreflect.FieldNumber(f.GetNumber())); rf != nil {
					switch {
					case rf.Message() != nil && f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
						tn = "." + string(rf.Message().FullName())
						f.TypeName = &tn
						continue
					case rf.Enum() != nil && f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_ENUM:
						tn = "." + string(rf.Enum().FullName())
						f.TypeName = &tn
						continue
					}
				}
			}
			switch {
			case tn == shadowName || strings.HasPrefix(tn, shadowName+"."):
				tn = targetName + strings.TrimPrefix(tn, shadowName)
			case strings.HasPrefix(tn, shadowPkg+"."):
				tn = regPkg + strings.TrimPrefix(tn, shadowPkg)
			}
			f.TypeName = &tn
		}
		for _, n := range d.NestedType {
			rewrite(n, nil)
		}
	}
	rewrite(dp, target)
	name := string(target.Name())
	dp.Name = &name
	idx := MessageIndexes(target)
	msgs := &fdp.MessageType
	for depth, i := range idx {
		if i >= len(*msgs) {
			return nil, fmt.Errorf("message %s not found in %s", target.FullName(), regFile.Path())
		}
		if depth == len(idx)-1 {
			(*msgs)[i] = dp
			break
		}
		msgs = &(*msgs)[i].NestedType
	}
	// The shadow may use well-known types the registry file does not
	// import.
	have := map[string]bool{}
	for _, d := range fdp.Dependency {
		have[d] = true
	}
	imps := shadow.ParentFile().Imports()
	for i := 0; i < imps.Len(); i++ {
		p := imps.Get(i).Path()
		if Builtin(p) && !have[p] && usesFile(dp, imps.Get(i).FileDescriptor) {
			fdp.Dependency = append(fdp.Dependency, p)
			have[p] = true
		}
	}
	return fdp, nil
}

// usesFile reports whether d references a top-level type declared in
// fd.
func usesFile(d *descriptorpb.DescriptorProto, fd protoreflect.FileDescriptor) bool {
	pkg := string(fd.Package()) + "."
	for _, f := range d.Field {
		tn := strings.TrimPrefix(f.GetTypeName(), ".")
		if !strings.HasPrefix(tn, pkg) {
			continue
		}
		n := protoreflect.Name(strings.TrimPrefix(tn, pkg))
		if fd.Messages().ByName(n) != nil || fd.Enums().ByName(n) != nil {
			return true
		}
	}
	for _, n := range d.NestedType {
		if usesFile(n, fd) {
			return true
		}
	}
	return false
}
