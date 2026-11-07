package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
)

// graphQLMirror is the per-source AST helper used by graphQLDispatcher
// to forward the caller's selection set to a downstream GraphQL
// service. The pre-cutover graphQLMirror also did type construction;
// that role moved to RenderGraphQLRuntime via IRTypeBuilder, leaving
// only the AST rewrite (rewriteFieldForRemote / unprefixTypeName)
// here. The mirror's lifetime tracks the schema build — a fresh mirror
// per source per build, captured by the dispatcher.
type graphQLMirror struct {
	src *graphQLSource
	// isLatest controls which prefix unprefixTypeName strips off
	// inline-fragment type conditions: "<ns>_" for latest, otherwise
	// "<ns>_<vN>_". Set by registerGraphQLDispatchersLocked after
	// construction so a single mirror can serve any version's
	// dispatchers in the registry.
	isLatest bool
}

func newGraphQLMirror(src *graphQLSource) *graphQLMirror {
	return &graphQLMirror{src: src, isLatest: true}
}

// rewriteFieldForRemote returns a clone of the AST field with its
// Name set to remoteName and the selection tree rewritten by
// rewriteSelectionSet. Argument lists and aliases pass through
// unchanged. `more` holds further ASTs for the same response key
// ({ f { a } f { b } }); their selections merge into one. Always
// uses ast.NewX constructors so the Kind fields the printer's
// visitor relies on are populated.
func (m *graphQLMirror) rewriteFieldForRemote(field *ast.Field, remoteName string, more []*ast.Field, frags map[string]ast.Definition) *ast.Field {
	out := ast.NewField(&ast.Field{
		Alias:      field.Alias,
		Name:       ast.NewName(&ast.Name{Value: remoteName}),
		Arguments:  field.Arguments,
		Directives: field.Directives,
	})
	out.SelectionSet = m.rewriteSelectionSet(mergeSelectionSets(field, more), frags)
	return out
}

// mergeSelectionSets returns the selections of field and of every
// field in more, concatenated. The upstream merges repeated
// selections the way the gateway would (validation §5.3.2 proved
// they can merge).
func mergeSelectionSets(field *ast.Field, more []*ast.Field) *ast.SelectionSet {
	if len(more) == 0 {
		return field.SelectionSet
	}
	var sels []ast.Selection
	for _, f := range append([]*ast.Field{field}, more...) {
		if f.SelectionSet != nil {
			sels = append(sels, f.SelectionSet.Selections...)
		}
	}
	if sels == nil {
		return nil
	}
	return ast.NewSelectionSet(&ast.SelectionSet{Selections: sels})
}

// rewriteSelectionSet clones sel for the remote:
//   - inline-fragment type conditions lose the local prefix
//     (`<ns>_Cat` → `Cat`) so the remote sees its own type names;
//   - named fragment spreads become inline fragments, because the
//     forwarded document holds only the operation, not the caller's
//     fragment definitions;
//   - an aliased __typename gets its alias marked with
//     typenameAliasPrefix, so prefixResponseTypenames can find its
//     value in the response.
//
// Nested field names pass through unchanged — the gateway only
// prefixes top-level names in the local schema.
func (m *graphQLMirror) rewriteSelectionSet(sel *ast.SelectionSet, frags map[string]ast.Definition) *ast.SelectionSet {
	if sel == nil {
		return nil
	}
	out := ast.NewSelectionSet(&ast.SelectionSet{
		Selections: make([]ast.Selection, 0, len(sel.Selections)),
	})
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			alias := n.Alias
			if alias != nil && alias.Value != "" && n.Name != nil && n.Name.Value == "__typename" {
				alias = ast.NewName(&ast.Name{Value: typenameAliasPrefix + alias.Value})
			}
			cloned := ast.NewField(&ast.Field{
				Alias:      alias,
				Name:       n.Name,
				Arguments:  n.Arguments,
				Directives: n.Directives,
			})
			if n.SelectionSet != nil {
				cloned.SelectionSet = m.rewriteSelectionSet(n.SelectionSet, frags)
			}
			out.Selections = append(out.Selections, cloned)
		case *ast.InlineFragment:
			out.Selections = append(out.Selections, m.rewriteInlineFragment(n.TypeCondition, n.Directives, n.SelectionSet, frags))
		case *ast.FragmentSpread:
			var def *ast.FragmentDefinition
			if n.Name != nil {
				def, _ = frags[n.Name.Value].(*ast.FragmentDefinition)
			}
			if def == nil {
				// Unknown fragment: graphql-go's validation rejects
				// these before dispatch. Forward unchanged.
				out.Selections = append(out.Selections, s)
				continue
			}
			out.Selections = append(out.Selections, m.rewriteInlineFragment(def.TypeCondition, n.Directives, def.SelectionSet, frags))
		default:
			out.Selections = append(out.Selections, s)
		}
	}
	return out
}

func (m *graphQLMirror) rewriteInlineFragment(cond *ast.Named, dirs []*ast.Directive, sel *ast.SelectionSet, frags map[string]ast.Definition) *ast.InlineFragment {
	frag := ast.NewInlineFragment(&ast.InlineFragment{
		Directives: dirs,
	})
	if cond != nil && cond.Name != nil {
		frag.TypeCondition = ast.NewNamed(&ast.Named{
			Name: ast.NewName(&ast.Name{Value: m.unprefixTypeName(cond.Name.Value)}),
		})
	}
	frag.SelectionSet = m.rewriteSelectionSet(sel, frags)
	return frag
}

