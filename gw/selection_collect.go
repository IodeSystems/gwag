package gateway

import (
	"github.com/IodeSystems/graphql-go/v2/language/ast"

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
				b, _ := c.vars[v.Name.Value].(bool)
				return b
			}
		}
	}
	return false
}
