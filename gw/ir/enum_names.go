package ir

import (
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// IsGraphQLEnumValueName reports whether s can be a GraphQL enum value
// as is: a GraphQL name (/^[_A-Za-z][_0-9A-Za-z]*$/), not reserved for
// introspection (leading "__"), and not true, false or null (the spec
// forbids those as enum values; graphql-go rejects them since v3.1.0).
//
// Stability: experimental
func IsGraphQLEnumValueName(s string) bool {
	if !isValidGraphQLIdent(s) || strings.HasPrefix(s, "__") {
		return false
	}
	switch s {
	case "true", "false", "null":
		return false
	}
	return true
}

// GraphQLEnumValueNames returns the GraphQL enum value name for each of
// names, in the same order. A name that IsGraphQLEnumValueName accepts
// is returned unchanged. Any other name is rewritten:
//
//   - true, false, null become TRUE, FALSE, NULL;
//   - each character outside [_A-Za-z0-9] becomes "_" (case is kept);
//   - a leading digit gets a "_" prefix, a leading "__" becomes "_",
//     and an empty name becomes "_";
//   - a result that another value of the same enum already uses gets
//     "_2", "_3", … appended.
//
// Rewritten names are assigned in sorted order of the original names,
// so the result depends only on the set of names, not on their order
// or on map iteration: every node of a cluster computes the same names.
// When no name needs a rewrite, names itself is returned.
//
// Only the GraphQL name changes. The value sent to and read from the
// upstream stays the original one.
//
// Stability: experimental
func GraphQLEnumValueNames(names []string) []string {
	bad := 0
	for _, n := range names {
		if !IsGraphQLEnumValueName(n) {
			bad++
		}
	}
	if bad == 0 {
		return names
	}
	taken := make(map[string]bool, len(names))
	var invalid []string
	for _, n := range names {
		if IsGraphQLEnumValueName(n) {
			taken[n] = true
		} else {
			invalid = append(invalid, n)
		}
	}
	sort.Strings(invalid)
	renamed := make(map[string]string, len(invalid))
	for _, n := range invalid {
		if _, done := renamed[n]; done {
			continue
		}
		base := sanitizeEnumValueName(n)
		name := base
		for i := 2; taken[name]; i++ {
			name = base + "_" + strconv.Itoa(i)
			if strings.HasPrefix(name, "__") { // base "_"
				name = "_" + strings.TrimLeft(name, "_")
			}
		}
		taken[name] = true
		renamed[n] = name
	}
	out := make([]string, len(names))
	for i, n := range names {
		if r, ok := renamed[n]; ok {
			out[i] = r
		} else {
			out[i] = n
		}
	}
	return out
}

func sanitizeEnumValueName(s string) string {
	switch s {
	case "true", "false", "null":
		return strings.ToUpper(s)
	}
	var b strings.Builder
	for _, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	switch {
	case out == "":
		return "_"
	case out[0] >= '0' && out[0] <= '9':
		return "_" + out
	case strings.HasPrefix(out, "__"):
		return "_" + strings.TrimLeft(out, "_")
	}
	return out
}

// EnumRenames returns, for each enum Type of svc that has a value
// GraphQLEnumValueNames rewrites, a map from the upstream value (the
// IR EnumValue.Name) to its GraphQL name. Keys are IR type names; only
// rewritten values are present. Nil when no enum needs a rewrite.
// Dispatchers that write upstream bytes straight to the response use
// it to emit the GraphQL name.
//
// Stability: experimental
func EnumRenames(svc *Service) map[string]map[string]string {
	if svc == nil {
		return nil
	}
	var out map[string]map[string]string
	for name, t := range svc.Types {
		if t.TypeKind != TypeEnum {
			continue
		}
		names := make([]string, len(t.Enum))
		for i, v := range t.Enum {
			names[i] = v.Name
		}
		gql := GraphQLEnumValueNames(names)
		for i := range names {
			if gql[i] == names[i] {
				continue
			}
			if out == nil {
				out = map[string]map[string]string{}
			}
			if out[name] == nil {
				out[name] = map[string]string{}
			}
			out[name][names[i]] = gql[i]
		}
	}
	return out
}

// ProtoEnumValueGraphQLName is the GraphQL name of a proto enum value:
// its descriptor name, rewritten as GraphQLEnumValueNames does over the
// enum's values when the name is not a GraphQL enum name (a proto value
// named true, false or null).
//
// Stability: experimental
func ProtoEnumValueGraphQLName(ev protoreflect.EnumValueDescriptor) string {
	name := string(ev.Name())
	if IsGraphQLEnumValueName(name) {
		return name
	}
	vals := ev.Parent().(protoreflect.EnumDescriptor).Values()
	names := make([]string, vals.Len())
	for i := range names {
		names[i] = string(vals.Get(i).Name())
	}
	return GraphQLEnumValueNames(names)[ev.Index()]
}
