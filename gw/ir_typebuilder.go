package gateway

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// IRTypeNaming controls how IR names project to graphql display names.
// Each callback maps an IR type / field / enum-value name to its
// rendered identifier. Per-format prefix conventions (proto's
// "Auth_V1_Context", OpenAPI's "<ns>_Pet", graphql-mirror's
// "<ns>_<vN>_Pet") live here, not inside the builder body — that's
// what lets one builder serve every origin.
//
// Zero-value naming is identity-everything for type names, lowerCamel
// for field names, and "<Object>_Input" for input objects.
type IRTypeNaming struct {
	ObjectName    func(irName string) string
	InputName     func(irName string) string
	EnumName      func(irName string) string
	UnionName     func(irName string) string
	InterfaceName func(irName string) string
	ScalarName    func(irName string) string
	FieldName     func(irName string) string
	EnumValueName func(irName string) string
}

// IRTypeBuilderOptions tweaks scalar projection. The defaults match
// the proto type-builder (int64 → String, map → JSON scalar);
// OpenAPI callers swap Int64Type / UInt64Type for the Long scalar so
// large integer responses survive the wire.
type IRTypeBuilderOptions struct {
	// Int64Type / UInt64Type render TypeRef.Builtin = ScalarInt64 /
	// ScalarUInt64. Default graphql.String matches the proto path's
	// "JSON Numbers can lose precision past 2^53" decision; OpenAPI
	// callers override with a Long-shaped custom scalar.
	Int64Type  graphql.Output
	UInt64Type graphql.Output

	// MapType renders TypeRef.Map. Default is a shared JSON scalar.
	// Callers that want to project maps to e.g. [Entry] can wire
	// their own type here.
	MapType graphql.Output

	// JSONType is the fallback used when an outputBase / inputBase
	// path hits a Map (input position) or an unknown named ref. When
	// nil the builder lazily constructs a private "JSON" scalar; pass
	// a shared scalar here when multiple per-source builders feed the
	// same graphql.Schema (graphql-go forbids two scalars sharing a
	// name).
	JSONType *graphql.Scalar

	// IDType renders TypeRef.Builtin = ScalarID. Default graphql.ID.
	IDType graphql.Output
}

// IRTypeBuilder produces graphql.{Object,InputObject,Enum,Union,Scalar}
// values from one ir.Service. Construct one per service+kind tuple
// (caller threads the same instance through every Field/Operation
// build call so cyclic refs share thunks); the builder caches by IR
// type name, so two graphql types with different naming policies
// require two builders.
//
// Concurrent calls aren't supported — graphql.NewObject's thunk
// closures aren't safe for parallel resolution either.
type IRTypeBuilder struct {
	svc       *ir.Service
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
// naming + scalar options. nil naming callbacks fall back to the
// defaults documented on IRTypeNaming. nil options uses
// graphql.String for int64/uint64, a shared JSON scalar for maps,
// and graphql.ID for ScalarID.
func NewIRTypeBuilder(svc *ir.Service, naming IRTypeNaming, opts IRTypeBuilderOptions) *IRTypeBuilder {
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

func identityName(s string) string { return s }

// jsonScalar lazily constructs a JSON scalar shared across the
// builder's lifetime — or returns the caller-supplied one when
// IRTypeBuilderOptions.JSONType is set (so multi-builder schemas
// can avoid the duplicate-scalar-name conflict).
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

// LongScalar returns a Long-named decimal-string scalar; OpenAPI
// callers wire it into IRTypeBuilderOptions.Int64Type / UInt64Type.
// Constructed once per builder because graphql-go forbids two
// scalars sharing a name in one schema.
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

// Output resolves an IR TypeRef to a graphql.Output, applying
// list / non-null / item-required wrappers in the canonical order
// (T, T!, [T], [T]!, [T!], [T!]!).
func (b *IRTypeBuilder) Output(ref ir.TypeRef, repeated, required, itemRequired bool) (graphql.Output, error) {
	t, err := b.outputBase(ref)
	if err != nil {
		return nil, err
	}
	return wrapOutput(t, repeated, required, itemRequired), nil
}

// Input is the input-side counterpart of Output. References to
// TypeUnion/TypeInterface aren't legal as inputs; the builder
// returns an error rather than silently downgrading to a scalar.
func (b *IRTypeBuilder) Input(ref ir.TypeRef, repeated, required, itemRequired bool) (graphql.Input, error) {
	t, err := b.inputBase(ref)
	if err != nil {
		return nil, err
	}
	return wrapInput(t, repeated, required, itemRequired), nil
}

func (b *IRTypeBuilder) outputBase(ref ir.TypeRef) (graphql.Output, error) {
	switch {
	case ref.IsMap():
		return b.options.MapType, nil
	case ref.IsNamed():
		t, ok := b.svc.Types[ref.Named]
		if !ok {
			// Unknown ref: fall through to JSON to keep the schema
			// emittable. Same posture as the OpenAPI builder.
			return b.jsonScalar(), nil
		}
		return b.outputForType(t)
	}
	return b.scalar(ref.Builtin)
}

func (b *IRTypeBuilder) inputBase(ref ir.TypeRef) (graphql.Input, error) {
	switch {
	case ref.IsMap():
		// Maps don't have a native graphql Input shape; project to
		// JSON same as outputs. graphql-go accepts scalars as inputs.
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

func (b *IRTypeBuilder) outputForType(t *ir.Type) (graphql.Output, error) {
	switch t.TypeKind {
	case ir.TypeObject:
		return b.objectFor(t), nil
	case ir.TypeInput:
		// Input-only type used in an output position: surface as JSON.
		// Cross-kind ingest typically doesn't generate this, but keep
		// the path safe.
		return b.jsonScalar(), nil
	case ir.TypeEnum:
		return b.enumFor(t), nil
	case ir.TypeUnion:
		return b.unionFor(t)
	case ir.TypeInterface:
		// Interface ingested from GraphQL: graphql-go can't easily
		// satisfy the "must declare implementing types" constraint
		// during thunk-build for cross-format use, so project to a
		// union of the interface's variants. Same posture as the
		// existing graphQLMirror path.
		return b.unionFor(t)
	case ir.TypeScalar:
		return b.scalarFor(t), nil
	}
	return nil, fmt.Errorf("ir typebuilder: unknown TypeKind %v for %s", t.TypeKind, t.Name)
}

func (b *IRTypeBuilder) inputForType(t *ir.Type) (graphql.Input, error) {
	switch t.TypeKind {
	case ir.TypeObject, ir.TypeInput:
		return b.inputObjectFor(t), nil
	case ir.TypeEnum:
		return b.enumFor(t), nil
	case ir.TypeScalar:
		return b.scalarFor(t), nil
	}
	return nil, fmt.Errorf("ir typebuilder: %v not valid in input position (%s)", t.TypeKind, t.Name)
}

func (b *IRTypeBuilder) objectFor(t *ir.Type) *graphql.Object {
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
				ft, err := b.Output(f.Type, f.Repeated, f.Required, f.ItemRequired)
				if err != nil {
					continue
				}
				fields[b.naming.FieldName(f.Name)] = &graphql.Field{
					Type:              ft,
					Description:       f.Description,
					DeprecationReason: f.Deprecated,
				}
			}
			if len(fields) == 0 {
				// graphql-go forbids empty Object types; proto allows
				// them (e.g. DeregisterResponse {}). Synthesise a
				// placeholder to keep the schema valid.
				fields["_void"] = &graphql.Field{Type: graphql.String}
			}
			return fields
		}),
	})
	b.objects[t.Name] = obj
	return obj
}

