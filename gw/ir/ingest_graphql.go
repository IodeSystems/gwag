package ir

import (
	"encoding/json"
	"fmt"
	"sort"
)

// IngestGraphQL parses the canonical introspection JSON ("data"
// envelope from running IntrospectionQuery against a service) and
// returns a single ir.Service whose Types and Operations mirror
// the introspected schema. Caller fills Service.Namespace/Version
// since GraphQL has no native namespace coordinate.
//
// The introspection model lives in this package so the IR doesn't
// depend back on the gw package; gw/graphql_introspect.go has its
// own copy for the runtime path. The two converge on the same
// wire format.
func IngestGraphQL(data json.RawMessage) (*Service, error) {
	var wire struct {
		Schema struct {
			QueryType        *struct{ Name string }    `json:"queryType"`
			MutationType     *struct{ Name string }    `json:"mutationType"`
			SubscriptionType *struct{ Name string }    `json:"subscriptionType"`
			Types            []wireGraphQLType         `json:"types"`
		} `json:"__schema"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode __schema: %w", err)
	}

	svc := &Service{
		Operations: []*Operation{},
		Types:      map[string]*Type{},
		OriginKind: KindGraphQL,
		Origin:     wire,
	}

	// First pass: register every named non-introspection type.
	for i := range wire.Schema.Types {
		t := &wire.Schema.Types[i]
		if isIntrospectionType(t.Name) {
			continue
		}
		if t.Name == "" {
			continue // wrapper types (LIST, NON_NULL) carry no name
		}
		switch t.Kind {
		case "OBJECT":
			obj := &Type{Name: t.Name, TypeKind: TypeObject, Description: t.Description, OriginKind: KindGraphQL, Origin: t}
			for _, f := range t.Fields {
				obj.Fields = append(obj.Fields, graphqlFieldToIRField(f))
			}
			svc.Types[t.Name] = obj
		case "INPUT_OBJECT":
			obj := &Type{Name: t.Name, TypeKind: TypeInput, Description: t.Description, OriginKind: KindGraphQL, Origin: t}
			for _, f := range t.InputFields {
				obj.Fields = append(obj.Fields, graphqlInputToIRField(f))
			}
			svc.Types[t.Name] = obj
		case "ENUM":
			obj := &Type{Name: t.Name, TypeKind: TypeEnum, Description: t.Description, OriginKind: KindGraphQL, Origin: t}
			for i, ev := range t.EnumValues {
				obj.Enum = append(obj.Enum, EnumValue{
					Name:        ev.Name,
					Number:      int32(i),
					Description: ev.Description,
					Deprecated:  ev.DeprecationReason,
				})
			}
			svc.Types[t.Name] = obj
		case "UNION":
			obj := &Type{Name: t.Name, TypeKind: TypeUnion, Description: t.Description, OriginKind: KindGraphQL, Origin: t}
			for _, p := range t.PossibleTypes {
				if p == nil || p.Name == "" {
					continue
				}
				obj.Variants = append(obj.Variants, p.Name)
			}
			svc.Types[t.Name] = obj
		case "INTERFACE":
			obj := &Type{Name: t.Name, TypeKind: TypeInterface, Description: t.Description, OriginKind: KindGraphQL, Origin: t}
			for _, f := range t.Fields {
				obj.Fields = append(obj.Fields, graphqlFieldToIRField(f))
			}
			for _, p := range t.PossibleTypes {
				if p == nil || p.Name == "" {
					continue
				}
				obj.Variants = append(obj.Variants, p.Name)
			}
			svc.Types[t.Name] = obj
		case "SCALAR":
			if !isBuiltinGraphQLScalar(t.Name) {
				svc.Types[t.Name] = &Type{
					Name:        t.Name,
					TypeKind:    TypeScalar,
					Description: t.Description,
					OriginKind:  KindGraphQL,
					Origin:      t,
				}
			}
		}
	}

	// Operations come from the three root types.
	rootName := func(p *struct{ Name string }) string {
		if p == nil {
			return ""
		}
		return p.Name
	}
	roots := []struct {
		name string
		kind OpKind
	}{
		{rootName(wire.Schema.QueryType), OpQuery},
		{rootName(wire.Schema.MutationType), OpMutation},
		{rootName(wire.Schema.SubscriptionType), OpSubscription},
	}
	for _, r := range roots {
		if r.name == "" {
			continue
		}
		root := findGraphQLType(wire.Schema.Types, r.name)
		if root == nil {
			continue
		}
		// Sorted so output is reproducible.
		fields := append([]wireGraphQLField(nil), root.Fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		for i := range fields {
			f := &fields[i]
			op := &Operation{
				Name:        f.Name,
				Kind:        r.kind,
				Description: f.Description,
				Deprecated:  f.DeprecationReason,
				OriginKind:  KindGraphQL,
				Origin:      f,
			}
			if ref := graphqlTypeRef(f.Type); ref != nil {
				op.Output = ref
				op.OutputRepeated = isListRef(f.Type)
				op.OutputRequired = isNonNullRef(f.Type)
				op.OutputItemRequired = isItemRequiredRef(f.Type)
			}
			for _, a := range f.Args {
				arg := &Arg{
					Name:        a.Name,
					Description: a.Description,
				}
				if ref := graphqlTypeRef(a.Type); ref != nil {
					arg.Type = *ref
				}
				arg.Repeated = isListRef(a.Type)
				arg.Required = isNonNullRef(a.Type)
				arg.ItemRequired = isItemRequiredRef(a.Type)
				op.Args = append(op.Args, arg)
			}
			svc.Operations = append(svc.Operations, op)
		}
	}
	return svc, nil
}

// graphqlFieldToIRField converts a wireGraphQLField to an IR Field.
// Used for OBJECT and INTERFACE types' fields.
func graphqlFieldToIRField(f wireGraphQLField) *Field {
	out := &Field{
		Name:        f.Name,
		JSONName:    f.Name,
		Description: f.Description,
		Deprecated:  f.DeprecationReason,
		OneofIndex:  -1,
	}
	if ref := graphqlTypeRef(f.Type); ref != nil {
		out.Type = *ref
	}
	out.Repeated = isListRef(f.Type)
	out.Required = isNonNullRef(f.Type)
	out.ItemRequired = isItemRequiredRef(f.Type)
	return out
}

func graphqlInputToIRField(f wireGraphQLInputValue) *Field {
	out := &Field{
		Name:        f.Name,
		JSONName:    f.Name,
		Description: f.Description,
		OneofIndex:  -1,
	}
	if ref := graphqlTypeRef(f.Type); ref != nil {
		out.Type = *ref
	}
	out.Repeated = isListRef(f.Type)
	out.Required = isNonNullRef(f.Type)
	out.ItemRequired = isItemRequiredRef(f.Type)
	return out
}

// graphqlTypeRef walks a (NON_NULL → LIST → NON_NULL → NAMED) wrapper
// chain and returns a TypeRef pointing at the innermost named type
// (or a primitive scalar). Repeated/Required are derived separately
// since they're field properties, not type-ref properties.
func graphqlTypeRef(r *wireGraphQLTypeRef) *TypeRef {
	if r == nil {
		return nil
	}
	cur := r
	for cur.Kind == "NON_NULL" || cur.Kind == "LIST" {
		if cur.OfType == nil {
			return nil
		}
		cur = cur.OfType
	}
	if cur.Name == "" {
		return nil
	}
	switch cur.Name {
	case "String":
		return &TypeRef{Builtin: ScalarString}
	case "Int":
		return &TypeRef{Builtin: ScalarInt32}
	case "Float":
		return &TypeRef{Builtin: ScalarDouble}
	case "Boolean":
		return &TypeRef{Builtin: ScalarBool}
	case "ID":
		return &TypeRef{Builtin: ScalarID}
	}
	return &TypeRef{Named: cur.Name}
}

func isListRef(r *wireGraphQLTypeRef) bool {
	for cur := r; cur != nil; cur = cur.OfType {
		if cur.Kind == "LIST" {
			return true
		}
	}
	return false
}

func isNonNullRef(r *wireGraphQLTypeRef) bool {
	if r == nil {
		return false
	}
	return r.Kind == "NON_NULL"
}

// isItemRequiredRef inspects a NON_NULL → LIST → ... wrapper chain
// and returns true when there's a NON_NULL inside the LIST (i.e.
// each list element must be non-null, GraphQL `[T!]`). Returns
// false for non-list refs.
func isItemRequiredRef(r *wireGraphQLTypeRef) bool {
	cur := r
	for cur != nil && cur.Kind == "NON_NULL" {
		cur = cur.OfType
	}
	if cur == nil || cur.Kind != "LIST" {
		return false
	}
	cur = cur.OfType
	return cur != nil && cur.Kind == "NON_NULL"
}

func isIntrospectionType(name string) bool {
	return len(name) >= 2 && name[:2] == "__"
}

func isBuiltinGraphQLScalar(name string) bool {
	switch name {
	case "String", "Int", "Float", "Boolean", "ID":
		return true
	}
	return false
}

func findGraphQLType(ts []wireGraphQLType, name string) *wireGraphQLType {
	for i := range ts {
		if ts[i].Name == name {
			return &ts[i]
		}
	}
	return nil
}

// Wire-format introspection types (mirror of those in
// gw/graphql_introspect.go). Kept private to ir/ so the package
// has no dependency back on gw.
type wireGraphQLType struct {
	Kind          string                  `json:"kind"`
	Name          string                  `json:"name"`
	Description   string                  `json:"description"`
	Fields        []wireGraphQLField      `json:"fields"`
	InputFields   []wireGraphQLInputValue `json:"inputFields"`
	EnumValues    []wireGraphQLEnumValue  `json:"enumValues"`
	Interfaces    []*wireGraphQLTypeRef   `json:"interfaces"`
	PossibleTypes []*wireGraphQLTypeRef   `json:"possibleTypes"`
}

type wireGraphQLField struct {
	Name              string                  `json:"name"`
	Description       string                  `json:"description"`
	Args              []wireGraphQLInputValue `json:"args"`
	Type              *wireGraphQLTypeRef     `json:"type"`
	IsDeprecated      bool                    `json:"isDeprecated"`
	DeprecationReason string                  `json:"deprecationReason"`
}

type wireGraphQLInputValue struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Type        *wireGraphQLTypeRef `json:"type"`
}

type wireGraphQLEnumValue struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	IsDeprecated      bool   `json:"isDeprecated"`
	DeprecationReason string `json:"deprecationReason"`
}

type wireGraphQLTypeRef struct {
	Kind   string              `json:"kind"`
	Name   string              `json:"name"`
	OfType *wireGraphQLTypeRef `json:"ofType"`
}

