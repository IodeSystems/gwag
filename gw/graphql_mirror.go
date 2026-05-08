package gateway

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// graphQLMirror walks the introspection model of one downstream
// service and produces graphql-go types prefixed with the source's
// namespace. The Object/Scalar/Enum/InputObject/List/NonNull subset is
// covered; Interfaces and Unions project to graphql.Union (the
// gateway forwards the full subselection, so __typename suffices for
// resolving the variant locally). Subscription roots are wired
// separately via subscribingResolver.
//
// Type construction (Object/Input/Enum/Scalar/Union) goes through
// IRTypeBuilder so the projection rules live in one place. Wrapper
// chains (LIST / NON_NULL) and root-field iteration stay in the
// mirror because they walk the introspection model directly. Inline-
// fragment AST rewriting (unprefixTypeName) and the forwarding
// resolver are independent of type construction and stay too.
type graphQLMirror struct {
	src         *graphQLSource
	metrics     Metrics
	bp          BackpressureOptions
	dispatchers *ir.DispatchRegistry
	tb          *IRTypeBuilder
	// isLatest controls whether this mirror's fields and types use
	// the bare "<ns>_" prefix (true, single-version or current) or
	// the disambiguated "<ns>_<vN>_" prefix (false, older version).
	// Older versions also stamp deprecationReason on every emitted
	// field so codegen consumers see "<latest> is current".
	isLatest          bool
	deprecationReason string
}

func newGraphQLMirror(src *graphQLSource, metrics Metrics, bp BackpressureOptions, dispatchers *ir.DispatchRegistry, jsonScalar *graphql.Scalar) *graphQLMirror {
	if metrics == nil {
		metrics = noopMetrics{}
	}
	m := &graphQLMirror{
		src:         src,
		metrics:     metrics,
		bp:          bp,
		dispatchers: dispatchers,
		isLatest:    true,
	}
	m.tb = newGraphQLSourceTypeBuilder(introspectionToIRService(src.introspection), m.prefixCallback, jsonScalar)
	return m
}

// prefixCallback returns the per-source naming projection. Closes
// over the mirror so isLatest can flip after construction (the
// versioned-vs-latest decision is made in buildGraphQLFields after
// the mirror is built but before the first type is materialized).
func (m *graphQLMirror) prefixCallback(name string) string {
	return m.prefix(name)
}

// newGraphQLSourceTypeBuilder constructs an IRTypeBuilder configured
// for graphql ingest:
//   - All named types prefixed via the per-mirror prefix closure.
//   - Field names pass through verbatim (introspection already gives
//     graphql-conventional names).
//   - Custom scalars use identity serialize/parse (the gateway
//     forwards values verbatim; the upstream applies its own
//     coercion).
//   - JSON scalar shared across per-source builders — graphql-go
//     forbids two scalars sharing a Name in one schema.
func newGraphQLSourceTypeBuilder(svc *ir.Service, prefix func(string) string, jsonScalar *graphql.Scalar) *IRTypeBuilder {
	naming := IRTypeNaming{
		ObjectName:     prefix,
		InputName:      prefix,
		EnumName:       prefix,
		UnionName:      prefix,
		InterfaceName:  prefix,
		ScalarName:     prefix,
		FieldName:      identityName,
		EnumValueName:  identityName,
		EnumValueValue: func(v ir.EnumValue) any { return v.Name },
	}
	return NewIRTypeBuilder(svc, naming, IRTypeBuilderOptions{
		MapType:  jsonScalar,
		JSONType: jsonScalar,
	})
}

