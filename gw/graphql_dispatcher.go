package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/printer"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// graphQLForwardInfoKey carries the resolver's ResolveInfo (FieldASTs +
// Operation + VariableValues) into the dispatcher. GraphQL→GraphQL
// forwarding can't reconstruct an equivalent upstream query from
// canonical args alone — it needs the user's selection-set so
// nested subselections forward verbatim. The other ingest paths
// (HTTP/JSON, gRPC) won't have this; the dispatcher falls back to a
// pre-printed canonicalQuery synthesized at construction time, when
// available.
type graphQLForwardInfoKey struct{}

func withGraphQLForwardInfo(ctx context.Context, info *graphql.ResolveInfo) context.Context {
	return context.WithValue(ctx, graphQLForwardInfoKey{}, info)
}

func graphQLForwardInfoFrom(ctx context.Context) *graphql.ResolveInfo {
	if v, _ := ctx.Value(graphQLForwardInfoKey{}).(*graphql.ResolveInfo); v != nil {
		return v
	}
	return ir.GraphQLResolveInfoFrom(ctx)
}

// chainArgsKey marks a dispatch that comes out of the cross-format
// runtime chain (wrapCanonicalDispatcherWithChain). The chain may
// have changed the operation's arguments, so the canonical args map,
// not the caller's AST, holds their values. The caller's selection
// still decides the response shape: an OpenAPI dispatcher projects
// onto it, and a GraphQL dispatcher forwards it with the top-level
// arguments bound to the post-chain values.
type chainArgsKey struct{}

func withChainArgs(ctx context.Context) context.Context {
	return context.WithValue(ctx, chainArgsKey{}, true)
}

func chainArgsFrom(ctx context.Context) bool {
	v, _ := ctx.Value(chainArgsKey{}).(bool)
	return v
}

// graphQLDispatcher implements ir.Dispatcher for one downstream
// GraphQL source + one mirrored field. Everything inside the
// pre-cutover forwardingResolver closure (AST rewrite, doc print,
// pickReplica, dispatchGraphQL, response decode) lives here;
// backpressureMiddleware wraps the outside.
//
// canonicalQuery + canonicalArgNames are populated at construction
// for top-level (non-grouped, non-subscription) ops. They drive the
// canonical-args dispatch path used when no graphQLForwardInfo is in
// context (HTTP/JSON or gRPC ingress, where there is no caller AST).
// Grouped ops are skipped because the upstream's namespace-shaped
// nesting can't be re-synthesized from a leaf op alone.
type graphQLDispatcher struct {
	mirror          *graphQLMirror
	remoteFieldName string
	opLabel         string // "query" / "mutation" / "subscription"
	metrics         Metrics
	ns              string
	ver             string
	label           string // "<opLabel> <remoteFieldName>"
	args            []*ir.Arg

	canonicalQuery    string
	canonicalArgNames []string

	printed forwardPrintCache
}

// forwardQuery prints the upstream query for this field. Printing
// walks the AST through graphql-go's visitor, which was ~80% of the
// allocations on a forwarded request. The plan cache keeps parsed
// documents, so the same query reaches here with the same AST nodes:
// the printed text is cached by (field, operation) identity.
//
// chainArgs: bind the top-level arguments to variables filled from
// the canonical args map (see chainArgsKey) instead of forwarding
// the caller's argument ASTs.
func (d *graphQLDispatcher) forwardQuery(info *graphql.ResolveInfo, chainArgs bool) (*forwardPrinted, error) {
	key := forwardKey{field: info.FieldASTs[0], op: info.Operation, chainArgs: chainArgs}
	if p, ok := d.printed.get(key); ok {
		return p, nil
	}
	opType := ast.OperationTypeQuery
	if op, ok := info.Operation.(*ast.OperationDefinition); ok && op != nil && op.Operation == ast.OperationTypeMutation {
		opType = ast.OperationTypeMutation
	}
	rewritten := d.mirror.rewriteFieldForRemote(info.FieldASTs[0], d.remoteFieldName, info.FieldASTs[1:], info.Fragments)
	var bind []*ir.Arg
	if chainArgs {
		bind = d.args
	}
	p, err := printForward(opType, rewritten, info.Operation, chainArgs, bind)
	if err != nil {
		return nil, err
	}
	d.printed.put(key, p)
	return p, nil
}

