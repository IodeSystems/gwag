package ir

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Annotation is one SDL-visible directive an upstream author attaches to
// an operation / type / field. Carried verbatim from the contract that
// declares it (OpenAPI `x-gwag-annotations`, proto `@gql` leading
// comment) into every egress format — GraphQL SDL directives, OpenAPI
// `x-gwag-annotations`, proto `@gql` comments. Metadata only; gwag never
// executes them (runtime policy lives in transforms + providers).
//
// Stability: experimental
type Annotation struct {
	Name string
	Args []AnnotationArg
}

// AnnotationArg is one named argument on an Annotation. Kind drives how
// Value renders per format (quoted string vs bareword vs number/bool).
//
// Stability: experimental
type AnnotationArg struct {
	Name  string
	Kind  AnnKind
	Value string
}

// AnnKind tags an argument value's lexical type so each emitter renders
// it correctly.
//
// Stability: experimental
type AnnKind uint8

const (
	AnnString AnnKind = iota // quoted in GraphQL/proto, JSON string in OpenAPI
	AnnNumber                // bareword everywhere, JSON number in OpenAPI
	AnnBool                  // bareword everywhere, JSON bool in OpenAPI
	AnnIdent                 // GraphQL enum/bareword; degrades to JSON string in OpenAPI
)

// gqlMarker is the proto leading-comment tag introducing one annotation:
// `@gql Name(arg: value, ...)` or `@gql Name`.
const gqlMarker = "@gql"

// xAnnotationsExtension is the OpenAPI extension key carrying annotations.
const xAnnotationsExtension = "x-gwag-annotations"

// gqlValue renders one arg value in GraphQL literal syntax.
func (a AnnotationArg) gqlValue() string {
	switch a.Kind {
	case AnnString:
		return strconv.Quote(a.Value)
	default:
		return a.Value
	}
}

// gqlType is the best-effort GraphQL input type for a synthesized
// directive-argument declaration.
func (a AnnotationArg) gqlType() string {
	switch a.Kind {
	case AnnNumber:
		return "Float"
	case AnnBool:
		return "Boolean"
	default:
		return "String"
	}
}

// gql renders the annotation as a GraphQL directive application, e.g.
// ` @hasRole(role: "ADMIN")` (leading space included for inline use).
func (an Annotation) gql() string {
	var b strings.Builder
	b.WriteString(" @")
	b.WriteString(an.Name)
	if len(an.Args) > 0 {
		b.WriteString("(")
		for i, a := range an.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Name)
			b.WriteString(": ")
			b.WriteString(a.gqlValue())
		}
		b.WriteString(")")
	}
	return b.String()
}

// gqlAll renders a list of annotations as concatenated directive
// applications, sorted by name for determinism.
func gqlAnnotations(anns []Annotation) string {
	if len(anns) == 0 {
		return ""
	}
	sorted := append([]Annotation(nil), anns...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, an := range sorted {
		b.WriteString(an.gql())
	}
	return b.String()
}

// protoComment renders the annotation as a `@gql` leading-comment line
// (no trailing newline), the inverse of parseGqlDirective.
func (an Annotation) protoComment() string {
	var b strings.Builder
	b.WriteString(gqlMarker)
	b.WriteString(" ")
	b.WriteString(an.Name)
	if len(an.Args) > 0 {
		b.WriteString("(")
		for i, a := range an.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Name)
			b.WriteString(": ")
			switch a.Kind {
			case AnnString:
				b.WriteString(strconv.Quote(a.Value))
			default:
				b.WriteString(a.Value)
			}
		}
		b.WriteString(")")
	}
	return b.String()
}

