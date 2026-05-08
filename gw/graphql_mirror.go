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
// covered; Interfaces, Unions, and Subscription roots are skipped
// with a registration-time log line so operators see what's missing.
type graphQLMirror struct {
	src         *graphQLSource
	metrics     Metrics
	bp          BackpressureOptions
	dispatchers *ir.DispatchRegistry
	// isLatest controls whether this mirror's fields and types use
	// the bare "<ns>_" prefix (true, single-version or current) or
	// the disambiguated "<ns>_<vN>_" prefix (false, older version).
	// Older versions also stamp deprecationReason on every emitted
	// field so codegen consumers see "<latest> is current".
	isLatest          bool
	deprecationReason string
	objects           map[string]*graphql.Object
	inputs            map[string]*graphql.InputObject
	enums             map[string]*graphql.Enum
	scalars           map[string]*graphql.Scalar
	unions            map[string]*graphql.Union
}

func newGraphQLMirror(src *graphQLSource, metrics Metrics, bp BackpressureOptions, dispatchers *ir.DispatchRegistry) *graphQLMirror {
	if metrics == nil {
		metrics = noopMetrics{}
	}
	return &graphQLMirror{
		src:         src,
		metrics:     metrics,
		bp:          bp,
		dispatchers: dispatchers,
		isLatest:    true,
		objects:     map[string]*graphql.Object{},
		inputs:      map[string]*graphql.InputObject{},
		enums:       map[string]*graphql.Enum{},
		scalars:     map[string]*graphql.Scalar{},
		unions:      map[string]*graphql.Union{},
	}
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
	t, ok := m.src.introspection.Types[name]
	if !ok {
		return nil, fmt.Errorf("type %q missing from introspection", name)
	}
	switch t.Kind {
	case "OBJECT":
		return m.objectFor(t), nil
	case "ENUM":
		return m.enumFor(t), nil
	case "SCALAR":
		return m.scalarFor(t), nil
	case "INTERFACE", "UNION":
		// Both Interface and Union project to graphql.NewUnion in the
		// local schema — the gateway forwards the whole subselection
		// upstream, so the only thing the local schema needs to do at
		// dispatch time is pick the right concrete Object via
		// `__typename`. Carrying graphql.Interface (with shared fields)
		// would force every Object to declare `Interfaces:` *during
		// thunk-build*, which is a topological hassle without
		// pulling its weight for a forwarding role.
		return m.unionFor(t), nil
	}
	return nil, fmt.Errorf("unsupported type kind %q for %s", t.Kind, name)
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
	t, ok := m.src.introspection.Types[ref.Name]
	if !ok {
		return nil, fmt.Errorf("input type %q missing", ref.Name)
	}
	switch t.Kind {
	case "ENUM":
		return m.enumFor(t), nil
	case "INPUT_OBJECT":
		return m.inputObjectFor(t), nil
	case "SCALAR":
		return m.scalarFor(t), nil
	}
	return nil, fmt.Errorf("unsupported input type kind %q for %s", t.Kind, ref.Name)
}

func (m *graphQLMirror) objectFor(t *introspectionType) *graphql.Object {
	if obj, ok := m.objects[t.Name]; ok {
		return obj
	}
	name := m.prefix(t.Name)
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name:        name,
		Description: t.Description,
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			fields := graphql.Fields{}
			for _, f := range t.Fields {
				typ, err := m.outputType(f.Type)
				if err != nil {
					continue
				}
				args, _ := m.argsConfig(f.Args)
				// Nested fields rely on the default resolver
				// (rp.Source.(map[string]any)[fieldName]) since the
				// top-level forwarder fetches the whole subtree in
				// one POST.
				fields[f.Name] = &graphql.Field{
					Type:        typ,
					Args:        args,
					Description: f.Description,
				}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.Field{Type: graphql.String}
			}
			return fields
		}),
	})
	m.objects[t.Name] = obj
	return obj
}

func (m *graphQLMirror) inputObjectFor(t *introspectionType) *graphql.InputObject {
	if io, ok := m.inputs[t.Name]; ok {
		return io
	}
	name := m.prefix(t.Name)
	io := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:        name,
		Description: t.Description,
		Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
			fields := graphql.InputObjectConfigFieldMap{}
			for _, f := range t.InputFields {
				typ, err := m.inputType(f.Type)
				if err != nil {
					continue
				}
				fields[f.Name] = &graphql.InputObjectFieldConfig{
					Type:        typ,
					Description: f.Description,
				}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.InputObjectFieldConfig{Type: graphql.String}
			}
			return fields
		}),
	})
	m.inputs[t.Name] = io
	return io
}

func (m *graphQLMirror) enumFor(t *introspectionType) *graphql.Enum {
	if e, ok := m.enums[t.Name]; ok {
		return e
	}
	values := graphql.EnumValueConfigMap{}
	for _, v := range t.EnumValues {
		values[v.Name] = &graphql.EnumValueConfig{
			Value:       v.Name, // remote sees the name on the wire
			Description: v.Description,
		}
	}
	e := graphql.NewEnum(graphql.EnumConfig{
		Name:        m.prefix(t.Name),
		Description: t.Description,
		Values:      values,
	})
	m.enums[t.Name] = e
	return e
}

func (m *graphQLMirror) scalarFor(t *introspectionType) *graphql.Scalar {
	if s, ok := m.scalars[t.Name]; ok {
		return s
	}
	s := graphql.NewScalar(graphql.ScalarConfig{
		Name:        m.prefix(t.Name),
		Description: t.Description,
		Serialize:   func(v any) any { return v },
		ParseValue:  func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any {
			if sv, ok := v.(*ast.StringValue); ok {
				return sv.Value
			}
			return nil
		},
	})
	m.scalars[t.Name] = s
	return s
}

// unionFor projects an INTERFACE or UNION into a graphql.NewUnion over
// the prefixed Object types from the introspected `possibleTypes`.
// ResolveType reads `__typename` off the per-value map (clients are
// expected to select it under any abstract type — the gateway does
// not auto-inject it).
//
// If `possibleTypes` is empty (e.g. a poorly-introspected upstream)
// or no variant resolves to an Object the mirror has built, fall
// back to a JSON-scalar mirror so the field still surfaces — same
// shape the v1 fallback used.
func (m *graphQLMirror) unionFor(t *introspectionType) graphql.Output {
	if u, ok := m.unions[t.Name]; ok {
		return u
	}
	types := []*graphql.Object{}
	byRemoteName := map[string]*graphql.Object{}
	for _, p := range t.PossibleTypes {
		if p == nil || p.Name == "" {
			continue
		}
		concrete, ok := m.src.introspection.Types[p.Name]
		if !ok || concrete.Kind != "OBJECT" {
			continue
		}
		obj := m.objectFor(concrete)
		types = append(types, obj)
		byRemoteName[p.Name] = obj
	}
	if len(types) == 0 {
		log.Printf("graphql ingest %s: %s %s has no resolvable possibleTypes — falling back to JSON scalar",
			m.src.namespace, t.Kind, t.Name)
		return m.scalarFor(t)
	}
	u := graphql.NewUnion(graphql.UnionConfig{
		Name:        m.prefix(t.Name),
		Description: t.Description,
		Types:       types,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if v, ok := p.Value.(map[string]any); ok {
				if name, ok := v["__typename"].(string); ok {
					if obj, ok := byRemoteName[name]; ok {
						return obj
					}
				}
			}
			return nil
		},
	})
	m.unions[t.Name] = u
	return u
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
