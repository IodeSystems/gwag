package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/graphql-go/graphql"
)

// introspectionQuery is the canonical GraphQL introspection query —
// what every codegen tool sends to learn a server's schema. Mirrors
// graphql-js' getIntrospectionQuery (without descriptions for brevity).
const introspectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives {
      name
      description
      locations
      args { ...InputValue }
    }
  }
}
fragment FullType on __Type {
  kind
  name
  description
  fields(includeDeprecated: true) {
    name
    description
    args { ...InputValue }
    type { ...TypeRef }
    isDeprecated
    deprecationReason
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) {
    name
    description
    isDeprecated
    deprecationReason
  }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue {
  name
  description
  type { ...TypeRef }
  defaultValue
}
fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType {
        kind
        name
        ofType {
          kind
          name
          ofType { kind name ofType { kind name ofType { kind name } } }
        }
      }
    }
  }
}`

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printSchemaSDL walks a graphql.Schema and emits a GraphQL SDL string
// representing it. Covers what this gateway emits: object types, input
// objects, enums, scalars, and field deprecation. Runtime concerns
// (resolvers, defaults beyond String/Int/Bool/Enum) are not reflected.
func printSchemaSDL(s *graphql.Schema) string {
	var b strings.Builder
	tm := s.TypeMap()

	names := make([]string, 0, len(tm))
	for n := range tm {
		// Skip the introspection types (__Schema etc.) and built-in
		// scalars; they're implicit in every SDL document.
		if strings.HasPrefix(n, "__") {
			continue
		}
		switch n {
		case "Int", "Float", "String", "Boolean", "ID":
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	first := true
	separate := func() {
		if !first {
			b.WriteString("\n")
		}
		first = false
	}

	// Schema declaration includes whichever roots graphql-go assembled.
	q := s.QueryType()
	sub := s.SubscriptionType()
	mut := s.MutationType()
	if q != nil || sub != nil || mut != nil {
		separate()
		b.WriteString("schema {\n")
		if q != nil {
			b.WriteString("  query: ")
			b.WriteString(q.Name())
			b.WriteString("\n")
		}
		if mut != nil {
			b.WriteString("  mutation: ")
			b.WriteString(mut.Name())
			b.WriteString("\n")
		}
		if sub != nil {
			b.WriteString("  subscription: ")
			b.WriteString(sub.Name())
			b.WriteString("\n")
		}
		b.WriteString("}\n")
	}

	for _, name := range names {
		switch t := tm[name].(type) {
		case *graphql.Object:
			separate()
			writeObject(&b, t)
		case *graphql.InputObject:
			separate()
			writeInputObject(&b, t)
		case *graphql.Enum:
			separate()
			writeEnum(&b, t)
		case *graphql.Scalar:
			separate()
			writeScalar(&b, t)
		case *graphql.Interface:
			separate()
			writeInterface(&b, t)
		case *graphql.Union:
			separate()
			writeUnion(&b, t)
		}
	}
	return b.String()
}

func writeDescription(b *strings.Builder, indent, desc string) {
	if desc == "" {
		return
	}
	for _, line := range strings.Split(desc, "\n") {
		b.WriteString(indent)
		b.WriteString("\"\"\"")
		b.WriteString(line)
		b.WriteString("\"\"\"\n")
	}
}

func writeObject(b *strings.Builder, o *graphql.Object) {
	writeDescription(b, "", o.Description())
	b.WriteString("type ")
	b.WriteString(o.Name())
	b.WriteString(" {\n")
	writeFields(b, o.Fields())
	b.WriteString("}\n")
}

func writeInterface(b *strings.Builder, o *graphql.Interface) {
	writeDescription(b, "", o.Description())
	b.WriteString("interface ")
	b.WriteString(o.Name())
	b.WriteString(" {\n")
	writeFields(b, o.Fields())
	b.WriteString("}\n")
}

func writeUnion(b *strings.Builder, u *graphql.Union) {
	writeDescription(b, "", u.Description())
	b.WriteString("union ")
	b.WriteString(u.Name())
	b.WriteString(" =")
	for i, t := range u.Types() {
		if i > 0 {
			b.WriteString(" |")
		}
		b.WriteString(" ")
		b.WriteString(t.Name())
	}
	b.WriteString("\n")
}

func writeFields(b *strings.Builder, fields graphql.FieldDefinitionMap) {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := fields[n]
		writeDescription(b, "  ", f.Description)
		b.WriteString("  ")
		b.WriteString(f.Name)
		if len(f.Args) > 0 {
			writeArgs(b, f.Args)
		}
		b.WriteString(": ")
		b.WriteString(typeRef(f.Type))
		if f.DeprecationReason != "" {
			b.WriteString(" @deprecated(reason: ")
			b.WriteString(quoteString(f.DeprecationReason))
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
}

func writeArgs(b *strings.Builder, args []*graphql.Argument) {
	b.WriteString("(")
	for i, a := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Name())
		b.WriteString(": ")
		b.WriteString(typeRef(a.Type))
		if a.DefaultValue != nil {
			b.WriteString(" = ")
			b.WriteString(formatDefault(a.DefaultValue))
		}
	}
	b.WriteString(")")
}

func writeInputObject(b *strings.Builder, io *graphql.InputObject) {
	writeDescription(b, "", io.Description())
	b.WriteString("input ")
	b.WriteString(io.Name())
	b.WriteString(" {\n")
	fields := io.Fields()
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := fields[n]
		writeDescription(b, "  ", f.Description())
		b.WriteString("  ")
		b.WriteString(f.Name())
		b.WriteString(": ")
		b.WriteString(typeRef(f.Type))
		if dv := f.DefaultValue; dv != nil {
			b.WriteString(" = ")
			b.WriteString(formatDefault(dv))
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
}

func writeEnum(b *strings.Builder, e *graphql.Enum) {
	writeDescription(b, "", e.Description())
	b.WriteString("enum ")
	b.WriteString(e.Name())
	b.WriteString(" {\n")
	values := append([]*graphql.EnumValueDefinition(nil), e.Values()...)
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	for _, v := range values {
		writeDescription(b, "  ", v.Description)
		b.WriteString("  ")
		b.WriteString(v.Name)
		if v.DeprecationReason != "" {
			b.WriteString(" @deprecated(reason: ")
			b.WriteString(quoteString(v.DeprecationReason))
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
}

func writeScalar(b *strings.Builder, s *graphql.Scalar) {
	writeDescription(b, "", s.Description())
	b.WriteString("scalar ")
	b.WriteString(s.Name())
	b.WriteString("\n")
}

// typeRef formats a graphql.Output as its SDL type reference: lists are
// [T] and non-null is T!.
func typeRef(t graphql.Type) string {
	switch tt := t.(type) {
	case *graphql.NonNull:
		return typeRef(tt.OfType) + "!"
	case *graphql.List:
		return "[" + typeRef(tt.OfType) + "]"
	default:
		return t.Name()
	}
}

func formatDefault(v any) string {
	switch x := v.(type) {
	case string:
		return quoteString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int, int32, int64, float32, float64:
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func quoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
