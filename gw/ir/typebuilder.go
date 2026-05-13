package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/IodeSystems/graphql-go"
	"github.com/IodeSystems/graphql-go/language/ast"
)

// lowerCamel converts snake_case to lowerCamelCase.
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

// IdentityName returns the input unchanged.
//
// Stability: stable
func IdentityName(s string) string { return s }

func identityName(s string) string { return s }

// isValidGraphQLIdent reports whether s matches GraphQL's identifier
// regex /^[_A-Za-z][_A-Za-z0-9]*$/.
func isValidGraphQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// IRTypeNaming controls how IR names project to graphql display names.
//
// Stability: stable
type IRTypeNaming struct {
	ObjectName    func(irName string) string
	InputName     func(irName string) string
	EnumName      func(irName string) string
	UnionName     func(irName string) string
	InterfaceName func(irName string) string
	ScalarName    func(irName string) string
	FieldName     func(irName string) string
	EnumValueName func(irName string) string

	// EnumValueValue returns the Go-side runtime Value graphql-go
	// will compare against when coercing inbound enum literals and
	// outbound payloads. Default returns the IR EnumValue.Number.
	EnumValueValue func(EnumValue) any
}

// IRTypeBuilderOptions tweaks scalar projection.
//
// Stability: stable
type IRTypeBuilderOptions struct {
	Int64Type  graphql.Output
	UInt64Type graphql.Output
	MapType    graphql.Output
	JSONType   *graphql.Scalar
	IDType     graphql.Output
}

// IRTypeBuilder produces graphql.{Object,InputObject,Enum,Union,Scalar}
// values from one Service. Construct one per service+kind tuple;
// the builder caches by IR type name.
//
// Stability: stable
type IRTypeBuilder struct {
	svc       *Service
	naming    IRTypeNaming
	options   IRTypeBuilderOptions
	objects   map[string]*graphql.Object
	inputs    map[string]*graphql.InputObject
	enums     map[string]*graphql.Enum
	unions    map[string]*graphql.Union
	scalars   map[string]*graphql.Scalar
	jsonOnce  bool
	jsonValue *graphql.Scalar
}

// NewIRTypeBuilder constructs a builder over svc with the supplied
// naming + scalar options.
//
// Stability: stable
func NewIRTypeBuilder(svc *Service, naming IRTypeNaming, opts IRTypeBuilderOptions) *IRTypeBuilder {
	b := &IRTypeBuilder{
		svc:     svc,
		naming:  naming,
		options: opts,
		objects: map[string]*graphql.Object{},
		inputs:  map[string]*graphql.InputObject{},
		enums:   map[string]*graphql.Enum{},
		unions:  map[string]*graphql.Union{},
		scalars: map[string]*graphql.Scalar{},
	}
	if b.naming.ObjectName == nil {
		b.naming.ObjectName = identityName
	}
	if b.naming.InputName == nil {
		b.naming.InputName = func(s string) string { return b.naming.ObjectName(s) + "_Input" }
	}
	if b.naming.EnumName == nil {
		b.naming.EnumName = identityName
	}
	if b.naming.UnionName == nil {
		b.naming.UnionName = identityName
	}
	if b.naming.InterfaceName == nil {
		b.naming.InterfaceName = identityName
	}
	if b.naming.ScalarName == nil {
		b.naming.ScalarName = identityName
	}
	if b.naming.FieldName == nil {
		b.naming.FieldName = lowerCamel
	}
	if b.naming.EnumValueName == nil {
		b.naming.EnumValueName = identityName
	}
	if b.naming.EnumValueValue == nil {
		b.naming.EnumValueValue = func(v EnumValue) any { return v.Number }
	}
	if b.options.Int64Type == nil {
		b.options.Int64Type = graphql.String
	}
	if b.options.UInt64Type == nil {
		b.options.UInt64Type = graphql.String
	}
	if b.options.IDType == nil {
		b.options.IDType = graphql.ID
	}
	if b.options.MapType == nil {
		b.options.MapType = b.jsonScalar()
	}
	return b
}

// jsonScalar lazily constructs a JSON scalar shared across the
// builder's lifetime.
func (b *IRTypeBuilder) jsonScalar() *graphql.Scalar {
	if b.options.JSONType != nil {
		return b.options.JSONType
	}
	if b.jsonOnce {
		return b.jsonValue
	}
	b.jsonOnce = true
	b.jsonValue = graphql.NewScalar(graphql.ScalarConfig{
		Name:         "JSON",
		Description:  "Untyped JSON value (used as a fallback for IR map / unmappable scalar shapes).",
		Serialize:    func(v any) any { return v },
		ParseValue:   func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any { return v },
	})
	return b.jsonValue
}

