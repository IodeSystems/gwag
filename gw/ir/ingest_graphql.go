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

	// Root type names are special — they're walked below to extract
	// Operations, but never land in svc.Types since they don't
	// represent reusable data shapes.
	rootTypeNames := map[string]bool{}
	if n := wire.Schema.QueryType; n != nil {
		rootTypeNames[n.Name] = true
	}
	if n := wire.Schema.MutationType; n != nil {
		rootTypeNames[n.Name] = true
	}
	if n := wire.Schema.SubscriptionType; n != nil {
		rootTypeNames[n.Name] = true
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
		if rootTypeNames[t.Name] {
			continue
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

	// Operations come from the three root types. Each root field is
	// classified as either a flat Operation or a nested
	// OperationGroup; namespace-container types found during the
	// classification walk are stripped from svc.Types since they're
	// IR-synthesized rather than real data types.
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
	namespaceTypes := map[string]bool{}
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
			op, grp := classifyGraphQLRootField(wire.Schema.Types, f, r.kind, namespaceTypes)
			if op != nil {
				svc.Operations = append(svc.Operations, op)
			}
			if grp != nil {
				svc.Groups = append(svc.Groups, grp)
			}
		}
	}
	// Prune namespace-container types — they're not data, just
	// IR-synthesized parents for the Groups tree. Round-trip via
	// re-render synthesizes them back from the Groups.
	for n := range namespaceTypes {
		delete(svc.Types, n)
	}
	return svc, nil
}

// classifyGraphQLRootField decides whether one field on a root (or
// nested-namespace) type is a flat Operation or an OperationGroup.
// The heuristic: a field with zero args whose return type unwraps
// to a single OBJECT (no LIST) and whose return type transitively
// contains at least one field-with-args is a Group; otherwise it
// is an Operation. namespaceTypes accumulates the names of
// namespace-container types so the caller can prune them from
// svc.Types after the walk.
func classifyGraphQLRootField(types []wireGraphQLType, f *wireGraphQLField, kind OpKind, namespaceTypes map[string]bool) (*Operation, *OperationGroup) {
	if grp := tryGraphQLGroup(types, f, kind, namespaceTypes, map[string]bool{}); grp != nil {
		return nil, grp
	}
	return graphqlFieldToOperation(f, kind), nil
}

// tryGraphQLGroup attempts to classify f as an OperationGroup. Returns
// nil if f is a flat Operation. seen tracks types under recursive
// inspection to break cycles in pathological schemas.
func tryGraphQLGroup(types []wireGraphQLType, f *wireGraphQLField, kind OpKind, namespaceTypes, seen map[string]bool) *OperationGroup {
	if len(f.Args) > 0 {
		return nil
	}
	objName := unwrapToObjectName(f.Type)
	if objName == "" {
		return nil
	}
	target := findGraphQLType(types, objName)
	if target == nil || target.Kind != "OBJECT" {
		return nil
	}
	if !typeHasOperations(types, target, map[string]bool{}) {
		return nil
	}
	// Cycle detection on the recursive build path. A namespace that
	// references itself (directly or via sub-groups) gets cut at the
	// first re-entry.
	if seen[objName] {
		return nil
	}
	seen[objName] = true
	defer delete(seen, objName)

	grp := &OperationGroup{Name: f.Name, Description: f.Description, Kind: kind}
	subFields := append([]wireGraphQLField(nil), target.Fields...)
	sort.Slice(subFields, func(i, j int) bool { return subFields[i].Name < subFields[j].Name })
	for i := range subFields {
		sf := &subFields[i]
		if sub := tryGraphQLGroup(types, sf, kind, namespaceTypes, seen); sub != nil {
			grp.Groups = append(grp.Groups, sub)
			continue
		}
		grp.Operations = append(grp.Operations, graphqlFieldToOperation(sf, kind))
	}
	namespaceTypes[objName] = true
	return grp
}

// graphqlFieldToOperation projects one GraphQL field onto an IR
// Operation. Used both for root-level fields that classified as
// Operations and for fields on namespace-container types that
// classified as Operations under a parent Group.
func graphqlFieldToOperation(f *wireGraphQLField, kind OpKind) *Operation {
	op := &Operation{
		Name:        f.Name,
		Kind:        kind,
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
	return op
}

// typeHasOperations returns true if t (an OBJECT type) contains at
// least one field-with-args directly or transitively through a
// sub-object reference. Mirror of the namespace-classification
// heuristic; used to decide whether to descend into a candidate
// namespace.
func typeHasOperations(types []wireGraphQLType, t *wireGraphQLType, visited map[string]bool) bool {
	if t == nil || visited[t.Name] {
		return false
	}
	visited[t.Name] = true
	for _, f := range t.Fields {
		if len(f.Args) > 0 {
			return true
		}
		innerName := unwrapToObjectName(f.Type)
		if innerName == "" {
			continue
		}
		inner := findGraphQLType(types, innerName)
		if inner != nil && inner.Kind == "OBJECT" && typeHasOperations(types, inner, visited) {
			return true
		}
	}
	return false
}

// unwrapToObjectName returns the OBJECT type's name if r is OBJECT
// or NON_NULL→OBJECT. Returns "" if the wrapper chain crosses a
// LIST or terminates at anything other than an OBJECT — namespace
// containers must be a single object, not a list.
func unwrapToObjectName(r *wireGraphQLTypeRef) string {
	cur := r
	for cur != nil {
		switch cur.Kind {
		case "NON_NULL":
			cur = cur.OfType
		case "LIST":
			return ""
		case "OBJECT":
			return cur.Name
		default:
			return ""
		}
	}
	return ""
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

