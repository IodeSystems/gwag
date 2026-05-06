package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"
)

// graphQLMirror walks the introspection model of one downstream
// service and produces graphql-go types prefixed with the source's
// namespace. The Object/Scalar/Enum/InputObject/List/NonNull subset is
// covered; Interfaces, Unions, and Subscription roots are skipped
// with a registration-time log line so operators see what's missing.
type graphQLMirror struct {
	src     *graphQLSource
	metrics Metrics
	bp      BackpressureOptions
	objects map[string]*graphql.Object
	inputs  map[string]*graphql.InputObject
	enums   map[string]*graphql.Enum
	scalars map[string]*graphql.Scalar
}

func newGraphQLMirror(src *graphQLSource, metrics Metrics, bp BackpressureOptions) *graphQLMirror {
	if metrics == nil {
		metrics = noopMetrics{}
	}
	return &graphQLMirror{
		src:     src,
		metrics: metrics,
		bp:      bp,
		objects: map[string]*graphql.Object{},
		inputs:  map[string]*graphql.InputObject{},
		enums:   map[string]*graphql.Enum{},
		scalars: map[string]*graphql.Scalar{},
	}
}

// build emits (queries, mutations) keyed by namespace-prefixed field
// names. Each top-level field's Resolve forwards to the remote.
func (m *graphQLMirror) build() (graphql.Fields, graphql.Fields, error) {
	intro := m.src.introspection
	if intro.SubscriptionTypeName != "" {
		log.Printf("graphql ingest %s: skipping Subscription root (not yet supported)", m.src.namespace)
	}

	queries, err := m.buildRootFields(intro.QueryTypeName, "query")
	if err != nil {
		return nil, nil, err
	}
	mutations, err := m.buildRootFields(intro.MutationTypeName, "mutation")
	if err != nil {
		return nil, nil, err
	}
	return queries, mutations, nil
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
		out[fieldName] = &graphql.Field{
			Type:    typ,
			Args:    args,
			Resolve: m.forwardingResolver(remote, opType),
		}
	}
	return out, nil
}

func (m *graphQLMirror) prefix(name string) string {
	return m.src.namespace + "_" + name
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
		// v1 fallback: serialise as JSON. Lossy but lets the field
		// exist in the schema instead of erroring out the whole build.
		return m.jsonScalarFor(t), nil
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

// jsonScalarFor is the catch-all for Interface/Union — emit a passthrough
// JSON scalar so the field type still resolves at schema-build time.
func (m *graphQLMirror) jsonScalarFor(t *introspectionType) *graphql.Scalar {
	log.Printf("graphql ingest %s: %s %s falls back to JSON scalar (Interface/Union not yet typed)", m.src.namespace, t.Kind, t.Name)
	return m.scalarFor(t)
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
	src := m.src
	metrics := m.metrics
	bp := m.bp
	ns := src.namespace
	methodLabel := opLabel + " " + remoteFieldName
	return func(rp graphql.ResolveParams) (any, error) {
		start := time.Now()
		record := func(err error) error {
			metrics.RecordDispatch(ns, "v1", methodLabel, time.Since(start), err)
			return err
		}
		// Acquire a slot. Fast path: src.sem nil (unbounded) or has
		// immediate capacity. Slow path: queue, observe dwell, time out
		// per MaxWaitTime. Mirrors the OpenAPI resolver so all three
		// dispatch paths share the same backpressure surface.
		if src.sem != nil {
			waitStart := time.Now()
			select {
			case src.sem <- struct{}{}:
				metrics.RecordDwell(ns, "v1", methodLabel, "unary", time.Since(waitStart))
			default:
				depth := int(src.queueing.Add(1))
				metrics.SetQueueDepth(ns, "v1", "unary", depth)
				dwell, err := waitForSlot(rp.Context, src.sem, bp.MaxWaitTime)
				now := int(src.queueing.Add(-1))
				metrics.SetQueueDepth(ns, "v1", "unary", now)
				metrics.RecordDwell(ns, "v1", methodLabel, "unary", dwell)
				if err != nil {
					metrics.RecordBackoff(ns, "v1", methodLabel, "unary", "wait_timeout")
					return nil, record(Reject(CodeResourceExhausted, fmt.Sprintf("%s: %s", ns, err.Error())))
				}
			}
			defer func() { <-src.sem }()
		}
		if len(rp.Info.FieldASTs) == 0 {
			return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: no AST for %s", remoteFieldName)))
		}
		field := rp.Info.FieldASTs[0]
		rewritten := rewriteFieldForRemote(field, remoteFieldName)
		opType := ast.OperationTypeQuery
		var varDefs []*ast.VariableDefinition
		if op, ok := rp.Info.Operation.(*ast.OperationDefinition); ok && op != nil {
			if op.Operation == ast.OperationTypeMutation {
				opType = ast.OperationTypeMutation
			}
			varDefs = op.VariableDefinitions
		}
		opDef := ast.NewOperationDefinition(&ast.OperationDefinition{
			Operation:           opType,
			VariableDefinitions: varDefs,
			SelectionSet: ast.NewSelectionSet(&ast.SelectionSet{
				Selections: []ast.Selection{rewritten},
			}),
		})
		doc := ast.NewDocument(&ast.Document{Definitions: []ast.Node{opDef}})
		raw := printer.Print(doc)
		printed, ok := raw.(string)
		if !ok {
			return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: printer returned %T (%v)", raw, raw)))
		}
		query := printed

		resp, err := dispatchGraphQL(rp.Context, src.httpClient, src.endpoint, query, rp.Info.VariableValues, src.forwardHeaders)
		if err != nil {
			return nil, record(err)
		}
		if len(resp.Errors) > 0 {
			// Surface the first error back to the local client so they
			// see what the remote complained about. Application-level
			// remote errors classify as INTERNAL — there's no portable
			// status code in the GraphQL error envelope.
			return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql remote: %s", resp.Errors[0])))
		}
		var data map[string]any
		if len(resp.Data) > 0 {
			if err := jsonUnmarshalLoose(resp.Data, &data); err != nil {
				return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql decode data: %s", err.Error())))
			}
		}
		record(nil)
		return data[remoteFieldName], nil
	}
}

// rewriteFieldForRemote returns a clone of the AST field with its
// Name set to remoteName and all nested field names un-prefixed
// where the prefix matches. Argument lists and aliases pass through
// unchanged. Always uses ast.NewX constructors so the Kind fields
// the printer's visitor relies on are populated.
func rewriteFieldForRemote(field *ast.Field, remoteName string) *ast.Field {
	out := ast.NewField(&ast.Field{
		Alias:      field.Alias,
		Name:       ast.NewName(&ast.Name{Value: remoteName}),
		Arguments:  field.Arguments,
		Directives: field.Directives,
	})
	if field.SelectionSet != nil {
		out.SelectionSet = rewriteSelectionSet(field.SelectionSet, remoteName)
	}
	return out
}

func rewriteSelectionSet(sel *ast.SelectionSet, parentRemote string) *ast.SelectionSet {
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
				cloned.SelectionSet = rewriteSelectionSet(n.SelectionSet, parentRemote)
			}
			out.Selections = append(out.Selections, cloned)
		default:
			// Inline fragments & fragment spreads pass through verbatim.
			out.Selections = append(out.Selections, s)
		}
	}
	return out
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

// jsonUnmarshalLoose is encoding/json.Unmarshal with the
// number-as-json.Number behavior so large IDs don't lose precision.
func jsonUnmarshalLoose(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
