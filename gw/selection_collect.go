package gateway

import (
	"github.com/IodeSystems/graphql-go/v3"
	"github.com/IodeSystems/graphql-go/v3/gqlerrors"
	"github.com/IodeSystems/graphql-go/v3/language/ast"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// plainSelectionMax bounds plainSelection and the stack buffers
// the append walkers scan into.
const plainSelectionMax = 32

// plainSelection reports whether sel can be emitted in order without
// field collection: only fields, no directives, no repeated response
// key. Most selections are plain; the others go through collect.
func plainSelection(sel *ast.SelectionSet) bool {
	if sel == nil || len(sel.Selections) > plainSelectionMax {
		return false
	}
	for i, s := range sel.Selections {
		f, ok := s.(*ast.Field)
		if !ok || len(f.Directives) > 0 {
			return false
		}
		k := selectionResponseKey(f)
		for _, prev := range sel.Selections[:i] {
			if selectionResponseKey(prev.(*ast.Field)) == k {
				return false
			}
		}
	}
	return true
}

func selectionResponseKey(f *ast.Field) string {
	if f.Alias != nil && f.Alias.Value != "" {
		return f.Alias.Value
	}
	return f.Name.Value
}

// collectedField is one response key after field collection: the
// field name and the sub-selection of every occurrence.
type collectedField struct {
	key  string
	name string
	sels []*ast.SelectionSet
}

// fieldCollector runs field collection against the request's named
// fragments and variable values (both from ResolveInfo). The zero
// value has no fragments and no variables.
type fieldCollector struct {
	frags map[string]ast.Definition
	vars  map[string]any

	// errp, when set, receives the first @skip / @include whose `if`
	// variable is null, as graphql-go reports it: `Argument "if" of
	// non-null type "Boolean!" must not be null.` at the variable,
	// with the op field's path. Set by newFieldCollector only when a
	// variable is null, so the common case carries no pointer.
	errp *error
	info *graphql.ResolveInfo
}

// newFieldCollector builds the collector for an op's sub-selection
// from its ResolveInfo. errp is the caller's error slot; it is used
// only when some variable is null, and the caller returns *errp (when
// set) instead of the projected value.
func newFieldCollector(info *graphql.ResolveInfo, errp *error) fieldCollector {
	fc := fieldCollector{frags: info.Fragments, vars: info.VariableValues}
	for _, v := range info.VariableValues {
		if v == nil {
			fc.errp, fc.info = errp, info
			break
		}
	}
	return fc
}

// fragmentGate decides whether a fragment's type condition applies to
// the object being collected. union is set when the object type t is a
// variant of that union; only then are type conditions checked.
// Outside a union, graphql-go's validation already proved every
// fragment applies, so the zero value applies every fragment. Type
// conditions carry the local prefixed name; the IR names do not.
type fragmentGate struct {
	prefix   string
	t, union *ir.Type
}

func (g fragmentGate) applies(cond *ast.Named) bool {
	if cond == nil || cond.Name == nil || g.union == nil {
		return true
	}
	if g.t == nil {
		return false
	}
	bare := cond.Name.Value
	if g.prefix != "" && len(bare) > len(g.prefix) && bare[:len(g.prefix)] == g.prefix {
		bare = bare[len(g.prefix):]
	}
	return bare == g.t.Name || bare == g.union.Name
}

// collect is the spec's CollectFields (§6.3.2): it follows inline
// fragments and named fragment spreads whose type condition applies
// (gate), drops selections that @skip / @include exclude, and merges
// repeated response keys in first-seen order. Results are appended to
// out, so a caller can pass a stack buffer.
func (c fieldCollector) collect(out []collectedField, sel *ast.SelectionSet, gate fragmentGate, visited []string) []collectedField {
	if sel == nil {
		return out
	}
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			if !c.included(n.Directives) {
				continue
			}
			key := selectionResponseKey(n)
			merged := false
			for i := range out {
				if out[i].key == key {
					if n.SelectionSet != nil {
						out[i].sels = append(out[i].sels, n.SelectionSet)
					}
					merged = true
					break
				}
			}
			if merged {
				continue
			}
			f := collectedField{key: key, name: n.Name.Value}
			if n.SelectionSet != nil {
				f.sels = []*ast.SelectionSet{n.SelectionSet}
			}
			out = append(out, f)
		case *ast.InlineFragment:
			if !c.included(n.Directives) || !gate.applies(n.TypeCondition) {
				continue
			}
			out = c.collect(out, n.SelectionSet, gate, visited)
		case *ast.FragmentSpread:
			if n.Name == nil || !c.included(n.Directives) {
				continue
			}
			name := n.Name.Value
			seen := false
			for _, v := range visited {
				if v == name {
					seen = true
					break
				}
			}
			if seen {
				continue
			}
			visited = append(visited, name)
			frag, ok := c.frags[name].(*ast.FragmentDefinition)
			if !ok || !gate.applies(frag.TypeCondition) {
				continue
			}
			out = c.collect(out, frag.SelectionSet, gate, visited)
		}
	}
	return out
}