// forwardVars returns the variables for a forwarded query: the
// caller's on the plain path, and on the runtime-chain path the
// caller variables the query still references plus the post-chain
// argument values.
func forwardVars(p *forwardPrinted, info *graphql.ResolveInfo, chainArgs bool, args map[string]any) map[string]any {
	if !chainArgs {
		return info.VariableValues
	}
	vars := make(map[string]any, len(p.callerVars)+len(p.argVars))
	for _, n := range p.callerVars {
		if v, ok := info.VariableValues[n]; ok {
			vars[n] = v
		}
	}
	for _, av := range p.argVars {
		if v, ok := args[av.arg]; ok {
			vars[av.varName] = v
		}
	}
	return vars
}

// forwardPrinted is one printed upstream operation.
type forwardPrinted struct {
	query string
	// callerVars are the caller's variables the query references.
	callerVars []string
	// argVars binds each top-level argument to the upstream variable
	// that carries it. Set only when the arguments come from the
	// canonical args map.
	argVars []forwardArgVar
}

type forwardArgVar struct{ arg, varName string }

// printForward prints `field` as the single root field of an upstream
// operation of type opType. Variable definitions are the caller's
// (from op) that field references: a variable the caller defined for
// another root field, or for a directive the gateway evaluated, would
// make the upstream reject the query (§5.8.4 "All Variables Used").
//
// chainArgs replaces field's arguments with one variable per entry in
// bind, named after the argument unless that name collides with a
// caller variable the selection still uses.
func printForward(opType string, field *ast.Field, op ast.Node, chainArgs bool, bind []*ir.Arg) (*forwardPrinted, error) {
	p := &forwardPrinted{}
	var synth []*ast.VariableDefinition
	if chainArgs {
		field.Arguments = nil
		p.callerVars = fieldVarRefs(nil, field)
		field.Arguments = make([]*ast.Argument, 0, len(bind))
		taken := append([]string(nil), p.callerVars...)
		for _, a := range bind {
			name := a.Name
			for containsString(taken, name) {
				name = "gw_" + name
			}
			taken = append(taken, name)
			p.argVars = append(p.argVars, forwardArgVar{arg: a.Name, varName: name})
			v := ast.NewVariable(&ast.Variable{Name: ast.NewName(&ast.Name{Value: name})})
			field.Arguments = append(field.Arguments, ast.NewArgument(&ast.Argument{
				Name:  ast.NewName(&ast.Name{Value: a.Name}),
				Value: v,
			}))
			synth = append(synth, ast.NewVariableDefinition(&ast.VariableDefinition{
				Variable: v,
				Type:     irArgASTType(a),
			}))
		}
	} else {
		p.callerVars = fieldVarRefs(nil, field)
	}
	var varDefs []*ast.VariableDefinition
	if o, ok := op.(*ast.OperationDefinition); ok && o != nil {
		for _, vd := range o.VariableDefinitions {
			if vd != nil && vd.Variable != nil && vd.Variable.Name != nil && containsString(p.callerVars, vd.Variable.Name.Value) {
				varDefs = append(varDefs, vd)
			}
		}
	}
	varDefs = append(varDefs, synth...)
	opDef := ast.NewOperationDefinition(&ast.OperationDefinition{
		Operation:           opType,
		VariableDefinitions: varDefs,
		SelectionSet: ast.NewSelectionSet(&ast.SelectionSet{
			Selections: []ast.Selection{field},
		}),
	})
	doc := ast.NewDocument(&ast.Document{Definitions: []ast.Node{opDef}})
	raw := printer.Print(doc)
	q, ok := raw.(string)
	if !ok {
		return nil, Reject(CodeInternal, fmt.Sprintf("graphql ingest: printer returned %T (%v)", raw, raw))
	}
	p.query = q
	return p, nil
}

// fieldVarRefs appends the names of the variables f references in
// its arguments, directives and selection, once each.
func fieldVarRefs(dst []string, f *ast.Field) []string {
	for _, a := range f.Arguments {
		dst = valueVarRefs(dst, a.Value)
	}
	dst = directiveVarRefs(dst, f.Directives)
	return selectionVarRefs(dst, f.SelectionSet)
}

