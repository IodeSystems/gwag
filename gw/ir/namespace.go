package ir

import "strings"

// SanitizeNamespace projects a free-form string — typically an OpenAPI
// spec's info.title — into a GraphQL-valid namespace identifier. The
// source string's case is preserved: a spec titled "Pets" yields
// "Pets", not "pets". Spaces and dashes become underscores so word
// boundaries survive ("Pet Store" → "Pet_Store"); every other rune
// that isn't valid in a GraphQL identifier is dropped. A leading
// digit is prefixed with `_` so the result satisfies
// /[_A-Za-z][_0-9A-Za-z]*/.
//
// Returns "" when nothing identifier-valid remains — callers supply
// their own fallback (the spec filename stem, a literal default).
//
// This is the single namespace-derivation rule shared by every
// ingest path (library AddOpenAPI, the gat translator, the gwag serve
// CLI) so the same spec produces the same namespace regardless of how
// it was registered.
//
// Stability: stable
func SanitizeNamespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '-':
			b.WriteRune('_')
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		return "_" + out
	}
	return out
}