// LongScalar returns a Long-named decimal-string scalar.
//
// Stability: stable
func (b *IRTypeBuilder) LongScalar() *graphql.Scalar {
	if s, ok := b.scalars["__Long"]; ok {
		return s
	}
	s := graphql.NewScalar(graphql.ScalarConfig{
		Name: "Long",
		Description: "64-bit integer encoded as a decimal string. graphql-go's " +
			"built-in Int is signed 32-bit; this scalar preserves precision " +
			"for int64 / uint64 values above 2^31.",
		Serialize: func(v any) any {
			switch x := v.(type) {
			case float64:
				return strconv.FormatInt(int64(x), 10)
			case int64:
				return strconv.FormatInt(x, 10)
			case uint64:
				return strconv.FormatUint(x, 10)
			case int:
				return strconv.Itoa(x)
			case string:
				return x
			case json.Number:
				return x.String()
			}
			return nil
		},
		ParseValue: func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any {
			switch x := v.(type) {
			case *ast.StringValue:
				return x.Value
			case *ast.IntValue:
				return x.Value
			}
			return nil
		},
	})
	b.scalars["__Long"] = s
	return s
}

// Output resolves an IR TypeRef to a graphql.Output.
//
// Stability: stable
func (b *IRTypeBuilder) Output(ref TypeRef, repeated, required, itemRequired bool) (graphql.Output, error) {
	t, err := b.outputBase(ref)
	if err != nil {
		return nil, err
	}
	return wrapOutput(t, repeated, required, itemRequired), nil
}

// Input is the input-side counterpart of Output.
//
// Stability: stable
func (b *IRTypeBuilder) Input(ref TypeRef, repeated, required, itemRequired bool) (graphql.Input, error) {
	t, err := b.inputBase(ref)
	if err != nil {
		return nil, err
	}
	return wrapInput(t, repeated, required, itemRequired), nil
}

func (b *IRTypeBuilder) outputBase(ref TypeRef) (graphql.Output, error) {
	switch {
	case ref.IsMap():
		return b.options.MapType, nil
	case ref.IsNamed():
		t, ok := b.svc.Types[ref.Named]
		if !ok {
			return b.jsonScalar(), nil
		}
		return b.outputForType(t)
	}
	return b.scalar(ref.Builtin)
}

func (b *IRTypeBuilder) inputBase(ref TypeRef) (graphql.Input, error) {
	switch {
	case ref.IsMap():
		return b.jsonScalar(), nil
	case ref.IsNamed():
		t, ok := b.svc.Types[ref.Named]
		if !ok {
			return b.jsonScalar(), nil
		}
		return b.inputForType(t)
	}
	s, err := b.scalar(ref.Builtin)
	if err != nil {
		return nil, err
	}
	in, ok := s.(graphql.Input)
	if !ok {
		return nil, fmt.Errorf("ir typebuilder: scalar %T is not graphql.Input", s)
	}
	return in, nil
}

func (b *IRTypeBuilder) outputForType(t *Type) (graphql.Output, error) {
	switch t.TypeKind {
	case TypeObject:
		return b.objectFor(t), nil
	case TypeInput:
		return b.jsonScalar(), nil
	case TypeEnum:
		return b.enumFor(t), nil
	case TypeUnion:
		return b.unionFor(t)
	case TypeInterface:
		return b.unionFor(t)
	case TypeScalar:
		return b.scalarFor(t), nil
	}
	return nil, fmt.Errorf("ir typebuilder: unknown TypeKind %v for %s", t.TypeKind, t.Name)
}

func (b *IRTypeBuilder) inputForType(t *Type) (graphql.Input, error) {
	switch t.TypeKind {
	case TypeObject, TypeInput:
		return b.inputObjectFor(t), nil
	case TypeEnum:
		return b.enumFor(t), nil
	case TypeScalar:
		return b.scalarFor(t), nil
	}
	return nil, fmt.Errorf("ir typebuilder: %v not valid in input position (%s)", t.TypeKind, t.Name)
}

func (b *IRTypeBuilder) objectFor(t *Type) *graphql.Object {
	if obj, ok := b.objects[t.Name]; ok {
		return obj
	}
	name := b.naming.ObjectName(t.Name)
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name:        name,
		Description: t.Description,
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			fields := graphql.Fields{}
			for _, f := range t.Fields {
				key := b.naming.FieldName(f.Name)
				if !isValidGraphQLIdent(key) {
					continue
				}
				ft, err := b.Output(f.Type, f.Repeated, f.Required, f.ItemRequired)
				if err != nil {
					continue
				}
				fields[key] = &graphql.Field{
					Type:              ft,
					Description:       f.Description,
					DeprecationReason: f.Deprecated,
				}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.Field{Type: graphql.String}
			}
			return fields
		}),
	})
	b.objects[t.Name] = obj
	return obj
}

func (b *IRTypeBuilder) inputObjectFor(t *Type) *graphql.InputObject {
	if io, ok := b.inputs[t.Name]; ok {
		return io
	}
	name := b.naming.InputName(t.Name)
	io := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:        name,
		Description: t.Description,
		Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
			fields := graphql.InputObjectConfigFieldMap{}
			for _, f := range t.Fields {
				key := b.naming.FieldName(f.Name)
				if !isValidGraphQLIdent(key) {
					continue
				}
				ft, err := b.Input(f.Type, f.Repeated, f.Required, f.ItemRequired)
				if err != nil {
					continue
				}
				fields[key] = &graphql.InputObjectFieldConfig{
					Type:        ft,
					Description: f.Description,
				}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.InputObjectFieldConfig{Type: graphql.String}
			}
			return fields
		}),
	})
	b.inputs[t.Name] = io
	return io
}