func (b *IRTypeBuilder) inputObjectFor(t *ir.Type) *graphql.InputObject {
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
				ft, err := b.Input(f.Type, f.Repeated, f.Required, f.ItemRequired)
				if err != nil {
					continue
				}
				fields[b.naming.FieldName(f.Name)] = &graphql.InputObjectFieldConfig{
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

func (b *IRTypeBuilder) enumFor(t *ir.Type) *graphql.Enum {
	if e, ok := b.enums[t.Name]; ok {
		return e
	}
	values := graphql.EnumValueConfigMap{}
	for _, v := range t.Enum {
		name := b.naming.EnumValueName(v.Name)
		values[name] = &graphql.EnumValueConfig{
			Value:             v.Number,
			Description:       v.Description,
			DeprecationReason: v.Deprecated,
		}
	}
	if len(values) == 0 {
		// graphql-go rejects empty enums. Conservative placeholder
		// keeps the schema valid; ingesters that emit empty enums
		// can be tightened later.
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

// unionFor projects an IR Union/Interface into a graphql.Union over
// the resolved variant Object types. Variants that don't resolve to
// Objects (missing from svc.Types, or another abstract kind) are
// dropped; an empty result falls back to a JSON scalar so the field
// still surfaces.
//
// ResolveType priority:
//  1. DiscriminatorProperty (OpenAPI's schema.discriminator.propertyName)
//     consulted when set: lookup via DiscriminatorMapping, fall through
//     to a variant-name identity match.
//  2. GraphQL `__typename` convention.
//  3. "First variant whose required fields are all present" heuristic
//     — matches the legacy openAPITypeBuilder behavior so payloads
//     without a discriminator still resolve.
func (b *IRTypeBuilder) unionFor(t *ir.Type) (graphql.Output, error) {
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
		if !ok || variantType.TypeKind != ir.TypeObject {
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
		// Also key by the rendered Object name so __typename matches
		// the local schema's identifier.
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

func (b *IRTypeBuilder) scalarFor(t *ir.Type) *graphql.Scalar {
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

func (b *IRTypeBuilder) scalar(s ir.ScalarKind) (graphql.Output, error) {
	switch s {
	case ir.ScalarBool:
		return graphql.Boolean, nil
	case ir.ScalarInt32, ir.ScalarUInt32:
		return graphql.Int, nil
	case ir.ScalarInt64:
		return b.options.Int64Type, nil
	case ir.ScalarUInt64:
		return b.options.UInt64Type, nil
	case ir.ScalarFloat, ir.ScalarDouble:
		return graphql.Float, nil
	case ir.ScalarString, ir.ScalarBytes, ir.ScalarTimestamp:
		return graphql.String, nil
	case ir.ScalarID:
		return b.options.IDType, nil
	case ir.ScalarUnknown:
		// OpenAPI ingest emits a zero TypeRef when a schema doesn't
		// classify cleanly (mixed-kind oneOf, missing fields). Project
		// to JSONType when the caller wired one — that matches the
		// legacy openAPITypeBuilder fallback. Proto callers leave
		// JSONType nil, so they keep the previous String behavior.
		if b.options.JSONType != nil {
			return b.options.JSONType, nil
		}
		return graphql.String, nil
	}
	return nil, fmt.Errorf("ir typebuilder: unknown ScalarKind %v", s)
}

// wrapOutput / wrapInput apply Repeated → List, ItemRequired →
// inner NON_NULL, Required → outer NON_NULL. Mirrors
// gw/ir/render_graphql.go's typeRefStr ordering so the produced
// graphql types match the rendered SDL.
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
