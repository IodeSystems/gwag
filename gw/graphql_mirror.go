package gateway

import (
	"encoding/json"

	"github.com/graphql-go/graphql/language/ast"
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
// Name set to remoteName and any inline-fragment type-conditions
// in the selection tree un-prefixed (`<ns>_Cat` → `Cat`) so the
// remote sees its own type names. Nested field names pass through
// unchanged — the gateway only prefixes top-level names in the
// local schema. Argument lists and aliases pass through unchanged.
// Always uses ast.NewX constructors so the Kind fields the
// printer's visitor relies on are populated.
func (m *graphQLMirror) rewriteFieldForRemote(field *ast.Field, remoteName string) *ast.Field {
	out := ast.NewField(&ast.Field{
		Alias:      field.Alias,
		Name:       ast.NewName(&ast.Name{Value: remoteName}),
		Arguments:  field.Arguments,
		Directives: field.Directives,
	})
	if field.SelectionSet != nil {
		out.SelectionSet = m.rewriteSelectionSet(field.SelectionSet)
	}
	return out
}

func (m *graphQLMirror) rewriteSelectionSet(sel *ast.SelectionSet) *ast.SelectionSet {
	if sel == nil {
		return nil
	}
	out := ast.NewSelectionSet(&ast.SelectionSet{
		Selections: make([]ast.Selection, 0, len(sel.Selections)),
	})
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			cloned := ast.NewField(&ast.Field{
				Alias:      n.Alias,
				Name:       n.Name,
				Arguments:  n.Arguments,
				Directives: n.Directives,
			})
			if n.SelectionSet != nil {
				cloned.SelectionSet = m.rewriteSelectionSet(n.SelectionSet)
			}
			out.Selections = append(out.Selections, cloned)
		case *ast.InlineFragment:
			frag := ast.NewInlineFragment(&ast.InlineFragment{
				Directives: n.Directives,
			})
			if n.TypeCondition != nil && n.TypeCondition.Name != nil {
				frag.TypeCondition = ast.NewNamed(&ast.Named{
					Name: ast.NewName(&ast.Name{Value: m.unprefixTypeName(n.TypeCondition.Name.Value)}),
				})
			}
			if n.SelectionSet != nil {
				frag.SelectionSet = m.rewriteSelectionSet(n.SelectionSet)
			}
			out.Selections = append(out.Selections, frag)
		default:
			// Fragment spreads pass through unchanged. v1 callers don't
			// synthesise these locally — every selection the gateway
			// sees comes from rp.Info.FieldASTs which only carries
			// inline fragments. If a real client ever sends a named
			// fragment we'd need its definition forwarded too.
			out.Selections = append(out.Selections, s)
		}
	}
	return out
}

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