// included evaluates @skip and @include (§3.13.2, §3.13.3). @skip
// wins when both are present.
func (c fieldCollector) included(dirs []*ast.Directive) bool {
	for _, d := range dirs {
		if d == nil || d.Name == nil {
			continue
		}
		switch d.Name.Value {
		case "skip":
			if c.directiveIf(d) {
				return false
			}
		case "include":
			if !c.directiveIf(d) {
				return false
			}
		}
	}
	return true
}

func (c fieldCollector) directiveIf(d *ast.Directive) bool {
	for _, a := range d.Arguments {
		if a == nil || a.Name == nil || a.Name.Value != "if" {
			continue
		}
		switch v := a.Value.(type) {
		case *ast.BooleanValue:
			return v.Value
		case *ast.Variable:
			if v.Name != nil {
				val, present := c.vars[v.Name.Value]
				if present && val == nil && c.errp != nil && *c.errp == nil {
					const msg = `Argument "if" of non-null type "Boolean!" must not be null.`
					*c.errp = gqlerrors.NewErrorWithPath(msg, []ast.Node{v}, msg, nil, nil, opFieldPath(c.info), nil)
				}
				b, _ := val.(bool)
				return b
			}
		}
	}
	return false
}

// opFieldPath is the response path of the op field info resolves: the
// response keys from the operation root down to info.FieldASTs[0],
// found by pointer in the operation's selections (fragments
// included). An op sits below namespace objects only, never in a list,
// so the path has no indexes. Nil when the field is not found. Used
// only to report an error, so the walk's cost does not matter.
func opFieldPath(info *graphql.ResolveInfo) []any {
	if info == nil || len(info.FieldASTs) == 0 {
		return nil
	}
	op, ok := info.Operation.(*ast.OperationDefinition)
	if !ok {
		return nil
	}
	target := info.FieldASTs[0]
	var find func(sel *ast.SelectionSet, path []any, depth int) []any
	find = func(sel *ast.SelectionSet, path []any, depth int) []any {
		if sel == nil || depth > 64 {
			return nil
		}
		for _, s := range sel.Selections {
			switch n := s.(type) {
			case *ast.Field:
				p := append(path[:len(path):len(path)], selectionResponseKey(n))
				if n == target {
					return p
				}
				if r := find(n.SelectionSet, p, depth+1); r != nil {
					return r
				}
			case *ast.InlineFragment:
				if r := find(n.SelectionSet, path, depth+1); r != nil {
					return r
				}
			case *ast.FragmentSpread:
				if n.Name == nil {
					continue
				}
				if f, ok := info.Fragments[n.Name.Value].(*ast.FragmentDefinition); ok {
					if r := find(f.SelectionSet, path, depth+1); r != nil {
						return r
					}
				}
			}
		}
		return nil
	}
	return find(op.SelectionSet, nil, 0)
}
