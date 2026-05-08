package gateway

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/graphql-go/graphql"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// RuntimeOptions configures RenderGraphQLRuntime. Today the only
// knobs are shared scalars — graphql-go forbids two scalars sharing
// a Name in one schema, so callers feeding multiple OpenAPI /
// GraphQL-ingest services into the same schema have to mint Long
// and JSON instances once and pass them through. The struct exists
// so future cutover steps (custom resolver hooks, naming overrides)
// have a stable place to add knobs.
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
// Multi-version fold: services are grouped by Namespace. Within
// each namespace, sorted ascending by versionN with latest last,
// the renderer emits:
//
//   - One root field `<ns>` per Kind (Query / Mutation), typed as
//     a synthesized container Object. The container holds latest's
//     ops/groups flat at top, plus a sub-field per version (incl
//     latest) named after the version label so old versions are
//     still addressable. Non-latest sub-groups carry @deprecated.
//   - Subscriptions flatten — graphql-go forbids nested Object
//     types under Subscription, so the renderer surfaces
//     `<ns>_<op>` (latest) / `<ns>_<vN>_<op>` (older) /
//     `<ns>_<group>_<op>` (when latest has a Subscription Group)
//     names directly on the Subscription root.
//
// Container type names follow the SDL convention
// (`<PathPascal><Kind>Namespace`): top-level `greeter` (Query) →
// `GreeterQueryNamespace`; sub `v1` → `GreeterV1QueryNamespace`.
//
// Naming, type-prefixing, and enum-value coercion follow the
// source format's convention via Service.OriginKind:
//
//   - KindProto: type names in their proto-FullName form via
//     exportedName() (proto packages keep version coordinates
//     globally unique, no extra prefix needed); EnumValue.Number
//     is the runtime Value (matches graphql-go's int32 enum
//     coercion).
//   - KindOpenAPI: type names prefixed `<ns>_` for latest, or
//     `<ns>_<vN>_` for older versions; Long / JSON scalars
//     projected from RuntimeOptions when present.
//   - KindGraphQL: type names prefixed the same way; EnumValue.Name
//     is the runtime Value (string), matching what the upstream's
//     introspection returned.
//
// Lives side-by-side with buildSchemaLocked in step 2; cutover
// begins in step 3.
func RenderGraphQLRuntime(svcs []*ir.Service, registry *ir.DispatchRegistry, opts RuntimeOptions) (*graphql.Schema, error) {
	if registry == nil {
		return nil, fmt.Errorf("runtime: nil DispatchRegistry")
	}

	byNS := map[string][]*ir.Service{}
	for _, svc := range svcs {
		byNS[svc.Namespace] = append(byNS[svc.Namespace], svc)
	}
	namespaces := make([]string, 0, len(byNS))
	for ns := range byNS {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	subs := graphql.Fields{}

	for _, ns := range namespaces {
		services := byNS[ns]
		sort.SliceStable(services, func(i, j int) bool {
			return parseRuntimeVersionN(services[i].Version) < parseRuntimeVersionN(services[j].Version)
		})
		latest := services[len(services)-1]
		latestReason := fmt.Sprintf("%s is current", latest.Version)

		builders := make(map[*ir.Service]*IRTypeBuilder, len(services))
		for _, svc := range services {
			tb, err := newRuntimeTypeBuilder(svc, opts, svc == latest)
			if err != nil {
				return nil, err
			}
			builders[svc] = tb
		}

		for _, kind := range []ir.OpKind{ir.OpQuery, ir.OpMutation} {
			field, err := buildNamespaceFold(ns, services, latest, latestReason, builders, kind, registry)
			if err != nil {
				return nil, fmt.Errorf("runtime: ns %s kind %v: %w", ns, kind, err)
			}
			if field == nil {
				continue
			}
			switch kind {
			case ir.OpQuery:
				queries[ns] = field
			case ir.OpMutation:
				mutations[ns] = field
			}
		}

		// Subscriptions flatten — graphql-go's executor doesn't
		// support nested Object types under Subscription.
		for _, svc := range services {
			isLatest := svc == latest
			depReason := ""
			if !isLatest {
				depReason = latestReason
			}
			prefix := ns + "_"
			if !isLatest {
				prefix = ns + "_" + svc.Version + "_"
			}
			if err := addSubscriptionFlat(subs, svc, builders[svc], prefix, depReason, registry); err != nil {
				return nil, fmt.Errorf("runtime: ns %s subscription: %w", ns, err)
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

// buildNamespaceFold synthesises one Query/Mutation root field for
// `ns`: a container Object whose fields are latest's ops/groups
// flat at top, plus a `vN` sub-field per version (including latest)
// addressing that specific version's content. Returns nil when no
// service in the namespace has content of this Kind.
func buildNamespaceFold(ns string, services []*ir.Service, latest *ir.Service, latestReason string, builders map[*ir.Service]*IRTypeBuilder, kind ir.OpKind, registry *ir.DispatchRegistry) (*graphql.Field, error) {
	nsPath := pascalCaseRuntime(ns)
	kindSfx := kindSuffixForRuntime(kind)

	nsFields := graphql.Fields{}
	if err := addServiceContent(nsFields, latest, builders[latest], kind, nsPath, "", registry); err != nil {
		return nil, fmt.Errorf("latest content: %w", err)
	}

	for _, svc := range services {
		isLatest := svc == latest
		depReason := ""
		if !isLatest {
			depReason = latestReason
		}
		verPath := nsPath + pascalCaseRuntime(svc.Version)
		verFields := graphql.Fields{}
		if err := addServiceContent(verFields, svc, builders[svc], kind, verPath, depReason, registry); err != nil {
			return nil, fmt.Errorf("version %s: %w", svc.Version, err)
		}
		if len(verFields) == 0 {
			continue
		}
		verContainer := graphql.NewObject(graphql.ObjectConfig{
			Name:   verPath + kindSfx + "Namespace",
			Fields: verFields,
		})
		verField := &graphql.Field{
			Type:    graphql.NewNonNull(verContainer),
			Resolve: func(rp graphql.ResolveParams) (any, error) { return struct{}{}, nil },
		}
		if depReason != "" {
			verField.DeprecationReason = depReason
		}
		if _, exists := nsFields[svc.Version]; exists {
			return nil, fmt.Errorf("version sub-group %q collides with op", svc.Version)
		}
		nsFields[svc.Version] = verField
	}

	if len(nsFields) == 0 {
		return nil, nil
	}

	nsContainer := graphql.NewObject(graphql.ObjectConfig{
		Name:   nsPath + kindSfx + "Namespace",
		Fields: nsFields,
	})
	return &graphql.Field{
		Type:    graphql.NewNonNull(nsContainer),
		Resolve: func(rp graphql.ResolveParams) (any, error) { return struct{}{}, nil },
	}, nil
}

// addServiceContent appends svc's top-level Operations + Groups
// (filtered to `kind`) into `dst` using `tb`. parentPath is the
// PascalCase running prefix used to name nested-group containers
// (e.g. "PetsV1" for an op under Query.pets.v1.<...>); the kind
// suffix and "Namespace" are appended. depReason, when non-empty,
// stamps onto every emitted field.
func addServiceContent(dst graphql.Fields, svc *ir.Service, tb *IRTypeBuilder, kind ir.OpKind, parentPath, depReason string, registry *ir.DispatchRegistry) error {
	for _, op := range svc.Operations {
		if op.Kind != kind {
			continue
		}
		if err := emitOperation(dst, op, tb, depReason, registry); err != nil {
			return err
		}
	}
	for _, g := range svc.Groups {
		if g.Kind != kind {
			continue
		}
		if err := emitGroupContainer(dst, g, tb, kind, parentPath, depReason, registry); err != nil {
			return err
		}
	}
	return nil
}

// emitGroupContainer renders one Group as a synthesized container
// Object and adds it to dst under the group's Name. Recursive on
// sub-groups; the synthesized type name path-joins the parent.
func emitGroupContainer(dst graphql.Fields, g *ir.OperationGroup, tb *IRTypeBuilder, kind ir.OpKind, parentPath, depReason string, registry *ir.DispatchRegistry) error {
	childPath := parentPath + pascalCaseRuntime(g.Name)
	fields := graphql.Fields{}
	for _, op := range g.Operations {
		if err := emitOperation(fields, op, tb, depReason, registry); err != nil {
			return fmt.Errorf("group %s: %w", g.Name, err)
		}
	}
	for _, sub := range g.Groups {
		if err := emitGroupContainer(fields, sub, tb, kind, childPath, depReason, registry); err != nil {
			return err
		}
	}
	if len(fields) == 0 {
		fields["_empty"] = &graphql.Field{Type: graphql.String}
	}
	container := graphql.NewObject(graphql.ObjectConfig{
		Name:        childPath + kindSuffixForRuntime(kind) + "Namespace",
		Description: g.Description,
		Fields:      fields,
	})
	field := &graphql.Field{
		Type:        graphql.NewNonNull(container),
		Description: g.Description,
		Resolve:     func(rp graphql.ResolveParams) (any, error) { return struct{}{}, nil },
	}
	if depReason != "" {
		field.DeprecationReason = depReason
	}
	if _, exists := dst[g.Name]; exists {
		return fmt.Errorf("group field collision: %s", g.Name)
	}
	dst[g.Name] = field
	return nil
}

// emitOperation builds a graphql.Field for op and adds it to dst
// keyed by op.Name. depReason, when non-empty, overrides the op's
// own Deprecated string with the version-fold deprecation message
// — non-latest version sub-groups stamp the same reason on every
// nested field.
func emitOperation(dst graphql.Fields, op *ir.Operation, tb *IRTypeBuilder, depReason string, registry *ir.DispatchRegistry) error {
	field, err := buildRuntimeOperation(tb, op, registry)
	if err != nil {
		return fmt.Errorf("op %s: %w", op.Name, err)
	}
	if depReason != "" {
		field.DeprecationReason = depReason
	}
	if _, exists := dst[op.Name]; exists {
		return fmt.Errorf("op field collision: %s", op.Name)
	}
	dst[op.Name] = field
	return nil
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

// addSubscriptionFlat appends svc's subscription-kind Operations +
// Groups (recursively flattened) into dst with `prefix`. graphql-go
// forbids nested objects under Subscription so the renderer
// surfaces them flat — same convention as the existing
// buildSubscriptionFields.
func addSubscriptionFlat(dst graphql.Fields, svc *ir.Service, tb *IRTypeBuilder, prefix, depReason string, registry *ir.DispatchRegistry) error {
	for _, op := range svc.Operations {
		if op.Kind != ir.OpSubscription {
			continue
		}
		f, err := buildRuntimeOperation(tb, op, registry)
		if err != nil {
			return fmt.Errorf("op %s: %w", op.Name, err)
		}
		if depReason != "" {
			f.DeprecationReason = depReason
		}
		name := prefix + op.Name
		if _, exists := dst[name]; exists {
			return fmt.Errorf("subscription field collision: %s", name)
		}
		dst[name] = f
	}
	for _, g := range svc.Groups {
		if g.Kind != ir.OpSubscription {
			continue
		}
		if err := flattenSubGroupWithPrefix(dst, g, prefix, depReason, registry, tb); err != nil {
			return err
		}
	}
	return nil
}

// flattenSubGroupWithPrefix walks one subscription-rooted Group and
// emits its operations into dst with name `prefix + group + "_" +
// op` (recursing through sub-groups). Matches the SDL renderer's
// flattenSubscriptionGroup convention.
func flattenSubGroupWithPrefix(dst graphql.Fields, g *ir.OperationGroup, prefix, depReason string, registry *ir.DispatchRegistry, tb *IRTypeBuilder) error {
	pre := prefix + g.Name + "_"
	for _, op := range g.Operations {
		f, err := buildRuntimeOperation(tb, op, registry)
		if err != nil {
			return fmt.Errorf("op %s: %w", op.Name, err)
		}
		if depReason != "" {
			f.DeprecationReason = depReason
		}
		name := pre + op.Name
		if _, exists := dst[name]; exists {
			return fmt.Errorf("subscription field collision: %s", name)
		}
		dst[name] = f
	}
	for _, sub := range g.Groups {
		if err := flattenSubGroupWithPrefix(dst, sub, pre, depReason, registry, tb); err != nil {
			return err
		}
	}
	return nil
}

// newRuntimeTypeBuilder picks the IRTypeNaming + IRTypeBuilderOptions
// matching svc.OriginKind. isLatest controls whether OpenAPI /
// GraphQL type names get the bare `<ns>_` prefix or the
// version-qualified `<ns>_<vN>_` prefix; proto's package-qualified
// FullNames are version-distinct without extra prefixing, so
// isLatest is ignored there.
//
// The naming closures intentionally mirror the per-format builders
// that exist today (newProtoIRTypeBuilder / newOpenAPISourceTypeBuilder
// / newGraphQLSourceTypeBuilder) so a schema produced by
// RenderGraphQLRuntime is name-identical to the per-format outputs
// it will eventually replace.
func newRuntimeTypeBuilder(svc *ir.Service, opts RuntimeOptions, isLatest bool) (*IRTypeBuilder, error) {
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
		if !isLatest && svc.Version != "" {
			prefix = svc.Namespace + "_" + svc.Version + "_"
		}
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
		if !isLatest && svc.Version != "" {
			prefix = svc.Namespace + "_" + svc.Version + "_"
		}
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

// parseRuntimeVersionN extracts a numeric version index from a
// "vN" / "N" / "" string for sort. Empty / unparseable inputs sort
// as 0 — single-version namespaces still get the fold treatment
// (same posture as `parseVersion` which canonicalises empty to v1).
func parseRuntimeVersionN(s string) int {
	if s == "" {
		return 0
	}
	digits := s
	if digits[0] == 'v' || digits[0] == 'V' {
		digits = digits[1:]
	}
	if n, err := strconv.Atoi(digits); err == nil {
		return n
	}
	return 0
}

// kindSuffixForRuntime maps an OpKind to the suffix used in
// synthesized container type names — matches gw/ir/render_graphql.go's
// kindSuffix so SDL and runtime type names agree.
func kindSuffixForRuntime(kind ir.OpKind) string {
	switch kind {
	case ir.OpMutation:
		return "Mutation"
	case ir.OpSubscription:
		return "Subscription"
	}
	return "Query"
}

// pascalCaseRuntime upper-cases the first rune. Distinct from the
// helper in gw/ir/render_graphql.go because that one isn't exported;
// the rules match (leading rune only, no segment normalisation) so
// SDL and runtime container names agree.
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
