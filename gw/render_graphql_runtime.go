package gateway

import (
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// RuntimeOptions configures RenderGraphQLRuntime. Today the only
// knobs are shared scalars — graphql-go forbids two scalars sharing
// a Name in one schema, so callers feeding multiple OpenAPI /
// GraphQL-ingest services into the same schema have to mint Long
// and JSON instances once and pass them through. The struct exists
// so future cutover steps (multi-version fold prefix policy, custom
// resolver hooks) have a stable place to add knobs.
type RuntimeOptions struct {
	// JSONType, when set, is shared across every per-service
	// IRTypeBuilder so the produced graphql.Schema doesn't end up
	// with two distinct "JSON" scalars. nil = each builder lazily
	// mints its own (only safe for single-source schemas).
	JSONType *graphql.Scalar

	// LongType is the equivalent for the OpenAPI Long scalar (int64
	// / uint64 → decimal-string). Wired into IRTypeBuilderOptions
	// for OpenAPI-origin services. nil = builder mints its own per
	// service (collides if multiple OpenAPI sources share a schema).
	LongType *graphql.Scalar
}

// RenderGraphQLRuntime walks `svcs` into a fully-wired
// graphql.Schema. Each Operation's Resolve / Subscribe looks up its
// Dispatcher in `registry` via Operation.SchemaID at call time, so
// the schema graph and dispatcher state evolve independently — the
// gateway rebuilds schemas more often than it rebuilds dispatchers
// and the lookup-by-id pattern keeps stale captures from leaking.
//
// Naming, type-prefixing, and enum-value coercion follow the source
// format's convention via Service.OriginKind:
//
//   - KindProto: type names in their proto-FullName form via
//     exportedName() (proto packages keep version coordinates
//     globally unique, no extra prefix needed); EnumValue.Number
//     is the runtime Value (matches graphql-go's int32 enum
//     coercion).
//   - KindOpenAPI: type names prefixed "<ns>_" so two namespaces
//     don't collide on a shared graphql.Schema; Long / JSON
//     scalars projected from RuntimeOptions when present.
//   - KindGraphQL: type names prefixed "<ns>_"; EnumValue.Name is
//     the runtime Value (string), matching what the upstream's
//     introspection returned.
//
// Step 1 (this commit): single-version per namespace — each service
// in `svcs` emits independently, no version-fold across services
// sharing a namespace. Field-name collisions across services share
// the root namespace (Query / Mutation / Subscription) and fail the
// build. Multi-version fold (latest at top, vN sub-Groups) lands in
// Step 2; cutover from buildSchemaLocked lands in Step 3 onward.
//
// Subscriptions: graphql-go forbids nested Object types under
// Subscription, so subscription-rooted Groups flatten to
// "<group>_<op>" names — same convention RenderGraphQL (SDL) uses
// via flattenSubscriptionGroup.
func RenderGraphQLRuntime(svcs []*ir.Service, registry *ir.DispatchRegistry, opts RuntimeOptions) (*graphql.Schema, error) {
	if registry == nil {
		return nil, fmt.Errorf("runtime: nil DispatchRegistry")
	}
	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	subs := graphql.Fields{}

	for _, svc := range svcs {
		tb, err := newRuntimeTypeBuilder(svc, opts)
		if err != nil {
			return nil, err
		}
		for _, op := range svc.Operations {
			field, err := buildRuntimeOperation(tb, op, registry)
			if err != nil {
				return nil, fmt.Errorf("runtime: %s/%s op %s: %w", svc.Namespace, svc.Version, op.Name, err)
			}
			if err := mergeRootField(svc.Namespace, op.Name, op.Kind, field, queries, mutations, subs); err != nil {
				return nil, err
			}
		}
		for _, g := range svc.Groups {
			switch g.Kind {
			case ir.OpQuery, ir.OpMutation:
				containerName := groupContainerName("", g.Name, g.Kind)
				container, err := buildRuntimeGroupContainer(tb, g, registry, containerName)
				if err != nil {
					return nil, fmt.Errorf("runtime: %s/%s group %s: %w", svc.Namespace, svc.Version, g.Name, err)
				}
				field := &graphql.Field{
					Type:        graphql.NewNonNull(container),
					Description: g.Description,
					Resolve: func(rp graphql.ResolveParams) (any, error) {
						return struct{}{}, nil
					},
				}
				dst := queries
				if g.Kind == ir.OpMutation {
					dst = mutations
				}
				if _, exists := dst[g.Name]; exists {
					return nil, fmt.Errorf("runtime: group field collision in %s: %s", svc.Namespace, g.Name)
				}
				dst[g.Name] = field
			case ir.OpSubscription:
				if err := flattenRuntimeSubscriptionGroup(tb, g, "", registry, subs); err != nil {
					return nil, fmt.Errorf("runtime: %s/%s subscription group %s: %w", svc.Namespace, svc.Version, g.Name, err)
				}
			}
		}
	}

	if len(queries) == 0 {
		queries["_status"] = &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (any, error) {
				return "no services registered", nil
			},
		}
	}

	cfg := graphql.SchemaConfig{
		Query: graphql.NewObject(graphql.ObjectConfig{Name: "Query", Fields: queries}),
	}
	if len(mutations) > 0 {
		cfg.Mutation = graphql.NewObject(graphql.ObjectConfig{Name: "Mutation", Fields: mutations})
	}
	if len(subs) > 0 {
		cfg.Subscription = graphql.NewObject(graphql.ObjectConfig{Name: "Subscription", Fields: subs})
	}
	schema, err := graphql.NewSchema(cfg)
	if err != nil {
		return nil, fmt.Errorf("runtime: graphql.NewSchema: %w", err)
	}
	return &schema, nil
}