func selectionVarRefs(dst []string, sel *ast.SelectionSet) []string {
	if sel == nil {
		return dst
	}
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			dst = fieldVarRefs(dst, n)
		case *ast.InlineFragment:
			dst = directiveVarRefs(dst, n.Directives)
			dst = selectionVarRefs(dst, n.SelectionSet)
		case *ast.FragmentSpread:
			dst = directiveVarRefs(dst, n.Directives)
		}
	}
	return dst
}

func directiveVarRefs(dst []string, dirs []*ast.Directive) []string {
	for _, d := range dirs {
		if d == nil {
			continue
		}
		for _, a := range d.Arguments {
			dst = valueVarRefs(dst, a.Value)
		}
	}
	return dst
}

func valueVarRefs(dst []string, v ast.Value) []string {
	switch n := v.(type) {
	case *ast.Variable:
		if n.Name != nil && !containsString(dst, n.Name.Value) {
			dst = append(dst, n.Name.Value)
		}
	case *ast.ListValue:
		for _, e := range n.Values {
			dst = valueVarRefs(dst, e)
		}
	case *ast.ObjectValue:
		for _, f := range n.Fields {
			dst = valueVarRefs(dst, f.Value)
		}
	}
	return dst
}

// irArgASTType is graphQLArgTypeString as an AST type node.
func irArgASTType(a *ir.Arg) ast.Type {
	var t ast.Type = irTypeRefAST(a.Type)
	if a.Repeated {
		if a.ItemRequired {
			t = ast.NewNonNull(&ast.NonNull{Type: t})
		}
		t = ast.NewList(&ast.List{Type: t})
	}
	if a.Required {
		t = ast.NewNonNull(&ast.NonNull{Type: t})
	}
	return t
}

func irTypeRefAST(r ir.TypeRef) ast.Type {
	if r.IsList() {
		var elem ast.Type = irTypeRefAST(r.List.Elem)
		if r.List.ItemRequired {
			elem = ast.NewNonNull(&ast.NonNull{Type: elem})
		}
		return ast.NewList(&ast.List{Type: elem})
	}
	return ast.NewNamed(&ast.Named{Name: ast.NewName(&ast.Name{Value: graphQLTypeNameForRef(r)})})
}

// forwardKey identifies one field in one parsed operation. The
// operation is part of the key because a fragment's field node is
// shared by every operation that spreads it, with different variable
// definitions. chainArgs is part of it because that path prints
// different arguments for the same node.
type forwardKey struct {
	field     *ast.Field
	op        ast.Node
	chainArgs bool
}

// forwardPrintCacheMax bounds the cache. Keys hold AST nodes alive
// after the plan cache evicts their document, so the cache empties
// when full; with the plan cache off, every request is a miss.
const forwardPrintCacheMax = 256

type forwardPrintCache struct {
	mu sync.RWMutex
	m  map[forwardKey]*forwardPrinted
}

func (c *forwardPrintCache) get(k forwardKey) (*forwardPrinted, bool) {
	c.mu.RLock()
	p, ok := c.m[k]
	c.mu.RUnlock()
	return p, ok
}

func (c *forwardPrintCache) put(k forwardKey, p *forwardPrinted) {
	c.mu.Lock()
	if c.m == nil || len(c.m) >= forwardPrintCacheMax {
		c.m = make(map[forwardKey]*forwardPrinted)
	}
	c.m[k] = p
	c.mu.Unlock()
}

func newGraphQLDispatcher(m *graphQLMirror, op *ir.Operation, opLabel string, metrics Metrics, isGrouped bool) *graphQLDispatcher {
	d := &graphQLDispatcher{
		mirror:          m,
		remoteFieldName: op.Name,
		opLabel:         opLabel,
		metrics:         metrics,
		ns:              m.src.namespace,
		ver:             m.src.version,
		label:           opLabel + " " + op.Name,
		args:            op.Args,
	}
	if !isGrouped && op.Kind != ir.OpSubscription {
		d.canonicalQuery, d.canonicalArgNames = buildGraphQLCanonicalQuery(m, op, opLabel)
	}
	return d
}

