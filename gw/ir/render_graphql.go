package ir

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// RenderGraphQL emits SDL text for svc. The IR carries enough type
// info to render directly without going through a graphql.Schema —
// we just walk Service.Types + Service.Operations + Service.Groups
// and string-build.
//
// Same-kind shortcut: not implemented for GraphQL, since the
// "natural" GraphQL output is SDL text and SDL isn't what
// IngestGraphQL captured (it ate JSON introspection). The
// canonical-field path produces the same SDL the upstream service
// would; same-kind round-trip via re-rendering the captured wire
// types would be a redundant code path.
//
// Cross-kind input (proto- or OpenAPI-origin Services) renders
// straight from the canonical fields. Unions / interfaces / inputs
// render as their GraphQL equivalents; oneof on proto messages
// emits as a regular field list (the oneof grouping is comment-
// noted but not enforced).
//
// Nested namespaces (Service.Groups) emit as synthesized container
// Object types under the corresponding root. A top-level Query
// group "greeter" renders as field `greeter: GreeterQueryNamespace`
// on Query, with a synthesized type `type GreeterQueryNamespace { ... }`
// holding that group's operations and any sub-groups (recursively
// suffix-extended: greeter.v1 → GreeterV1QueryNamespace).
// Subscription groups flatten to path-joined names because
// graphql-go's executor doesn't support nested objects under
// Subscription.
func RenderGraphQL(svcs []*Service) string {
	var b strings.Builder

	// Schema declaration: pick the operation kinds that have any
	// content. GraphQL requires at least Query.
	hasQuery, hasMut, hasSub := false, false, false
	for _, svc := range svcs {
		for _, op := range svc.Operations {
			switch op.Kind {
			case OpQuery:
				hasQuery = true
			case OpMutation:
				hasMut = true
			case OpSubscription:
				hasSub = true
			}
		}
		for _, g := range svc.Groups {
			switch g.Kind {
			case OpQuery:
				hasQuery = true
			case OpMutation:
				hasMut = true
			case OpSubscription:
				hasSub = true
			}
		}
	}
	if !hasQuery {
		hasQuery = true // we'll still emit a stub Query to keep the SDL valid
	}
	b.WriteString("schema {\n")
	if hasQuery {
		b.WriteString("  query: Query\n")
	}
	if hasMut {
		b.WriteString("  mutation: Mutation\n")
	}
	if hasSub {
		b.WriteString("  subscription: Subscription\n")
	}
	b.WriteString("}\n")

	// Collect every type from every service. Cross-service name
	// collisions are the caller's problem (e.g. transforms apply
	// per-service prefixes before render).
	allTypes := map[string]*Type{}
	for _, svc := range svcs {
		for k, t := range svc.Types {
			allTypes[k] = t
		}
	}
	typeNames := make([]string, 0, len(allTypes))
	for k := range allTypes {
		typeNames = append(typeNames, k)
	}
	sort.Strings(typeNames)
	for _, n := range typeNames {
		t := allTypes[n]
		b.WriteString("\n")
		switch t.TypeKind {
		case TypeObject:
			renderGraphQLObject(&b, t, "type")
		case TypeInput:
			renderGraphQLObject(&b, t, "input")
		case TypeInterface:
			renderGraphQLObject(&b, t, "interface")
		case TypeEnum:
			renderGraphQLEnum(&b, t)
		case TypeUnion:
			renderGraphQLUnion(&b, t)
		case TypeScalar:
			fmt.Fprintf(&b, "scalar %s\n", t.Name)
		}
	}

	// Top-level Operations group by Kind.
	queries := []*Operation{}
	mutations := []*Operation{}
	subs := []*Operation{}
	for _, svc := range svcs {
		for _, op := range svc.Operations {
			switch op.Kind {
			case OpQuery:
				queries = append(queries, op)
			case OpMutation:
				mutations = append(mutations, op)
			case OpSubscription:
				subs = append(subs, op)
			}
		}
	}

	// Top-level Groups by Kind. Subscription groups flatten to ops
	// (graphql-go limitation), so they merge into subs.
	queryGroups := []*OperationGroup{}
	mutationGroups := []*OperationGroup{}
	for _, svc := range svcs {
		for _, g := range svc.Groups {
			switch g.Kind {
			case OpQuery:
				queryGroups = append(queryGroups, g)
			case OpMutation:
				mutationGroups = append(mutationGroups, g)
			case OpSubscription:
				flattenSubscriptionGroup(g, "", &subs)
			}
		}
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].Name < queries[j].Name })
	sort.Slice(mutations, func(i, j int) bool { return mutations[i].Name < mutations[j].Name })
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	sort.Slice(queryGroups, func(i, j int) bool { return queryGroups[i].Name < queryGroups[j].Name })
	sort.Slice(mutationGroups, func(i, j int) bool { return mutationGroups[i].Name < mutationGroups[j].Name })

	if hasQuery {
		renderGraphQLRoot(&b, "Query", queries, queryGroups, "Query")
	}
	if hasMut {
		renderGraphQLRoot(&b, "Mutation", mutations, mutationGroups, "Mutation")
	}
	if hasSub {
		renderGraphQLRoot(&b, "Subscription", subs, nil, "Subscription")
	}

	// Synthesized container Object types for nested groups, walked
	// recursively. Emitted after the Query/Mutation/Subscription
	// roots so refs resolve forward; SDL parsers don't require
	// declaration order.
	for _, g := range queryGroups {
		renderGraphQLGroupContainer(&b, g, pascalCase(g.Name)+kindSuffix(g.Kind)+"Namespace")
	}
	for _, g := range mutationGroups {
		renderGraphQLGroupContainer(&b, g, pascalCase(g.Name)+kindSuffix(g.Kind)+"Namespace")
	}
	return b.String()
}