// newRuntimeTypeBuilder picks the IRTypeNaming + IRTypeBuilderOptions
// matching svc.OriginKind. The naming closures intentionally mirror
// the per-format builders that exist today (newProtoIRTypeBuilder,
// newOpenAPISourceTypeBuilder, newGraphQLSourceTypeBuilder) so a
// schema produced by RenderGraphQLRuntime is name-identical to the
// per-format outputs it will eventually replace.
func newRuntimeTypeBuilder(svc *ir.Service, opts RuntimeOptions) (*IRTypeBuilder, error) {
	switch svc.OriginKind {
	case ir.KindProto:
		return NewIRTypeBuilder(svc, IRTypeNaming{
			ObjectName: exportedName,
			EnumName:   exportedName,
			UnionName:  exportedName,
			InputName:  func(s string) string { return exportedName(s) + "_Input" },
			FieldName:  lowerCamel,
		}, IRTypeBuilderOptions{}), nil

	case ir.KindOpenAPI:
		prefix := svc.Namespace + "_"
		naming := IRTypeNaming{
			ObjectName:    func(s string) string { return prefix + s },
			InputName:     func(s string) string { return prefix + s + "Input" },
			EnumName:      func(s string) string { return prefix + s },
			UnionName:     func(s string) string { return prefix + s },
			InterfaceName: func(s string) string { return prefix + s },
			ScalarName:    func(s string) string { return prefix + s },
			FieldName:     lowerCamel,
		}
		return NewIRTypeBuilder(svc, naming, IRTypeBuilderOptions{
			Int64Type:  opts.LongType,
			UInt64Type: opts.LongType,
			MapType:    opts.JSONType,
			JSONType:   opts.JSONType,
		}), nil

	case ir.KindGraphQL:
		prefix := svc.Namespace + "_"
		naming := IRTypeNaming{
			ObjectName:     func(s string) string { return prefix + s },
			InputName:      func(s string) string { return prefix + s },
			EnumName:       func(s string) string { return prefix + s },
			UnionName:      func(s string) string { return prefix + s },
			InterfaceName:  func(s string) string { return prefix + s },
			ScalarName:     func(s string) string { return prefix + s },
			FieldName:      identityName,
			EnumValueName:  identityName,
			EnumValueValue: func(v ir.EnumValue) any { return v.Name },
		}
		return NewIRTypeBuilder(svc, naming, IRTypeBuilderOptions{
			MapType:  opts.JSONType,
			JSONType: opts.JSONType,
		}), nil

	default:
		return nil, fmt.Errorf("runtime: unsupported OriginKind %v on %s/%s", svc.OriginKind, svc.Namespace, svc.Version)
	}
}