// splitGqlAnnotations pulls every `@gql ...` line out of a proto leading
// comment, returning the comment with those lines removed and the parsed
// annotations in source order. Lines that start with `@gql` but don't
// parse are dropped from the comment and skipped (best-effort).
func splitGqlAnnotations(comment string) (clean string, anns []Annotation) {
	if !strings.Contains(comment, gqlMarker) {
		return comment, nil
	}
	lines := strings.Split(comment, "\n")
	out := lines[:0:0]
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if rest, ok := afterMarker(t, gqlMarker); ok {
			if an, ok := parseGqlDirective(rest); ok {
				anns = append(anns, an)
			}
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n")), anns
}

// afterMarker reports whether t begins with marker followed by
// whitespace, returning the trimmed remainder.
func afterMarker(t, marker string) (string, bool) {
	if !strings.HasPrefix(t, marker) {
		return "", false
	}
	rest := t[len(marker):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// parseGqlDirective parses `Name` or `Name(arg: value, ...)` into an
// Annotation. Values are GraphQL scalar literals: quoted strings,
// numbers, true/false, or barewords (enum/ident).
func parseGqlDirective(s string) (Annotation, bool) {
	s = strings.TrimSpace(s)
	name, rest := scanIdent(s)
	if name == "" {
		return Annotation{}, false
	}
	an := Annotation{Name: name}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return an, true
	}
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return Annotation{}, false
	}
	inner := strings.TrimSpace(rest[1 : len(rest)-1])
	if inner == "" {
		return an, true
	}
	for _, pair := range splitArgs(inner) {
		key, raw, ok := strings.Cut(pair, ":")
		if !ok {
			return Annotation{}, false
		}
		argName := strings.TrimSpace(key)
		val := strings.TrimSpace(raw)
		if argName == "" || val == "" {
			return Annotation{}, false
		}
		an.Args = append(an.Args, AnnotationArg{Name: argName, Kind: classifyValue(val), Value: unquoteValue(val)})
	}
	return an, true
}

// scanIdent peels a leading GraphQL identifier off s, returning it and
// the remainder.
func scanIdent(s string) (string, string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

// splitArgs splits a comma-separated argument list, respecting double-
// quoted string values so commas inside strings don't split.
func splitArgs(s string) []string {
	var parts []string
	var cur strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && (i == 0 || s[i-1] != '\\'):
			inStr = !inStr
			cur.WriteByte(c)
		case c == ',' && !inStr:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		parts = append(parts, cur.String())
	}
	return parts
}

func classifyValue(v string) AnnKind {
	switch {
	case strings.HasPrefix(v, `"`):
		return AnnString
	case v == "true" || v == "false":
		return AnnBool
	default:
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return AnnNumber
		}
		return AnnIdent
	}
}

func unquoteValue(v string) string {
	if strings.HasPrefix(v, `"`) {
		if uq, err := strconv.Unquote(v); err == nil {
			return uq
		}
		return strings.Trim(v, `"`)
	}
	return v
}

// annotationsToExt encodes annotations as the OpenAPI `x-gwag-annotations`
// JSON shape: [{name, args: {k: v}}]. Ident args degrade to JSON strings.
func annotationsToExt(anns []Annotation) []any {
	out := make([]any, 0, len(anns))
	for _, an := range anns {
		entry := map[string]any{"name": an.Name}
		if len(an.Args) > 0 {
			args := make(map[string]any, len(an.Args))
			for _, a := range an.Args {
				switch a.Kind {
				case AnnNumber:
					if f, err := strconv.ParseFloat(a.Value, 64); err == nil {
						args[a.Name] = f
					} else {
						args[a.Name] = a.Value
					}
				case AnnBool:
					args[a.Name] = a.Value == "true"
				default:
					args[a.Name] = a.Value
				}
			}
			entry["args"] = args
		}
		out = append(out, entry)
	}
	return out
}

