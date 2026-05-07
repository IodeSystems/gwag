package ir

import (
	"fmt"
	"sort"
	"strings"
)

// RenderGraphQL emits SDL text for svc. The IR carries enough type
// info to render directly without going through a graphql.Schema —
// we just walk Service.Types + Service.Operations and string-build.
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

	// Operations group by Kind under Query / Mutation / Subscription.
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
	sort.Slice(queries, func(i, j int) bool { return queries[i].Name < queries[j].Name })
	sort.Slice(mutations, func(i, j int) bool { return mutations[i].Name < mutations[j].Name })
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })

	if hasQuery {
		b.WriteString("\ntype Query {\n")
		if len(queries) == 0 {
			b.WriteString("  _empty: String\n")
		}
		for _, op := range queries {
			renderGraphQLOp(&b, op)
		}
		b.WriteString("}\n")
	}
	if hasMut {
		b.WriteString("\ntype Mutation {\n")
		for _, op := range mutations {
			renderGraphQLOp(&b, op)
		}
		b.WriteString("}\n")
	}
	if hasSub {
		b.WriteString("\ntype Subscription {\n")
		for _, op := range subs {
			renderGraphQLOp(&b, op)
		}
		b.WriteString("}\n")
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
