package gateway

import (
	"strings"
	"unicode"
)

// exportedName converts a proto full name like "auth.v1.Context" into a
// GraphQL-safe type name like "Auth_V1_Context".
func exportedName(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	return strings.Join(parts, "_")
}

func lowerCamel(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "_")
	for i := range parts {
		if i == 0 {
			continue
		}
		if parts[i] == "" {
			continue
		}
		r := []rune(parts[i])
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	r := []rune(parts[0])
	if len(r) > 0 {
		r[0] = unicode.ToLower(r[0])
		parts[0] = string(r)
	}
	return strings.Join(parts, "")
}