// buildRuntimeOperation renders one Operation into a graphql.Field.
// The closure looks up the Dispatcher via op.SchemaID at call time;
// if the dispatcher isn't registered (e.g. service deregistered
// between schema rebuild and request), the resolver returns
// CodeInternal at call time rather than panicking.
func buildRuntimeOperation(tb *IRTypeBuilder, op *ir.Operation, registry *ir.DispatchRegistry) (*graphql.Field, error) {
	args := graphql.FieldConfigArgument{}
	for _, a := range op.Args {
		t, err := tb.Input(a.Type, a.Repeated, a.Required, a.ItemRequired)
		if err != nil {
			return nil, fmt.Errorf("arg %s: %w", a.Name, err)
		}
		args[a.Name] = &graphql.ArgumentConfig{
			Type:         t,
			Description:  a.Description,
			DefaultValue: a.Default,
		}
	}
	var out graphql.Output = graphql.String
	if op.Output != nil {
		o, err := tb.Output(*op.Output, op.OutputRepeated, op.OutputRequired, op.OutputItemRequired)
		if err != nil {
			return nil, fmt.Errorf("output: %w", err)
		}
		out = o
	}
	sid := op.SchemaID
	dispatch := func(rp graphql.ResolveParams) (any, error) {
		d := registry.Get(sid)
		if d == nil {
			return nil, Reject(CodeInternal, fmt.Sprintf("gateway: no dispatcher for %s", sid))
		}
		return d.Dispatch(rp.Context, rp.Args)
	}
	if op.Kind == ir.OpSubscription {
		return &graphql.Field{
			Type:              out,
			Args:              args,
			Description:       op.Description,
			DeprecationReason: op.Deprecated,
			Subscribe:         dispatch,
			Resolve: func(rp graphql.ResolveParams) (any, error) {
				return rp.Source, nil
			},
		}, nil
	}
	return &graphql.Field{
		Type:              out,
		Args:              args,
		Description:       op.Description,
		DeprecationReason: op.Deprecated,
		Resolve:           dispatch,
	}, nil
}

// buildRuntimeGroupContainer synthesises one Object type for an
// OperationGroup, recursively descending into sub-groups. Uses
// FieldsThunk so cyclic refs (a sub-group whose Object references
// the parent) resolve correctly — same idiom IRTypeBuilder uses for
// recursive Type fields.
func buildRuntimeGroupContainer(tb *IRTypeBuilder, g *ir.OperationGroup, registry *ir.DispatchRegistry, containerName string) (*graphql.Object, error) {
	// Build fields eagerly first to surface errors at schema-build
	// time rather than letting a thunk swallow them. The thunk just
	// returns the precomputed map.
	fields := graphql.Fields{}
	for _, op := range g.Operations {
		field, err := buildRuntimeOperation(tb, op, registry)
		if err != nil {
			return nil, fmt.Errorf("op %s: %w", op.Name, err)
		}
		fields[op.Name] = field
	}
	for _, sub := range g.Groups {
		subContainerName := nestedContainerName(containerName, sub.Name)
		subObj, err := buildRuntimeGroupContainer(tb, sub, registry, subContainerName)
		if err != nil {
			return nil, fmt.Errorf("subgroup %s: %w", sub.Name, err)
		}
		fields[sub.Name] = &graphql.Field{
			Type:        graphql.NewNonNull(subObj),
			Description: sub.Description,
			Resolve: func(rp graphql.ResolveParams) (any, error) {
				return struct{}{}, nil
			},
		}
	}
	if len(fields) == 0 {
		fields["_empty"] = &graphql.Field{Type: graphql.String}
	}
	return graphql.NewObject(graphql.ObjectConfig{
		Name:        containerName,
		Description: g.Description,
		Fields:      fields,
	}), nil
}