// introspectionToIRService converts the parsed introspection model
// into an ir.Service whose Types map has every named type the
// downstream schema declares — including namespace-shaped Objects.
// Skips the Operation/Group classification IngestGraphQL does for
// cross-kind rendering (which prunes namespace-container types);
// the runtime mirror walks introspection root fields directly and
// must be able to resolve every type those fields reach. Root types
// (Query / Mutation / Subscription) are excluded since they're
// schema scaffolding, not data shapes.
func introspectionToIRService(intro *introspectionSchema) *ir.Service {
	svc := &ir.Service{Types: map[string]*ir.Type{}}
	rootNames := map[string]bool{}
	if n := intro.QueryTypeName; n != "" {
		rootNames[n] = true
	}
	if n := intro.MutationTypeName; n != "" {
		rootNames[n] = true
	}
	if n := intro.SubscriptionTypeName; n != "" {
		rootNames[n] = true
	}
	for name, t := range intro.Types {
		if rootNames[name] {
			continue
		}
		switch t.Kind {
		case "OBJECT":
			obj := &ir.Type{Name: name, TypeKind: ir.TypeObject, Description: t.Description, OriginKind: ir.KindGraphQL}
			for _, f := range t.Fields {
				obj.Fields = append(obj.Fields, introspectionFieldToIR(f))
			}
			svc.Types[name] = obj
		case "INPUT_OBJECT":
			obj := &ir.Type{Name: name, TypeKind: ir.TypeInput, Description: t.Description, OriginKind: ir.KindGraphQL}
			for _, f := range t.InputFields {
				obj.Fields = append(obj.Fields, introspectionInputToIR(f))
			}
			svc.Types[name] = obj
		case "ENUM":
			obj := &ir.Type{Name: name, TypeKind: ir.TypeEnum, Description: t.Description, OriginKind: ir.KindGraphQL}
			for i, ev := range t.EnumValues {
				obj.Enum = append(obj.Enum, ir.EnumValue{
					Name:        ev.Name,
					Number:      int32(i),
					Description: ev.Description,
				})
			}
			svc.Types[name] = obj
		case "UNION", "INTERFACE":
			kind := ir.TypeUnion
			if t.Kind == "INTERFACE" {
				kind = ir.TypeInterface
			}
			obj := &ir.Type{Name: name, TypeKind: kind, Description: t.Description, OriginKind: ir.KindGraphQL}
			for _, p := range t.PossibleTypes {
				if p == nil || p.Name == "" {
					continue
				}
				obj.Variants = append(obj.Variants, p.Name)
			}
			if kind == ir.TypeInterface {
				for _, f := range t.Fields {
					obj.Fields = append(obj.Fields, introspectionFieldToIR(f))
				}
			}
			svc.Types[name] = obj
		case "SCALAR":
			if isBuiltinScalar(name) {
				continue
			}
			svc.Types[name] = &ir.Type{
				Name:        name,
				TypeKind:    ir.TypeScalar,
				Description: t.Description,
				OriginKind:  ir.KindGraphQL,
			}
		}
	}
	return svc
}

func introspectionFieldToIR(f *introspectionField) *ir.Field {
	out := &ir.Field{
		Name:        f.Name,
		JSONName:    f.Name,
		Description: f.Description,
		OneofIndex:  -1,
	}
	if ref := introspectionTypeRefToIR(f.Type); ref != nil {
		out.Type = *ref
	}
	out.Repeated, out.Required, out.ItemRequired = wrapperFlags(f.Type)
	return out
}

func introspectionInputToIR(f *introspectionInputV) *ir.Field {
	out := &ir.Field{
		Name:       f.Name,
		JSONName:   f.Name,
		OneofIndex: -1,
	}
	if ref := introspectionTypeRefToIR(f.Type); ref != nil {
		out.Type = *ref
	}
	out.Repeated, out.Required, out.ItemRequired = wrapperFlags(f.Type)
	return out
}

// introspectionTypeRefToIR walks past wrapper kinds and returns the
// leaf TypeRef. Wrapper flags are extracted separately via
// wrapperFlags so the IR Field's Repeated/Required/ItemRequired align
// with how IRTypeBuilder applies them.
func introspectionTypeRefToIR(r *introspectionTypeRef) *ir.TypeRef {
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
		return &ir.TypeRef{Builtin: ir.ScalarString}
	case "Int":
		return &ir.TypeRef{Builtin: ir.ScalarInt32}
	case "Float":
		return &ir.TypeRef{Builtin: ir.ScalarDouble}
	case "Boolean":
		return &ir.TypeRef{Builtin: ir.ScalarBool}
	case "ID":
		return &ir.TypeRef{Builtin: ir.ScalarID}
	}
	return &ir.TypeRef{Named: cur.Name}
}