// protoMetaComment builds the leading-comment body for a synthesized
// proto method/element from its `@ref` marker and `@gql` annotations,
// one per line with a trailing newline (the shape protoprint expects).
// Returns "" when there's nothing to carry.
func protoMetaComment(ref string, anns []Annotation) string {
	var lines []string
	if ref != "" {
		lines = append(lines, refMarker+" "+ref)
	}
	for _, an := range anns {
		lines = append(lines, an.protoComment())
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// splitMeta peels both the `@ref` marker and `@gql` annotation lines off
// a proto leading comment, returning the cleaned description plus the
// extracted metadata. The single entry point proto ingest uses so the
// per-descriptor sites stay one line.
func splitMeta(comment string) (clean, ref string, anns []Annotation) {
	clean, ref = splitRef(comment)
	clean, anns = splitGqlAnnotations(clean)
	return clean, ref, anns
}

// extAnnotations reads + decodes the `x-gwag-annotations` extension off
// an OpenAPI Extensions map.
func extAnnotations(ext map[string]any) []Annotation {
	v, ok := ext[xAnnotationsExtension]
	if !ok {
		return nil
	}
	return annotationsFromExt(v)
}

// annotationsFromExt decodes the `x-gwag-annotations` extension value
// (as produced by a JSON/YAML loader) into Annotations. Unparseable
// entries are skipped.
func annotationsFromExt(v any) []Annotation {
	raw := v
	if rm, ok := v.(json.RawMessage); ok {
		var decoded any
		if err := json.Unmarshal(rm, &decoded); err != nil {
			return nil
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var anns []Annotation
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		an := Annotation{Name: name}
		if args, ok := m["args"].(map[string]any); ok {
			names := make([]string, 0, len(args))
			for k := range args {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				an.Args = append(an.Args, jsonArg(k, args[k]))
			}
		}
		anns = append(anns, an)
	}
	return anns
}

func jsonArg(name string, v any) AnnotationArg {
	switch x := v.(type) {
	case string:
		return AnnotationArg{Name: name, Kind: AnnString, Value: x}
	case bool:
		return AnnotationArg{Name: name, Kind: AnnBool, Value: strconv.FormatBool(x)}
	case float64:
		return AnnotationArg{Name: name, Kind: AnnNumber, Value: strconv.FormatFloat(x, 'g', -1, 64)}
	case json.Number:
		return AnnotationArg{Name: name, Kind: AnnNumber, Value: x.String()}
	default:
		return AnnotationArg{Name: name, Kind: AnnString, Value: fmt.Sprintf("%v", v)}
	}
}

// AnnotationIndex maps the final (printed) GraphQL type / field names to
// the annotations that decorate them, and accumulates the directive
// declarations the rendered SDL must include to stay valid. Populated
// during the runtime build (via RuntimeOptions.AnnotationSink) and
// consumed by PrintSchemaSDL.
//
// Stability: experimental
type AnnotationIndex struct {
	typeAnns  map[string][]Annotation
	fieldAnns map[string]map[string][]Annotation
	// decls[directiveName][argName] = gql arg type, accumulated across
	// every usage so one declaration covers them all.
	decls map[string]map[string]string
}

// NewAnnotationIndex returns an empty index ready to pass as
// RuntimeOptions.AnnotationSink.
//
// Stability: experimental
func NewAnnotationIndex() *AnnotationIndex {
	return &AnnotationIndex{
		typeAnns:  map[string][]Annotation{},
		fieldAnns: map[string]map[string][]Annotation{},
		decls:     map[string]map[string]string{},
	}
}

func (idx *AnnotationIndex) recordType(typeName string, anns []Annotation) {
	if idx == nil || len(anns) == 0 {
		return
	}
	idx.typeAnns[typeName] = anns
	idx.accrue(anns)
}

func (idx *AnnotationIndex) recordField(typeName, fieldName string, anns []Annotation) {
	if idx == nil || len(anns) == 0 {
		return
	}
	if idx.fieldAnns[typeName] == nil {
		idx.fieldAnns[typeName] = map[string][]Annotation{}
	}
	idx.fieldAnns[typeName][fieldName] = anns
	idx.accrue(anns)
}

func (idx *AnnotationIndex) accrue(anns []Annotation) {
	for _, an := range anns {
		if idx.decls[an.Name] == nil {
			idx.decls[an.Name] = map[string]string{}
		}
		for _, a := range an.Args {
			// String wins on conflict (most permissive).
			if existing, ok := idx.decls[an.Name][a.Name]; !ok || existing != "String" {
				idx.decls[an.Name][a.Name] = a.gqlType()
			}
		}
	}
}

func (idx *AnnotationIndex) typeAnnotations(typeName string) []Annotation {
	if idx == nil {
		return nil
	}
	return idx.typeAnns[typeName]
}

func (idx *AnnotationIndex) fieldAnnotations(typeName, fieldName string) []Annotation {
	if idx == nil {
		return nil
	}
	if m := idx.fieldAnns[typeName]; m != nil {
		return m[fieldName]
	}
	return nil
}

// declarations renders the synthesized `directive @Name(...) on ...`
// definitions, one per distinct annotation name, sorted. A permissive
// location set keeps every usage valid without tracking per-site
// locations.
func (idx *AnnotationIndex) declarations() []string {
	if idx == nil || len(idx.decls) == 0 {
		return nil
	}
	const locations = "FIELD_DEFINITION | OBJECT | INPUT_OBJECT | INPUT_FIELD_DEFINITION | ENUM | SCALAR | UNION | INTERFACE | ARGUMENT_DEFINITION"
	names := make([]string, 0, len(idx.decls))
	for n := range idx.decls {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		args := idx.decls[n]
		var sb strings.Builder
		sb.WriteString("directive @")
		sb.WriteString(n)
		if len(args) > 0 {
			argNames := make([]string, 0, len(args))
			for a := range args {
				argNames = append(argNames, a)
			}
			sort.Strings(argNames)
			sb.WriteString("(")
			for i, a := range argNames {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(a)
				sb.WriteString(": ")
				sb.WriteString(args[a])
			}
			sb.WriteString(")")
		}
		sb.WriteString(" on ")
		sb.WriteString(locations)
		out = append(out, sb.String())
	}
	return out
}