// flattenRuntimeSubscriptionGroup walks a Subscription-rooted group
// and emits flat fields into `out`. graphql-go doesn't support
// nested objects under Subscription, so the renderer surfaces them
// as `<group>_<op>` (matching the SDL renderer's
// flattenSubscriptionGroup convention).
func flattenRuntimeSubscriptionGroup(tb *IRTypeBuilder, g *ir.OperationGroup, prefix string, registry *ir.DispatchRegistry, out graphql.Fields) error {
	pre := prefix + g.Name + "_"
	for _, op := range g.Operations {
		field, err := buildRuntimeOperation(tb, op, registry)
		if err != nil {
			return fmt.Errorf("op %s: %w", op.Name, err)
		}
		name := pre + op.Name
		if _, exists := out[name]; exists {
			return fmt.Errorf("subscription field collision: %s", name)
		}
		out[name] = field
	}
	for _, sub := range g.Groups {
		if err := flattenRuntimeSubscriptionGroup(tb, sub, pre, registry, out); err != nil {
			return err
		}
	}
	return nil
}

// mergeRootField places one top-level Operation into the matching
// root (Query / Mutation / Subscription). Field-name collisions
// across services share the same root, so a duplicate fails the
// build — matches buildSchemaLocked's existing collision posture.
func mergeRootField(ns, name string, kind ir.OpKind, field *graphql.Field, queries, mutations, subs graphql.Fields) error {
	switch kind {
	case ir.OpQuery:
		if _, exists := queries[name]; exists {
			return fmt.Errorf("runtime: query field collision in %s: %s", ns, name)
		}
		queries[name] = field
	case ir.OpMutation:
		if _, exists := mutations[name]; exists {
			return fmt.Errorf("runtime: mutation field collision in %s: %s", ns, name)
		}
		mutations[name] = field
	case ir.OpSubscription:
		if _, exists := subs[name]; exists {
			return fmt.Errorf("runtime: subscription field collision in %s: %s", ns, name)
		}
		subs[name] = field
	}
	return nil
}

// groupContainerName returns the synthesized type name for one
// Group container. Matches the SDL renderer's convention
// `<PathPascal><Kind>Namespace` so a cross-checked SDL diff
// against RenderGraphQL's output is identifier-stable.
func groupContainerName(parentPath, groupName string, kind ir.OpKind) string {
	suffix := "Query"
	switch kind {
	case ir.OpMutation:
		suffix = "Mutation"
	case ir.OpSubscription:
		// Subscriptions flatten — this branch isn't reached through
		// the normal path, but kept consistent so a fallback caller
		// produces a predictable name.
		suffix = "Subscription"
	}
	return parentPath + pascalCaseRuntime(groupName) + suffix + "Namespace"
}

// nestedContainerName extends a parent container name with one
// child group segment. "GreeterQueryNamespace" + "v1" →
// "GreeterQueryV1Namespace": the kind suffix stays at the end so
// the same namespace shape under Query and Mutation produces
// distinct types.
func nestedContainerName(parentContainer, childName string) string {
	root := strings.TrimSuffix(parentContainer, "Namespace")
	for _, sfx := range []string{"Mutation", "Subscription", "Query"} {
		if strings.HasSuffix(root, sfx) {
			path := strings.TrimSuffix(root, sfx)
			return path + pascalCaseRuntime(childName) + sfx + "Namespace"
		}
	}
	return root + pascalCaseRuntime(childName) + "Namespace"
}

// pascalCaseRuntime upper-cases the first rune. Distinct from the
// helper in gw/ir/render_graphql.go because that one lives in the
// ir package and isn't exported; the rules match (leading rune
// only, no segment normalisation) so SDL and runtime container
// names agree.
func pascalCaseRuntime(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}