// wrapperFlags inspects the wrapper chain. The introspection model
// allows arbitrary nesting (e.g. [[T]]); IR's flag triple captures
// only the common single-list shape. Nested lists collapse to
// repeated=true with itemRequired derived from the outermost LIST's
// inner NON_NULL — same as the existing mirror's coverage.
func wrapperFlags(r *introspectionTypeRef) (repeated, required, itemRequired bool) {
	if r == nil {
		return
	}
	if r.Kind == "NON_NULL" {
		required = true
		r = r.OfType
	}
	if r != nil && r.Kind == "LIST" {
		repeated = true
		inner := r.OfType
		if inner != nil && inner.Kind == "NON_NULL" {
			itemRequired = true
		}
	}
	return
}

// build emits (queries, mutations, subscriptions) keyed by
// namespace-prefixed field names. Each top-level field's Resolve
// (queries / mutations) or Subscribe (subscriptions) forwards to the
// remote.
func (m *graphQLMirror) build() (graphql.Fields, graphql.Fields, graphql.Fields, error) {
	intro := m.src.introspection
	queries, err := m.buildRootFields(intro.QueryTypeName, "query")
	if err != nil {
		return nil, nil, nil, err
	}
	mutations, err := m.buildRootFields(intro.MutationTypeName, "mutation")
	if err != nil {
		return nil, nil, nil, err
	}
	subscriptions, err := m.buildSubscriptionRootFields(intro.SubscriptionTypeName)
	if err != nil {
		return nil, nil, nil, err
	}
	return queries, mutations, subscriptions, nil
}

func (m *graphQLMirror) buildRootFields(rootName, opType string) (graphql.Fields, error) {
	if rootName == "" {
		return graphql.Fields{}, nil
	}
	root := m.src.introspection.Types[rootName]
	if root == nil {
		return nil, fmt.Errorf("root type %q missing from introspection", rootName)
	}
	out := graphql.Fields{}
	for _, f := range root.Fields {
		typ, err := m.outputType(f.Type)
		if err != nil {
			log.Printf("graphql ingest %s: skipping field %s: %v", m.src.namespace, f.Name, err)
			continue
		}
		args, err := m.argsConfig(f.Args)
		if err != nil {
			log.Printf("graphql ingest %s: skipping field %s args: %v", m.src.namespace, f.Name, err)
			continue
		}
		fieldName := m.prefix(f.Name)
		remote := f.Name // unprefixed name on the remote
		field := &graphql.Field{
			Type:    typ,
			Args:    args,
			Resolve: m.forwardingResolver(remote, opType),
		}
		if !m.isLatest {
			field.DeprecationReason = m.deprecationReason
		}
		out[fieldName] = field
	}
	return out, nil
}

// buildSubscriptionRootFields walks the upstream's Subscription type
// and emits a `<namespace>_<remoteName>` field per remote subscription.
// Subscribe opens an upstream graphql-transport-ws and pumps frames
// into graphql-go's subscribe channel; Resolve picks the remote field
// out of each `next` payload's data envelope so the local consumer
// sees the field's value directly (matching the query/mutation
// forwarder's shape).
func (m *graphQLMirror) buildSubscriptionRootFields(rootName string) (graphql.Fields, error) {
	if rootName == "" {
		return graphql.Fields{}, nil
	}
	root := m.src.introspection.Types[rootName]
	if root == nil {
		return nil, fmt.Errorf("subscription root type %q missing from introspection", rootName)
	}
	out := graphql.Fields{}
	for _, f := range root.Fields {
		typ, err := m.outputType(f.Type)
		if err != nil {
			log.Printf("graphql ingest %s: skipping subscription %s: %v", m.src.namespace, f.Name, err)
			continue
		}
		args, err := m.argsConfig(f.Args)
		if err != nil {
			log.Printf("graphql ingest %s: skipping subscription %s args: %v", m.src.namespace, f.Name, err)
			continue
		}
		fieldName := m.prefix(f.Name)
		remote := f.Name
		field := &graphql.Field{
			Type:      typ,
			Args:      args,
			Subscribe: m.subscribingResolver(remote),
			Resolve: func(rp graphql.ResolveParams) (any, error) {
				// rp.Source is the per-frame map[string]any the
				// Subscribe channel emitted; pluck the remote field's
				// value so consumers see it directly.
				if m, ok := rp.Source.(map[string]any); ok {
					return m[remote], nil
				}
				return rp.Source, nil
			},
		}
		if !m.isLatest {
			field.DeprecationReason = m.deprecationReason
		}
		out[fieldName] = field
	}
	return out, nil
}