func (d *graphQLDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	tr := tracerFromContext(ctx)
	ctx, span := tr.startDispatchSpan(ctx, "gateway.dispatch.graphql",
		namespaceAttr(d.ns),
		versionAttr(d.ver),
		methodAttr(d.label),
	)
	defer span.End()
	if d.opLabel == "subscription" {
		return d.dispatchSubscribe(ctx)
	}
	return d.dispatchUnary(ctx, args)
}

func (d *graphQLDispatcher) dispatchUnary(ctx context.Context, args map[string]any) (any, error) {
	start := time.Now()
	record := func(err error) error {
		elapsed := time.Since(start)
		d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, elapsed, err)
		addDispatchTime(ctx, elapsed)
		return err
	}

	printed, vars, err := d.upstreamRequest(ctx, args)
	if err != nil {
		return nil, record(err)
	}

	src := d.mirror.src
	resp, err := src.roundTrip(ctx, d.label, printed, vars)
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
	return data[d.remoteFieldName], nil
}

// upstreamRequest returns the upstream query text and variables for
// one dispatch. With the caller's AST in ctx it forwards the caller's
// selection; without one (HTTP/JSON or gRPC ingress) it falls back to
// the construction-time synthesized query, with variables read out
// of the canonical args map by IR Arg name (Op.Args index by name =
// upstream variable name).
func (d *graphQLDispatcher) upstreamRequest(ctx context.Context, args map[string]any) (string, map[string]any, error) {
	if info := graphQLForwardInfoFrom(ctx); info != nil && len(info.FieldASTs) > 0 {
		chainArgs := chainArgsFrom(ctx)
		p, err := d.forwardQuery(info, chainArgs)
		if err != nil {
			return "", nil, err
		}
		return p.query, forwardVars(p, info, chainArgs, args), nil
	}
	if d.canonicalQuery == "" {
		return "", nil, Reject(CodeInternal, fmt.Sprintf("graphql ingest: no AST for %s", d.remoteFieldName))
	}
	var vars map[string]any
	if len(d.canonicalArgNames) > 0 && len(args) > 0 {
		vars = make(map[string]any, len(d.canonicalArgNames))
		for _, name := range d.canonicalArgNames {
			if v, ok := args[name]; ok {
				vars[name] = v
			}
		}
	}
	return d.canonicalQuery, vars, nil
}

// DispatchAppend is the byte-splice variant of dispatchUnary. Same
// upstream call, but extracts the per-field JSON without recursing
// into nested objects (single-level map[string]json.RawMessage) and
// appends the bytes directly to dst. Subscriptions don't take this
// path — the renderer only installs ResolveAppend for unary fields.
//
// The append path captures most of the per-request alloc reclaim
// from the projected wedge: ~970 allocs → ~200 on
// BenchmarkProtoSchemaExec by skipping the full-tree map walk that
// Dispatch performs.
func (d *graphQLDispatcher) DispatchAppend(ctx context.Context, args map[string]any, dst []byte) ([]byte, error) {
	tr := tracerFromContext(ctx)
	ctx, span := tr.startDispatchSpan(ctx, "gateway.dispatch.graphql",
		namespaceAttr(d.ns),
		versionAttr(d.ver),
		methodAttr(d.label),
	)
	defer span.End()
	if d.opLabel == "subscription" {
		return dst, fmt.Errorf("graphql ingest: DispatchAppend not supported for subscriptions")
	}

	start := time.Now()
	record := func(err error) error {
		elapsed := time.Since(start)
		d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, elapsed, err)
		addDispatchTime(ctx, elapsed)
		return err
	}

	printed, vars, err := d.upstreamRequest(ctx, args)
	if err != nil {
		return dst, record(err)
	}

	src := d.mirror.src
	resp, err := src.roundTrip(ctx, d.label, printed, vars)
	if err != nil {
		return dst, record(err)
	}
	if len(resp.Errors) > 0 {
		return dst, record(Reject(CodeInternal, fmt.Sprintf("graphql remote: %s", resp.Errors[0])))
	}
	if len(resp.Data) == 0 {
		record(nil)
		return append(dst, "null"...), nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return dst, record(Reject(CodeInternal, fmt.Sprintf("graphql decode data: %s", err.Error())))
	}
	rawField, ok := data[d.remoteFieldName]
	if !ok || len(rawField) == 0 {
		record(nil)
		return append(dst, "null"...), nil
	}
	// Re-prefix upstream __typename values so the local response
	// shows the gateway-prefixed type name (matches the local
	// schema). graphql-go's __typename handling runs on the per-
	// field-resolver path, which ResolveAppend bypasses.
	rawField = d.mirror.prefixResponseTypenames(rawField)
	record(nil)
	return append(dst, rawField...), nil
}