func (b *IRTypeBuilder) enumFor(t *Type) *graphql.Enum {
	if e, ok := b.enums[t.Name]; ok {
		return e
	}
	values := graphql.EnumValueConfigMap{}
	for _, v := range t.Enum {
		name := b.naming.EnumValueName(v.Name)
		values[name] = &graphql.EnumValueConfig{
			Value:             b.naming.EnumValueValue(v),
			Description:       v.Description,
			DeprecationReason: v.Deprecated,
		}
	}
	if len(values) == 0 {
		values["_EMPTY"] = &graphql.EnumValueConfig{Value: int32(0)}
	}
	e := graphql.NewEnum(graphql.EnumConfig{
		Name:        b.naming.EnumName(t.Name),
		Description: t.Description,
		Values:      values,
	})
	b.enums[t.Name] = e
	return e
}

func (b *IRTypeBuilder) unionFor(t *Type) (graphql.Output, error) {
	if u, ok := b.unions[t.Name]; ok {
		return u, nil
	}
	type variantInfo struct {
		obj      *graphql.Object
		required []string
	}
	variants := []variantInfo{}
	types := []*graphql.Object{}
	byName := map[string]*graphql.Object{}
	for _, v := range t.Variants {
		variantType, ok := b.svc.Types[v]
		if !ok || variantType.TypeKind != TypeObject {
			continue
		}
		obj := b.objectFor(variantType)
		req := []string{}
		for _, f := range variantType.Fields {
			if f.Required {
				req = append(req, f.Name)
			}
		}
		variants = append(variants, variantInfo{obj: obj, required: req})
		types = append(types, obj)
		byName[v] = obj
		byName[obj.Name()] = obj
	}
	if len(types) == 0 {
		return b.jsonScalar(), nil
	}
	discProp := t.DiscriminatorProperty
	discMap := t.DiscriminatorMapping
	u := graphql.NewUnion(graphql.UnionConfig{
		Name:        b.naming.UnionName(t.Name),
		Description: t.Description,
		Types:       types,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			m, ok := p.Value.(map[string]any)
			if !ok {
				return nil
			}
			if discProp != "" {
				if d, ok := m[discProp].(string); ok {
					if mapped, ok := discMap[d]; ok {
						if obj, ok := byName[mapped]; ok {
							return obj
						}
					}
					if obj, ok := byName[d]; ok {
						return obj
					}
				}
			}
			if name, ok := m["__typename"].(string); ok {
				if obj, ok := byName[name]; ok {
					return obj
				}
			}
			for _, v := range variants {
				ok := true
				for _, name := range v.required {
					if _, present := m[name]; !present {
						ok = false
						break
					}
				}
				if ok {
					return v.obj
				}
			}
			return nil
		},
	})
	b.unions[t.Name] = u
	return u, nil
}

func (b *IRTypeBuilder) scalarFor(t *Type) *graphql.Scalar {
	if s, ok := b.scalars[t.Name]; ok {
		return s
	}
	s := graphql.NewScalar(graphql.ScalarConfig{
		Name:         b.naming.ScalarName(t.Name),
		Description:  t.Description,
		Serialize:    func(v any) any { return v },
		ParseValue:   func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any { return v },
	})
	b.scalars[t.Name] = s
	return s
}

func (b *IRTypeBuilder) scalar(s ScalarKind) (graphql.Output, error) {
	switch s {
	case ScalarBool:
		return graphql.Boolean, nil
	case ScalarInt32, ScalarUInt32:
		return graphql.Int, nil
	case ScalarInt64:
		return b.options.Int64Type, nil
	case ScalarUInt64:
		return b.options.UInt64Type, nil
	case ScalarFloat, ScalarDouble:
		return graphql.Float, nil
	case ScalarString, ScalarBytes, ScalarTimestamp:
		return graphql.String, nil
	case ScalarID:
		return b.options.IDType, nil
	case ScalarUnknown:
		if b.options.JSONType != nil {
			return b.options.JSONType, nil
		}
		return graphql.String, nil
	}
	return nil, fmt.Errorf("ir typebuilder: unknown ScalarKind %v", s)
}

// wrapOutput / wrapInput apply Repeated → List, ItemRequired →
// inner NON_NULL, Required → outer NON_NULL.
func wrapOutput(t graphql.Output, repeated, required, itemRequired bool) graphql.Output {
	inner := t
	if repeated {
		if itemRequired {
			inner = graphql.NewNonNull(inner)
		}
		inner = graphql.NewList(inner)
	}
	if required {
		inner = graphql.NewNonNull(inner)
	}
	return inner
}

func wrapInput(t graphql.Input, repeated, required, itemRequired bool) graphql.Input {
	var inner graphql.Input = t
	if repeated {
		if itemRequired {
			inner = graphql.NewNonNull(inner)
		}
		inner = graphql.NewList(inner)
	}
	if required {
		inner = graphql.NewNonNull(inner)
	}
	return inner
}