// typenameAliasPrefix marks an aliased __typename in a forwarded
// query (`t: __typename` goes upstream as `__gwtn_t: __typename`).
// prefixResponseTypenames strips the mark from the response key and
// prefixes the value. A caller alias that itself starts with this
// prefix would be misread; "__" aliases are legal but unusual.
const typenameAliasPrefix = "__gwtn_"

// unprefixTypeName strips the source's "<ns>_" or "<ns>_<vN>_" prefix
// from a local type name. Returns the name unchanged when there's no
// match — the rewriter is best-effort, and a non-prefixed name is
// either a built-in scalar or an introspection mismatch we forward
// verbatim.
func (m *graphQLMirror) unprefixTypeName(name string) string {
	if !m.isLatest {
		long := m.src.namespace + "_" + m.src.version + "_"
		if len(name) > len(long) && name[:len(long)] == long {
			return name[len(long):]
		}
	}
	short := m.src.namespace + "_"
	if len(name) > len(short) && name[:len(short)] == short {
		return name[len(short):]
	}
	return name
}

// jsonUnmarshalLoose decodes the upstream's `data` envelope into a
// generic map. Numbers stay as float64 — graphql-go's coerceInt
// does not match json.Number (typed-string), so UseNumber would
// silently null out every Int field on the response. ID values
// large enough to lose float64 precision should be carried as
// strings on the wire (which is the conventional GraphQL ID shape
// anyway).
func jsonUnmarshalLoose(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// prefixResponseTypenames rewrites every `"__typename":"<value>"`
// occurrence in `src` so the value is prepended with the gateway's
// namespace prefix (matching what mirror.unprefixTypeName strips on
// the way out). Needed on the DispatchAppend path: the fork's
// ExecutePlanAppend walker bypasses per-field resolution for fields
// that set ResolveAppend, so graphql-go's __typename machinery never
// fires — the upstream's bare type name would leak into the local
// response unless we patch it here.
//
// An aliased __typename comes back under its marked alias
// (typenameAliasPrefix, set by rewriteSelectionSet): the mark is
// stripped from the key and the value prefixed the same way.
//
// The scanner tracks string-literal context so a string VALUE that
// literally contains the substring `"__typename":"..."` doesn't get
// rewritten (rare in practice — JSON would have escaped the inner
// quotes, but defensive). Numbers, true/false/null on the value side
// are not type names; the scanner requires a quoted value.
func (m *graphQLMirror) prefixResponseTypenames(src []byte) []byte {
	// Both key forms start with `"__`. Most responses select no
	// __typename: return them without a copy.
	if !bytes.Contains(src, []byte(`"__`)) {
		return src
	}
	prefix := m.src.namespace + "_"
	out := make([]byte, 0, len(src)+8)
	i := 0
	for i < len(src) {
		// Find next candidate key position: a `"__typename"` or marked
		// alias key at a JSON-key slot, preceded by `{` or `,` (with
		// optional whitespace) and followed by `:` (with optional
		// whitespace).
		start, end, alias := indexTypenameKey(src[i:])
		if start < 0 {
			out = append(out, src[i:]...)
			break
		}
		if alias {
			// Emit the key without the mark.
			out = append(out, src[i:i+start+1]...)
			out = append(out, src[i+start+1+len(typenameAliasPrefix):i+end]...)
		} else {
			out = append(out, src[i:i+end]...)
		}
		i += end
		// Skip optional whitespace.
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
			out = append(out, src[i])
			i++
		}
		// Expect colon.
		if i >= len(src) || src[i] != ':' {
			continue
		}
		out = append(out, src[i])
		i++
		// Skip optional whitespace after colon.
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
			out = append(out, src[i])
			i++
		}
		// Expect quoted value.
		if i >= len(src) || src[i] != '"' {
			continue
		}
		// Emit opening quote, then prefix, then the rest of the value
		// up to the closing quote.
		out = append(out, '"')
		out = append(out, prefix...)
		i++
		for i < len(src) {
			c := src[i]
			if c == '\\' && i+1 < len(src) {
				out = append(out, c, src[i+1])
				i += 2
				continue
			}
			out = append(out, c)
			i++
			if c == '"' {
				break
			}
		}
	}
	return out
}

// indexTypenameKey finds the first JSON-object key in src that is
// `"__typename"` or a typenameAliasPrefix-marked alias — preceded by
// `{` or `,` with optional whitespace, and not inside a string
// literal. It returns the offsets of the key's opening quote and one
// past its closing quote, and whether the key is a marked alias.
// start is -1 when there is no such key.
//
// The scanner is byte-level; doesn't allocate.
func indexTypenameKey(src []byte) (start, end int, alias bool) {
	const typename = `"__typename"`
	const mark = `"` + typenameAliasPrefix
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString:
			if c == '\\' && i+1 < len(src) {
				i++ // skip escaped char
				continue
			}
			if c == '"' {
				inString = false
			}
		case c == '"':
			// A key slot: the preceding non-whitespace byte is `{`
			// or `,`.
			pos := i - 1
			for pos >= 0 && (src[pos] == ' ' || src[pos] == '\t' || src[pos] == '\n' || src[pos] == '\r') {
				pos--
			}
			if pos < 0 || src[pos] == '{' || src[pos] == ',' {
				rest := src[i:]
				if bytes.HasPrefix(rest, []byte(typename)) {
					return i, i + len(typename), false
				}
				if bytes.HasPrefix(rest, []byte(mark)) {
					// GraphQL names hold no quote or escape.
					if j := bytes.IndexByte(rest[len(mark):], '"'); j >= 0 {
						return i, i + len(mark) + j + 1, true
					}
				}
			}
			inString = true
		}
	}
	return -1, -1, false
}