var _ ir.AppendDispatcher = (*graphQLDispatcher)(nil)

// graphqlGroupDispatcher forwards an entire local sub-selection rooted
// at a GraphQL-origin OperationGroup as a single upstream request.
// Wired by `emitGroupContainer` for graphql-origin groups so the
// caller-side query `{ ns { greeter { hello echo } } }` becomes one
// upstream POST `{ greeter { hello echo } }` instead of N round
// trips per leaf — preserves GraphQL's batching property across
// sibling fields under a namespace.
//
// One graphqlGroupDispatcher per top-level group per source. Nested
// sub-groups don't get their own dispatcher; they're folded into the
// parent group's sub-selection and dereferenced from the response
// map via graphql-go's DefaultResolveFn.
//
// canonical-args ingress (HTTP/JSON, gRPC) doesn't reach this
// dispatcher — the per-leaf graphQLDispatcher still owns those
// ingress paths and falls back to its "no AST" error for grouped
// ops. That's a separate limitation worth fixing if HTTP/gRPC clients
// need to reach a namespace-grouped GraphQL upstream.
type graphqlGroupDispatcher struct {
	mirror  *graphQLMirror
	group   *ir.OperationGroup
	metrics Metrics
	ns      string
	ver     string
	label   string // e.g. "query greeter"

	printed forwardPrintCache
}

// forwardQuery prints the upstream query for this group field. Cached
// per (field, operation) AST node like graphQLDispatcher.forwardQuery;
// the plan cache hands the same nodes to repeated requests.
func (d *graphqlGroupDispatcher) forwardQuery(info *graphql.ResolveInfo) (string, error) {
	localField := info.FieldASTs[0]
	key := forwardKey{field: localField, op: info.Operation}
	if p, ok := d.printed.get(key); ok {
		return p.query, nil
	}
	// Build the upstream field: same name as the local group field
	// (graphql-origin: local and remote share the namespace path
	// structure post-introspection), with the local sub-selection
	// rewritten via the mirror so inline-fragment type conditions get
	// unprefixed.
	groupField := d.mirror.rewriteFieldForRemote(localField, d.group.Name, info.FieldASTs[1:], info.Fragments)
	opType := ast.OperationTypeQuery
	if op, ok := info.Operation.(*ast.OperationDefinition); ok && op != nil && op.Operation == ast.OperationTypeMutation {
		opType = ast.OperationTypeMutation
	}
	p, err := printForward(opType, groupField, info.Operation, false, nil)
	if err != nil {
		return "", err
	}
	d.printed.put(key, p)
	return p.query, nil
}

func newGraphqlGroupDispatcher(m *graphQLMirror, group *ir.OperationGroup, metrics Metrics) *graphqlGroupDispatcher {
	label := "query"
	if group.Kind == ir.OpMutation {
		label = "mutation"
	}
	return &graphqlGroupDispatcher{
		mirror:  m,
		group:   group,
		metrics: metrics,
		ns:      m.src.namespace,
		ver:     m.src.version,
		label:   label + " " + group.Name,
	}
}