func (m *graphQLMirror) prefix(name string) string {
	if m.isLatest {
		return m.src.namespace + "_" + name
	}
	return m.src.namespace + "_" + m.src.version + "_" + name
}

// shouldPrefix names — built-in scalars stay unprefixed so codegen
// works out of the box. Everything else (objects, enums, inputs,
// custom scalars) gets `<ns>_` so collisions across services can't
// happen.
func isBuiltinScalar(name string) bool {
	switch name {
	case "String", "Int", "Float", "Boolean", "ID":
		return true
	}
	return false
}

// outputType walks a typeRef chain (NON_NULL/LIST wrappers + final
// named type) and returns the corresponding graphql-go type.
func (m *graphQLMirror) outputType(ref *introspectionTypeRef) (graphql.Output, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil type ref")
	}
	switch ref.Kind {
	case "NON_NULL":
		inner, err := m.outputType(ref.OfType)
		if err != nil {
			return nil, err
		}
		return graphql.NewNonNull(inner), nil
	case "LIST":
		inner, err := m.outputType(ref.OfType)
		if err != nil {
			return nil, err
		}
		return graphql.NewList(inner), nil
	}
	return m.namedOutput(ref.Name)
}

func (m *graphQLMirror) namedOutput(name string) (graphql.Output, error) {
	if isBuiltinScalar(name) {
		return graphqlBuiltinScalar(name), nil
	}
	if _, ok := m.src.introspection.Types[name]; !ok {
		return nil, fmt.Errorf("type %q missing from introspection", name)
	}
	return m.tb.Output(ir.TypeRef{Named: name}, false, false, false)
}

func (m *graphQLMirror) inputType(ref *introspectionTypeRef) (graphql.Input, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil input type ref")
	}
	switch ref.Kind {
	case "NON_NULL":
		inner, err := m.inputType(ref.OfType)
		if err != nil {
			return nil, err
		}
		return graphql.NewNonNull(inner), nil
	case "LIST":
		inner, err := m.inputType(ref.OfType)
		if err != nil {
			return nil, err
		}
		return graphql.NewList(inner), nil
	}
	if isBuiltinScalar(ref.Name) {
		return graphqlBuiltinScalar(ref.Name), nil
	}
	if _, ok := m.src.introspection.Types[ref.Name]; !ok {
		return nil, fmt.Errorf("input type %q missing", ref.Name)
	}
	return m.tb.Input(ir.TypeRef{Named: ref.Name}, false, false, false)
}

func (m *graphQLMirror) argsConfig(args []*introspectionInputV) (graphql.FieldConfigArgument, error) {
	out := graphql.FieldConfigArgument{}
	for _, a := range args {
		typ, err := m.inputType(a.Type)
		if err != nil {
			return nil, err
		}
		out[a.Name] = &graphql.ArgumentConfig{Type: typ, Description: a.Description}
	}
	return out, nil
}

// forwardingResolver is the resolver attached to every top-level
// Query / Mutation field on a downstream graphQLSource. It
// reconstructs the GraphQL operation by:
//
//  1. taking rp.Info.FieldASTs[0] (this field's AST node),
//  2. rewriting its Name to drop the namespace prefix,
//  3. replacing every Name in the selection tree below with the
//     remote-side name (i.e. unprefix when a prefix is present),
//  4. printing the AST back to a query string,
//  5. wrapping in a `query { ... }` or `mutation { ... }` shell,
//  6. POSTing to the remote with rp.Info.VariableValues forwarded.
//
// The result for THIS field comes back in `data.<remoteName>`.
//
// opLabel is "query" or "mutation" — the GraphQL operation type this
// field lives under. Combined with remoteFieldName it forms the
// dispatch metric's `method` label so operators can slice by
// downstream operation the same way OpenAPI dispatch slices by
// `<HTTP_METHOD> <pathTemplate>`.
func (m *graphQLMirror) forwardingResolver(remoteFieldName, opLabel string) graphql.FieldResolveFn {
	core := newGraphQLDispatcher(m, remoteFieldName, opLabel, m.metrics)
	dispatcher := BackpressureMiddleware(graphQLBackpressureConfig(m.src, core.label, m.metrics, m.bp))(core)
	sid := ir.MakeSchemaID(m.src.namespace, m.src.version, opLabel+"_"+remoteFieldName)
	if m.dispatchers != nil {
		m.dispatchers.Set(sid, dispatcher)
	}
	return func(rp graphql.ResolveParams) (any, error) {
		ctx := withGraphQLForwardInfo(rp.Context, &rp.Info)
		if m.dispatchers != nil {
			if d := m.dispatchers.Get(sid); d != nil {
				return d.Dispatch(ctx, rp.Args)
			}
		}
		return dispatcher.Dispatch(ctx, rp.Args)
	}
}

