package ir

import (
	"encoding/json"
	"strings"
)

// refMarker is the Doxygen-extension tag tslsmcp scans for to link a
// generated GraphQL field / OpenAPI operation / proto method back to
// its source-of-truth. Shape: `@ref <workspace-relative-path>[:<symbol>]`.
//
// The marker is author-authored at the upstream source (a proto leading
// comment, an OpenAPI `x-ref` extension, or a GraphQL description line)
// and carried verbatim across every format the gateway emits. The
// gateway never sees the ultimate Go source, so it *propagates* the
// reference rather than synthesizing it.
const refMarker = "@ref"

// xRefExtension is the OpenAPI extension key carrying the same marker.
const xRefExtension = "x-ref"

// splitRef pulls the first `@ref ...` line out of a doc comment /
// description, returning the description with that marker line removed
// and the ref payload (the text after `@ref`). ref is "" when no marker
// is present, in which case desc is returned unchanged. Only the first
// marker is consumed — multi-source `@ref` carriage is deferred until a
// concrete case appears.
func splitRef(desc string) (clean, ref string) {
	if !strings.Contains(desc, refMarker) {
		return desc, ""
	}
	lines := strings.Split(desc, "\n")
	out := lines[:0:0]
	for _, line := range lines {
		if ref == "" {
			if payload, ok := matchRef(line); ok {
				ref = payload
				continue // drop the marker line from the description
			}
		}
		out = append(out, line)
	}
	if ref == "" {
		return desc, ""
	}
	return strings.TrimSpace(strings.Join(out, "\n")), ref
}

// matchRef reports whether a single line is an `@ref <payload>` marker
// (ignoring surrounding whitespace and the comment artifacts protoc
// leaves on leading comments) and returns the trimmed payload. The
// character after `@ref` must be whitespace so `@reference` doesn't
// match.
func matchRef(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, refMarker) {
		return "", false
	}
	rest := t[len(refMarker):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	if rest = strings.TrimSpace(rest); rest == "" {
		return "", false
	}
	return rest, true
}

// withRef re-appends a `@ref` marker to a rendered description in the
// canonical shape (description, blank line, marker). Returns desc
// unchanged when ref is empty.
func withRef(desc, ref string) string {
	if ref == "" {
		return desc
	}
	desc = strings.TrimRight(desc, "\n")
	if desc == "" {
		return refMarker + " " + ref
	}
	return desc + "\n\n" + refMarker + " " + ref
}

// extString reads a string-valued extension off an OpenAPI Extensions
// map. JSON/YAML loads decode `x-ref` to a native string; the
// json.RawMessage branch covers extensions populated by other loaders.
func extString(ext map[string]any, key string) string {
	v, ok := ext[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case json.RawMessage:
		var out string
		if json.Unmarshal(s, &out) == nil {
			return out
		}
	case []byte:
		var out string
		if json.Unmarshal(s, &out) == nil {
			return out
		}
	}
	return ""
}