func (d *graphqlGroupDispatcher) Dispatch(ctx context.Context, _ map[string]any) (any, error) {
	tr := tracerFromContext(ctx)
	ctx, span := tr.startDispatchSpan(ctx, "gateway.dispatch.graphql",
		namespaceAttr(d.ns),
		versionAttr(d.ver),
		methodAttr(d.label),
	)
	defer span.End()

	start := time.Now()
	record := func(err error) error {
		elapsed := time.Since(start)
		d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, elapsed, err)
		addDispatchTime(ctx, elapsed)
		return err
	}

	info := graphQLForwardInfoFrom(ctx)
	if info == nil || len(info.FieldASTs) == 0 {
		// Canonical-args ingress reaches this only when an HTTP/gRPC
		// client targets a leaf op inside a graphql-origin group —
		// surface the gap rather than fabricate a selection.
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: no AST for group %s (canonical-args ingress on a nested-namespace GraphQL upstream)", d.group.Name)))
	}

	// Build the upstream field: same name as the local group field
	// (graphql-origin: local and remote share the namespace path
	// structure post-introspection), with the local sub-selection
	// rewritten via the mirror so inline-fragment type conditions get
	// unprefixed.
	localField := info.FieldASTs[0]
	printed, err := d.forwardQuery(info)
	if err != nil {
		return nil, record(err)
	}

	src := d.mirror.src
	resp, err := src.roundTrip(ctx, d.label, printed, info.VariableValues)
	if err != nil {
		return nil, record(err)
	}
	if len(resp.Errors) > 0 {
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql remote: %s", resp.Errors[0])))
	}
	var data map[string]any
	if len(resp.Data) > 0 {
		if err := jsonUnmarshalLoose(resp.Data, &data); err != nil {
			return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql decode data: %s", err.Error())))
		}
	}
	record(nil)
	// Response key is the alias if present, else the field name.
	respKey := d.group.Name
	if localField.Alias != nil && localField.Alias.Value != "" {
		respKey = localField.Alias.Value
	}
	return data[respKey], nil
}

// DispatchAppend emits the upstream's response data for this group
// field directly into dst, skipping the full-tree unmarshal that
// Dispatch performs. The upstream's JSON for `data["greeter"]` is
// already exactly the shape this field's value should take in the
// local response (we forward the LOCAL selection upstream, so
// projection matches by construction), so we splice the bytes
// verbatim.
//
// One outer Unmarshal into map[string]json.RawMessage keeps the
// values as []byte without recursing into nested objects — saves
// the full-tree map allocation that Dispatch performs. The
// AppendDispatcher caller (graphql-go's writePlannedField) drops
// us into the response buffer at the right offset.
func (d *graphqlGroupDispatcher) DispatchAppend(ctx context.Context, _ map[string]any, dst []byte) ([]byte, error) {
	tr := tracerFromContext(ctx)
	ctx, span := tr.startDispatchSpan(ctx, "gateway.dispatch.graphql",
		namespaceAttr(d.ns),
		versionAttr(d.ver),
		methodAttr(d.label),
	)
	defer span.End()

	start := time.Now()
	record := func(err error) error {
		elapsed := time.Since(start)
		d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, elapsed, err)
		addDispatchTime(ctx, elapsed)
		return err
	}

	info := graphQLForwardInfoFrom(ctx)
	if info == nil || len(info.FieldASTs) == 0 {
		return dst, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: no AST for group %s (canonical-args ingress on a nested-namespace GraphQL upstream)", d.group.Name)))
	}

	localField := info.FieldASTs[0]
	printed, err := d.forwardQuery(info)
	if err != nil {
		return dst, record(err)
	}

	src := d.mirror.src
	resp, err := src.roundTrip(ctx, d.label, printed, info.VariableValues)
	if err != nil {
		return dst, record(err)
	}
	if len(resp.Errors) > 0 {
		return dst, record(Reject(CodeInternal, fmt.Sprintf("graphql remote: %s", resp.Errors[0])))
	}

	// One-level unmarshal: keep each field value as raw JSON bytes
	// rather than recursing into nested maps. Savings vs Dispatch's
	// full-tree map[string]any unmarshal scale with response size.
	respKey := d.group.Name
	if localField.Alias != nil && localField.Alias.Value != "" {
		respKey = localField.Alias.Value
	}
	if len(resp.Data) == 0 {
		record(nil)
		return append(dst, "null"...), nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return dst, record(Reject(CodeInternal, fmt.Sprintf("graphql decode data: %s", err.Error())))
	}
	raw2, ok := data[respKey]
	if !ok || len(raw2) == 0 {
		record(nil)
		return append(dst, "null"...), nil
	}
	raw2 = d.mirror.prefixResponseTypenames(raw2)
	record(nil)
	return append(dst, raw2...), nil
}

// Compile-time assertion: graphqlGroupDispatcher implements both
// ir.Dispatcher and ir.AppendDispatcher.
var _ ir.AppendDispatcher = (*graphqlGroupDispatcher)(nil)