func renderGraphQLObject(b *strings.Builder, t *Type, keyword string) {
	fmt.Fprintf(b, "%s %s {\n", keyword, t.Name)
	if len(t.Fields) == 0 {
		b.WriteString("  _empty: String\n")
		b.WriteString("}\n")
		return
	}
	for _, f := range t.Fields {
		fmt.Fprintf(b, "  %s: %s", f.Name, typeRefStrFull(f.Type, f.Repeated, f.Required, f.ItemRequired))
		if f.Deprecated != "" {
			fmt.Fprintf(b, ` @deprecated(reason: "%s")`, escapeGraphQLString(f.Deprecated))
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
}

func renderGraphQLEnum(b *strings.Builder, t *Type) {
	fmt.Fprintf(b, "enum %s {\n", t.Name)
	for _, ev := range t.Enum {
		fmt.Fprintf(b, "  %s", ev.Name)
		if ev.Deprecated != "" {
			fmt.Fprintf(b, ` @deprecated(reason: "%s")`, escapeGraphQLString(ev.Deprecated))
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
}

func renderGraphQLUnion(b *strings.Builder, t *Type) {
	fmt.Fprintf(b, "union %s = %s\n", t.Name, strings.Join(t.Variants, " | "))
}

// renderGraphQLRoot emits one of Query/Mutation/Subscription with
// flat ops + group-container fields. typeName is "Query" /
// "Mutation" / "Subscription"; kindSfx is the matching suffix used
// to name container types ("Query" / "Mutation"; Subscriptions are
// flattened so this isn't called with "Subscription").
func renderGraphQLRoot(b *strings.Builder, typeName string, ops []*Operation, groups []*OperationGroup, kindSfx string) {
	fmt.Fprintf(b, "\ntype %s {\n", typeName)
	if len(ops) == 0 && len(groups) == 0 {
		b.WriteString("  _empty: String\n")
		b.WriteString("}\n")
		return
	}
	for _, op := range ops {
		renderGraphQLOp(b, op)
	}
	for _, g := range groups {
		containerName := pascalCase(g.Name) + kindSfx + "Namespace"
		fmt.Fprintf(b, "  %s: %s!\n", g.Name, containerName)
	}
	b.WriteString("}\n")
}

// renderGraphQLGroupContainer emits the synthesized type that backs
// one group, recursively descending into sub-groups. typeName is
// the already-computed name for this group's container; sub-group
// types extend it with PascalCase(child.Name).
func renderGraphQLGroupContainer(b *strings.Builder, g *OperationGroup, typeName string) {
	fmt.Fprintf(b, "\ntype %s {\n", typeName)
	if len(g.Operations) == 0 && len(g.Groups) == 0 {
		b.WriteString("  _empty: String\n")
		b.WriteString("}\n")
		return
	}
	ops := append([]*Operation(nil), g.Operations...)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
	for _, op := range ops {
		renderGraphQLOp(b, op)
	}
	subs := append([]*OperationGroup(nil), g.Groups...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	subContainerNames := make([]string, len(subs))
	for i, sub := range subs {
		subContainerNames[i] = typeName[:len(typeName)-len("Namespace")] + pascalCase(sub.Name) + "Namespace"
		fmt.Fprintf(b, "  %s: %s!\n", sub.Name, subContainerNames[i])
	}
	b.WriteString("}\n")
	for i, sub := range subs {
		renderGraphQLGroupContainer(b, sub, subContainerNames[i])
	}
}

// flattenSubscriptionGroup recursively pulls every operation out of
// a Subscription-rooted group, prefixing names with the group path
// joined by "_". graphql-go's executor doesn't support nested
// objects under Subscription, so the renderer surfaces them flat.
// Wrapper around the package-internal flattenGroupOps so the
// subscription path stays self-documenting.
func flattenSubscriptionGroup(g *OperationGroup, prefix string, out *[]*Operation) {
	flattenGroupOps(g, prefix, out)
}

func renderGraphQLOp(b *strings.Builder, op *Operation) {
	fmt.Fprintf(b, "  %s", op.Name)
	if len(op.Args) > 0 {
		b.WriteString("(")
		argParts := make([]string, 0, len(op.Args))
		for _, a := range op.Args {
			s := fmt.Sprintf("%s: %s", a.Name, typeRefStrFull(a.Type, a.Repeated, a.Required, a.ItemRequired))
			argParts = append(argParts, s)
		}
		b.WriteString(strings.Join(argParts, ", "))
		b.WriteString(")")
	}
	if op.Output != nil {
		fmt.Fprintf(b, ": %s", typeRefStrFull(*op.Output, op.OutputRepeated, op.OutputRequired, op.OutputItemRequired))
	} else {
		b.WriteString(": String")
	}
	if op.Deprecated != "" {
		fmt.Fprintf(b, ` @deprecated(reason: "%s")`, escapeGraphQLString(op.Deprecated))
	}
	b.WriteString("\n")
}

// kindSuffix maps an OpKind to the suffix used in synthesized
// container type names so a namespace under Query and the same
// namespace under Mutation produce distinct types.
func kindSuffix(k OpKind) string {
	switch k {
	case OpMutation:
		return "Mutation"
	default:
		return "Query"
	}
}

// pascalCase upper-cases the first rune of s. Multi-segment names
// (snake_case "v1_users") aren't normalized — the IR carries names
// as the source format wrote them, and the heuristic just makes
// the leading letter valid as a GraphQL type name.
func pascalCase(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// typeRefStrFull is the canonical wrapper formatter — Repeated
// wraps in [], Required wraps in !, and (when Repeated)
// ItemRequired wraps the inner element in !. Covers the common
// GraphQL slot shapes T / T! / [T] / [T]! / [T!] / [T!]!.
func typeRefStrFull(r TypeRef, repeated, required, itemRequired bool) string {
	return typeRefStr(r, repeated, required, itemRequired)
}

// typeRefStr formats a TypeRef as GraphQL syntax: applies List
// wrapping for repeated, NON_NULL wrapping for required, plus an
// inner NON_NULL when the list's items are non-null. Maps project
// to the canonical "JSON" custom scalar name (GraphQL has no
// native map shape).
func typeRefStr(r TypeRef, repeated, required, itemRequired bool) string {
	var inner string
	switch {
	case r.IsMap():
		inner = "JSON"
	case r.IsNamed():
		inner = r.Named
	default:
		switch r.Builtin {
		case ScalarBool:
			inner = "Boolean"
		case ScalarInt32, ScalarUInt32:
			inner = "Int"
		case ScalarInt64, ScalarUInt64:
			inner = "String" // graphql-go's Int is 32-bit
		case ScalarFloat, ScalarDouble:
			inner = "Float"
		case ScalarBytes, ScalarString, ScalarTimestamp:
			inner = "String"
		case ScalarID:
			inner = "ID"
		default:
			inner = "String"
		}
	}
	if repeated {
		if itemRequired {
			inner += "!"
		}
		inner = "[" + inner + "]"
	}
	if required {
		inner += "!"
	}
	return inner
}

// escapeGraphQLString escapes characters that aren't safe inside a
// GraphQL string literal. Conservative — covers backslash + double
// quote; full GraphQL string escaping (unicode escapes etc.) isn't
// needed for the @deprecated reasons we emit.
func escapeGraphQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