// subscribingResolver is the Subscribe-side counterpart of
// forwardingResolver. It rebuilds the upstream operation, opens a
// graphql-transport-ws connection (one per local subscriber for v1 —
// multiplexing is a tier-3 follow-up), and pumps the upstream's
// `next` payloads into a Go channel that graphql-go drains.
//
// Errors and `complete` from the upstream terminate the local
// subscription with the same shape graphql-go's runSubscription
// expects.
func (m *graphQLMirror) subscribingResolver(remoteFieldName string) graphql.FieldResolveFn {
	src := m.src
	return func(rp graphql.ResolveParams) (any, error) {
		if len(rp.Info.FieldASTs) == 0 {
			return nil, fmt.Errorf("graphql ingest: no AST for subscription %s", remoteFieldName)
		}
		field := rp.Info.FieldASTs[0]
		rewritten := m.rewriteFieldForRemote(field, remoteFieldName)
		var varDefs []*ast.VariableDefinition
		if op, ok := rp.Info.Operation.(*ast.OperationDefinition); ok && op != nil {
			varDefs = op.VariableDefinitions
		}
		opDef := ast.NewOperationDefinition(&ast.OperationDefinition{
			Operation:           ast.OperationTypeSubscription,
			VariableDefinitions: varDefs,
			SelectionSet: ast.NewSelectionSet(&ast.SelectionSet{
				Selections: []ast.Selection{rewritten},
			}),
		})
		doc := ast.NewDocument(&ast.Document{Definitions: []ast.Node{opDef}})
		raw := printer.Print(doc)
		printed, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("graphql ingest: printer returned %T (%v)", raw, raw)
		}

		r := src.pickReplica()
		if r == nil {
			return nil, fmt.Errorf("graphql ingest: no live replicas for %s", src.namespace)
		}
		broker := src.getSubBroker()
		upstream, release, err := broker.acquire(rp.Context, r.endpoint, printed, rp.Info.VariableValues, src.forwardHeaders)
		if err != nil {
			return nil, err
		}
		// Translate upstream frames into a graphql-go Subscribe channel:
		// emit map[string]any for `next`, close on `complete` / ctx.
		out := make(chan any, 8)
		go func() {
			defer close(out)
			defer release()
			for {
				select {
				case f, ok := <-upstream:
					if !ok {
						return
					}
					if f == nil {
						continue
					}
					if len(f.Errors) > 0 {
						select {
						case out <- fmt.Errorf("graphql remote: %s", f.Errors[0]):
						case <-rp.Context.Done():
							return
						}
						if f.Done {
							return
						}
						continue
					}
					if f.Result != nil {
						select {
						case out <- f.Result:
						case <-rp.Context.Done():
							return
						}
					}
					if f.Done {
						return
					}
				case <-rp.Context.Done():
					return
				}
			}
		}()
		return out, nil
	}
}

// rewriteFieldForRemote returns a clone of the AST field with its
// Name set to remoteName and any inline-fragment type-conditions
// in the selection tree un-prefixed (`<ns>_Cat` → `Cat`) so the
// remote sees its own type names. Nested field names pass through
// unchanged — the gateway only prefixes top-level names in the
// local schema. Argument lists and aliases pass through unchanged.
// Always uses ast.NewX constructors so the Kind fields the
// printer's visitor relies on are populated.
func (m *graphQLMirror) rewriteFieldForRemote(field *ast.Field, remoteName string) *ast.Field {
	out := ast.NewField(&ast.Field{
		Alias:      field.Alias,
		Name:       ast.NewName(&ast.Name{Value: remoteName}),
		Arguments:  field.Arguments,
		Directives: field.Directives,
	})
	if field.SelectionSet != nil {
		out.SelectionSet = m.rewriteSelectionSet(field.SelectionSet)
	}
	return out
}