// buildGraphQLCanonicalQuery synthesizes a printable graphql operation
// for canonical-args dispatch. Returns ("", nil) when synthesis can't
// produce something the upstream will accept (output type missing
// from introspection); the dispatcher falls back to the "no AST"
// error in that case so the gap is visible rather than silently
// returning nulls.
//
// Variable names match IR Arg names (canonical-args dispatch keys
// the args map the same way). Selection set: scalar/enum fields on
// the unwrapped output object plus __typename; fields with required
// arguments are skipped (we have nothing to bind them to). Union /
// interface output types degrade to __typename only.
func buildGraphQLCanonicalQuery(m *graphQLMirror, op *ir.Operation, opLabel string) (string, []string) {
	intro := m.src.introspection
	selection := buildCanonicalSelectionSet(intro, op.Output)
	// Object/Interface/Union output with no selectable fields means
	// we can't synthesize a valid query — graphql requires a
	// non-empty selection set on composite return types.
	if selection == "" && op.Output != nil && op.Output.Leaf().IsNamed() {
		if t := intro.Types[op.Output.Leaf().Named]; t != nil {
			switch t.Kind {
			case "OBJECT", "INTERFACE", "UNION":
				return "", nil
			}
		}
	}
	var b strings.Builder
	b.WriteString(opLabel)
	argNames := make([]string, 0, len(op.Args))
	if len(op.Args) > 0 {
		b.WriteString("(")
		for i, a := range op.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("$")
			b.WriteString(a.Name)
			b.WriteString(": ")
			b.WriteString(graphQLArgTypeString(a))
			argNames = append(argNames, a.Name)
		}
		b.WriteString(")")
	}
	b.WriteString(" { ")
	b.WriteString(op.Name)
	if len(op.Args) > 0 {
		b.WriteString("(")
		for i, a := range op.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Name)
			b.WriteString(": $")
			b.WriteString(a.Name)
		}
		b.WriteString(")")
	}
	if selection != "" {
		b.WriteString(" ")
		b.WriteString(selection)
	}
	b.WriteString(" }")
	return b.String(), argNames
}

func graphQLArgTypeString(a *ir.Arg) string {
	inner := graphQLTypeNameForRef(a.Type)
	if a.Repeated {
		item := inner
		if a.ItemRequired {
			item += "!"
		}
		listed := "[" + item + "]"
		if a.Required {
			listed += "!"
		}
		return listed
	}
	if a.Required {
		return inner + "!"
	}
	return inner
}

func graphQLTypeNameForRef(r ir.TypeRef) string {
	if r.IsList() {
		elem := graphQLTypeNameForRef(r.List.Elem)
		if r.List.ItemRequired {
			elem += "!"
		}
		return "[" + elem + "]"
	}
	if r.IsNamed() {
		return r.Named
	}
	switch r.Builtin {
	case ir.ScalarBool:
		return "Boolean"
	case ir.ScalarInt32, ir.ScalarUInt32, ir.ScalarInt64, ir.ScalarUInt64:
		return "Int"
	case ir.ScalarFloat, ir.ScalarDouble:
		return "Float"
	case ir.ScalarID:
		return "ID"
	case ir.ScalarTimestamp, ir.ScalarBytes, ir.ScalarString, ir.ScalarUnknown:
		return "String"
	}
	return "String"
}

// buildCanonicalSelectionSet returns "{ field1 field2 __typename }"
// for object/interface output types, "{ __typename }" for unions, and
// "" for scalars/enums (no selection set required) or missing types.
// Fields with required arguments are skipped — canonical-args dispatch
// has nothing to bind them to.
func buildCanonicalSelectionSet(intro *introspectionSchema, out *ir.TypeRef) string {
	if out == nil || intro == nil {
		return ""
	}
	leaf := out.Leaf()
	out = &leaf
	if out.IsBuiltin() || !out.IsNamed() {
		return ""
	}
	t := intro.Types[out.Named]
	if t == nil {
		return ""
	}
	switch t.Kind {
	case "SCALAR", "ENUM":
		return ""
	case "OBJECT", "INTERFACE":
		fields := []string{"__typename"}
		for _, f := range t.Fields {
			if !introspectionFieldHasOnlyOptionalArgs(f) {
				continue
			}
			if isLeafIntrospectionType(f.Type) {
				fields = append(fields, f.Name)
			}
		}
		return "{ " + strings.Join(fields, " ") + " }"
	case "UNION":
		return "{ __typename }"
	}
	return ""
}

