package gateway

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/graphql-go/graphql"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// typeBuilder memoises graphql type construction so cycles and shared
// types resolve once. Output objects and input objects are tracked
// separately because GraphQL distinguishes them.
type typeBuilder struct {
	policy  *policy
	objects map[protoreflect.FullName]*graphql.Object
	inputs  map[protoreflect.FullName]*graphql.InputObject
	enums   map[protoreflect.FullName]*graphql.Enum
}

// hidden reports whether a message-typed field should be omitted from
// any input position. Only message kinds participate in hide rules.
func (tb *typeBuilder) hidden(fd protoreflect.FieldDescriptor) bool {
	if tb.policy == nil {
		return false
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return false
	}
	return tb.policy.hides[fd.Message().FullName()]
}

func (tb *typeBuilder) objectFromMessage(md protoreflect.MessageDescriptor) (*graphql.Object, error) {
	if obj, ok := tb.objects[md.FullName()]; ok {
		return obj, nil
	}
	name := exportedName(string(md.FullName()))
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name: name,
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			fields := graphql.Fields{}
			for i := 0; i < md.Fields().Len(); i++ {
				fd := md.Fields().Get(i)
				t, err := tb.outputType(fd)
				if err != nil {
					continue
				}
				fields[lowerCamel(string(fd.Name()))] = &graphql.Field{Type: t}
			}
			// GraphQL forbids empty object types; proto allows them
			// (e.g. DeregisterResponse {}). Synthesise a placeholder
			// so the schema validates.
			if len(fields) == 0 {
				fields["_void"] = &graphql.Field{
					Type: graphql.String,
					Resolve: func(graphql.ResolveParams) (any, error) {
						return "", nil
					},
				}
			}
			return fields
		}),
	})
	tb.objects[md.FullName()] = obj
	return obj, nil
}

func (tb *typeBuilder) inputFromMessage(md protoreflect.MessageDescriptor) (*graphql.InputObject, error) {
	if obj, ok := tb.inputs[md.FullName()]; ok {
		return obj, nil
	}
	name := exportedName(string(md.FullName())) + "_Input"
	obj := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
			fields := graphql.InputObjectConfigFieldMap{}
			for i := 0; i < md.Fields().Len(); i++ {
				fd := md.Fields().Get(i)
				if tb.hidden(fd) {
					continue
				}
				t, err := tb.inputType(fd)
				if err != nil {
					continue
				}
				fields[lowerCamel(string(fd.Name()))] = &graphql.InputObjectFieldConfig{Type: t}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.InputObjectFieldConfig{Type: graphql.String}
			}
			return fields
		}),
	})
	tb.inputs[md.FullName()] = obj
	return obj, nil
}

func (tb *typeBuilder) argsFromMessage(md protoreflect.MessageDescriptor) (graphql.FieldConfigArgument, error) {
	out := graphql.FieldConfigArgument{}
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if tb.hidden(fd) {
			continue
		}
		t, err := tb.inputType(fd)
		if err != nil {
			return nil, err
		}
		out[lowerCamel(string(fd.Name()))] = &graphql.ArgumentConfig{Type: t}
	}
	return out, nil
}

func (tb *typeBuilder) outputType(fd protoreflect.FieldDescriptor) (graphql.Output, error) {
	t, err := tb.scalarOrObject(fd, false)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		t = graphql.NewList(t)
	}
	return t, nil
}

func (tb *typeBuilder) inputType(fd protoreflect.FieldDescriptor) (graphql.Input, error) {
	t, err := tb.scalarOrObject(fd, true)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		t = graphql.NewList(t)
	}
	return t.(graphql.Input), nil
}

// scalarOrObject returns either a scalar / enum, or a (NewObject /
// NewInputObject) for message-typed fields. Whether the message form is
// input or output is selected by `asInput`.
func (tb *typeBuilder) scalarOrObject(fd protoreflect.FieldDescriptor, asInput bool) (graphql.Type, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return graphql.Boolean, nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind:
		return graphql.Int, nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		// JSON numbers can lose precision past 2^53; prefer string for safety.
		return graphql.String, nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return graphql.Float, nil
	case protoreflect.StringKind, protoreflect.BytesKind:
		return graphql.String, nil
	case protoreflect.EnumKind:
		return tb.enumFromDescriptor(fd.Enum()), nil
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if asInput {
			return tb.inputFromMessage(fd.Message())
		}
		return tb.objectFromMessage(fd.Message())
	default:
		return nil, fmt.Errorf("unsupported field kind: %v", fd.Kind())
	}
}

func (tb *typeBuilder) enumFromDescriptor(ed protoreflect.EnumDescriptor) *graphql.Enum {
	if e, ok := tb.enums[ed.FullName()]; ok {
		return e
	}
	values := graphql.EnumValueConfigMap{}
	for i := 0; i < ed.Values().Len(); i++ {
		vd := ed.Values().Get(i)
		values[string(vd.Name())] = &graphql.EnumValueConfig{Value: int32(vd.Number())}
	}
	e := graphql.NewEnum(graphql.EnumConfig{
		Name:   exportedName(string(ed.FullName())),
		Values: values,
	})
	tb.enums[ed.FullName()] = e
	return e
}

// exportedName converts a proto full name like "auth.v1.Context" into a
// GraphQL-safe type name like "Auth_V1_Context".
func exportedName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	return strings.Join(parts, "_")
}

func lowerCamel(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "_")
	for i := range parts {
		if i == 0 {
			continue
		}
		if parts[i] == "" {
			continue
		}
		r := []rune(parts[i])
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	r := []rune(parts[0])
	if len(r) > 0 {
		r[0] = unicode.ToLower(r[0])
		parts[0] = string(r)
	}
	return strings.Join(parts, "")
}