func (m *graphQLMirror) rewriteSelectionSet(sel *ast.SelectionSet) *ast.SelectionSet {
	if sel == nil {
		return nil
	}
	out := ast.NewSelectionSet(&ast.SelectionSet{
		Selections: make([]ast.Selection, 0, len(sel.Selections)),
	})
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			// Nested fields: their names are remote-side already (the
			// gateway only prefixes top-level names). Re-build via
			// ast.NewField so the visitor sees a proper Kind, but
			// recurse so subselections come along.
			cloned := ast.NewField(&ast.Field{
				Alias:      n.Alias,
				Name:       n.Name,
				Arguments:  n.Arguments,
				Directives: n.Directives,
			})
			if n.SelectionSet != nil {
				cloned.SelectionSet = m.rewriteSelectionSet(n.SelectionSet)
			}
			out.Selections = append(out.Selections, cloned)
		case *ast.InlineFragment:
			// Inline fragments target a concrete variant of an abstract
			// type. The local TypeCondition carries the prefixed name
			// (e.g. `<ns>_Cat`); on the wire the remote expects its own
			// `Cat`. Recurse into the body so nested inline fragments /
			// inline subselections also un-prefix.
			frag := ast.NewInlineFragment(&ast.InlineFragment{
				Directives: n.Directives,
			})
			if n.TypeCondition != nil && n.TypeCondition.Name != nil {
				frag.TypeCondition = ast.NewNamed(&ast.Named{
					Name: ast.NewName(&ast.Name{Value: m.unprefixTypeName(n.TypeCondition.Name.Value)}),
				})
			}
			if n.SelectionSet != nil {
				frag.SelectionSet = m.rewriteSelectionSet(n.SelectionSet)
			}
			out.Selections = append(out.Selections, frag)
		default:
			// Fragment spreads (`...Foo`) pass through unchanged. v1
			// callers don't synthesise these locally — every selection
			// the gateway sees comes from rp.Info.FieldASTs which only
			// carries inline fragments. If a real client ever sends a
			// named fragment we'd need its definition forwarded too.
			out.Selections = append(out.Selections, s)
		}
	}
	return out
}

// unprefixTypeName strips the source's "<ns>_" or "<ns>_<vN>_" prefix
// from a local type name. Returns the name unchanged when there's no
// match — the rewriter is best-effort, and a non-prefixed name is
// either a built-in scalar or an introspection mismatch we forward
// verbatim.
func (m *graphQLMirror) unprefixTypeName(name string) string {
	if !m.isLatest {
		long := m.src.namespace + "_" + m.src.version + "_"
		if len(name) > len(long) && name[:len(long)] == long {
			return name[len(long):]
		}
	}
	short := m.src.namespace + "_"
	if len(name) > len(short) && name[:len(short)] == short {
		return name[len(short):]
	}
	return name
}

// newGraphQLIngestJSONScalar constructs the shared JSON scalar used
// as the IRTypeBuilder fallback for every per-source mirror in one
// schema build. graphql-go rejects two scalars sharing a Name; the
// scalar must therefore be built once and shared across builders.
func newGraphQLIngestJSONScalar() *graphql.Scalar {
	return graphql.NewScalar(graphql.ScalarConfig{
		Name:         "JSON",
		Description:  "Untyped JSON value (used as the graphql-ingest fallback for poorly-introspected abstract types).",
		Serialize:    func(v any) any { return v },
		ParseValue:   func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any { return v },
	})
}

// graphqlBuiltinScalar maps the standard scalar names to graphql-go's
// built-ins. Returns nil for unknown names; callers should validate
// with isBuiltinScalar first.
func graphqlBuiltinScalar(name string) *graphql.Scalar {
	switch name {
	case "String":
		return graphql.String
	case "Int":
		return graphql.Int
	case "Float":
		return graphql.Float
	case "Boolean":
		return graphql.Boolean
	case "ID":
		return graphql.ID
	}
	return nil
}

// jsonUnmarshalLoose decodes the upstream's `data` envelope into a
// generic map. Numbers stay as float64 — graphql-go's coerceInt
// does not match json.Number (typed-string), so UseNumber would
// silently null out every Int field on the response. ID values
// large enough to lose float64 precision should be carried as
// strings on the wire (which is the conventional GraphQL ID
// shape anyway).
func jsonUnmarshalLoose(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