// introspectionFieldHasOnlyOptionalArgs reports whether every arg on
// the field is nullable. A canonical-args selection set can't supply
// arguments for nested fields (we only have args for the top op), so
// fields with NON_NULL args have to be skipped.
func introspectionFieldHasOnlyOptionalArgs(f *introspectionField) bool {
	for _, a := range f.Args {
		if a.Type != nil && a.Type.Kind == "NON_NULL" {
			return false
		}
	}
	return true
}

func isLeafIntrospectionType(r *introspectionTypeRef) bool {
	cur := r
	for cur != nil {
		switch cur.Kind {
		case "NON_NULL", "LIST":
			cur = cur.OfType
		case "SCALAR", "ENUM":
			return true
		default:
			return false
		}
	}
	return false
}

// forwardSubscriptionQuery prints the upstream subscription for this
// field, cached per (field, operation) AST node. A subscription field
// belongs to exactly one dispatcher, so it shares the unary cache.
func (d *graphQLDispatcher) forwardSubscriptionQuery(info *graphql.ResolveInfo) (string, error) {
	key := forwardKey{field: info.FieldASTs[0], op: info.Operation}
	if p, ok := d.printed.get(key); ok {
		return p.query, nil
	}
	rewritten := d.mirror.rewriteFieldForRemote(info.FieldASTs[0], d.remoteFieldName, info.FieldASTs[1:], info.Fragments)
	p, err := printForward(ast.OperationTypeSubscription, rewritten, info.Operation, false, nil)
	if err != nil {
		return "", err
	}
	d.printed.put(key, p)
	return p.query, nil
}

// dispatchSubscribe opens a multiplexed subscription against the
// upstream and returns a chan any of pre-plucked frames. The
// renderer's Subscribe path treats the dispatcher result as the
// subscribe channel; its Resolve closure surfaces rp.Source directly,
// so this goroutine plucks `frame.Result[remoteFieldName]` before
// emitting (matching the pre-cutover subscribingResolver shape where
// the local Resolve picked the field out of the data envelope).
//
// Backpressure middleware is intentionally not wrapped around the
// subscribe path — src.sem is per-source unary capacity, not stream
// lifetime. The pre-cutover subscribingResolver bypassed
// backpressureMiddleware for the same reason; the multiplexer broker
// is the rate-control story for streams.
func (d *graphQLDispatcher) dispatchSubscribe(ctx context.Context) (any, error) {
	info := graphQLForwardInfoFrom(ctx)
	if info == nil || len(info.FieldASTs) == 0 {
		return nil, fmt.Errorf("graphql ingest: no AST for subscription %s", d.remoteFieldName)
	}
	printed, err := d.forwardSubscriptionQuery(info)
	if err != nil {
		return nil, err
	}

	src := d.mirror.src
	r := src.pickReplica()
	if r == nil {
		return nil, fmt.Errorf("graphql ingest: no live replicas for %s", src.namespace)
	}
	broker := src.getSubBroker()
	upstream, release, err := broker.acquire(ctx, r.endpoint, printed, info.VariableValues, src.forwardHeaders)
	if err != nil {
		return nil, err
	}
	out := make(chan any, 8)
	remote := d.remoteFieldName
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
					case <-ctx.Done():
						return
					}
					if f.Done {
						return
					}
					continue
				}
				if f.Result != nil {
					select {
					case out <- f.Result[remote]:
					case <-ctx.Done():
						return
					}
				}
				if f.Done {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// graphQLBackpressureConfig bundles a graphql source's per-dispatch
// knobs for backpressureMiddleware. Sibling of pool / openAPI
// equivalents.
func graphQLBackpressureConfig(src *graphQLSource, label string, metrics Metrics, bp BackpressureOptions) backpressureConfig {
	return backpressureConfig{
		Sem:         src.sem,
		Queueing:    &src.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   src.namespace,
		Version:     src.version,
		Label:       label,
		Kind:        "unary",
	}
}

var _ ir.Dispatcher = (*graphQLDispatcher)(nil)
