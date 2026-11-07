package gateway

import (
	"encoding/json"
	"errors"

	"github.com/IodeSystems/graphql-go/v2/language/ast"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// openAPIWalker projects an upstream JSON response onto the local
// GraphQL selection. It carries the dispatcher's IR Service + local
// type-name prefix so it can:
//
//  1. Emit __typename with the local prefixed type name on object
//     selections (matching what graphql-go's projector would do).
//  2. Discriminate Union types (TypeKind=TypeUnion) using the IR's
//     DiscriminatorProperty + DiscriminatorMapping (Case 3a) with a
//     required-fields heuristic fallback (Case 3b), mirroring
//     IRTypeBuilder.unionFor's ResolveType implementation.
//
// frags and vars come from the request's ResolveInfo: named fragment
// spreads resolve through frags, and @skip / @include read their `if`
// argument from vars when it is a variable.
//
// Walker fields are read-only after construction. Nil svc/prefix
// degrades the walker to the pre-Phase-3a behavior (untyped
// passthrough, __typename emits null).
type openAPIWalker struct {
	svc    *ir.Service
	prefix string // "<ns>_" or "<ns>_<vN>_" — matches renderer naming
	frags  map[string]ast.Definition
	vars   map[string]any
}

// appendValue walks JSON `raw` in lockstep with the selection,
// projecting only the fields the local selection asks for. The
// selection is `sel` merged with `more`: a response key selected more
// than once (`{ a { x } a { y } }`, or through fragments) has one
// selection set per occurrence, and the spec merges them. `more` is
// nil in the common case. `outRef` is the IR type ref for this
// position; nil means an unknown context (e.g. missing op metadata)
// and the walker degrades gracefully.
//
// The walker is one-shallow-Unmarshal per object level: outer JSON
// gets unmarshaled into map[string]json.RawMessage (one allocation,
// no recursion into nested objects), then each selected child's raw
// bytes are walked recursively.
//
// Fundamental limitation: OpenAPI specs that publish a looser JSON
// type than the local GraphQL schema declares (e.g. `"42"` for an
// Int) end up with the upstream's representation flowing through
// verbatim — the legacy Dispatch path coerced via graphql-go's
// Serialize; the append path doesn't. Most well-behaved specs match
// upstream shapes exactly, so this is rarely visible.
func (w openAPIWalker) appendValue(dst []byte, raw json.RawMessage, sel *ast.SelectionSet, more []*ast.SelectionSet, outRef *ir.TypeRef) ([]byte, error) {
	if len(raw) == 0 {
		return append(dst, "null"...), nil
	}
	i := 0
	for i < len(raw) && isJSONWhitespace(raw[i]) {
		i++
	}
	if i >= len(raw) {
		return append(dst, "null"...), nil
	}
	switch raw[i] {
	case '{':
		if sel == nil && len(more) == 0 {
			return append(dst, raw...), nil
		}
		return w.appendObject(dst, raw, sel, more, w.resolveType(outRef))
	case '[':
		if sel == nil && len(more) == 0 {
			return append(dst, raw...), nil
		}
		if outRef != nil && outRef.IsList() {
			// Inner list of a nested list: its elements take the
			// list's element ref.
			return w.appendList(dst, raw, sel, more, &outRef.List.Elem)
		}
		return w.appendList(dst, raw, sel, more, outRef)
	default:
		return append(dst, raw...), nil
	}
}

// appendObject projects a single JSON object. `t` may be nil when
// the IR can't be resolved — emission still works, __typename just
// falls back to "null" (legacy behavior). A union `t` is
// discriminated to its variant first.
//
// The object is scanned once for the selected members' values
// (scanJSONMembers) instead of being decoded into a map; only union
// discrimination still decodes one.
func (w openAPIWalker) appendObject(dst []byte, raw json.RawMessage, sel *ast.SelectionSet, more []*ast.SelectionSet, t *ir.Type) ([]byte, error) {
	var union *ir.Type
	if t != nil && t.TypeKind == ir.TypeUnion {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return dst, err
		}
		union, t = t, w.resolveUnionVariant(obj, t)
	}
	var namesBuf [plainSelectionMax]string
	var valsBuf [plainSelectionMax][]byte
	dst = append(dst, '{')
	if union == nil && len(more) == 0 && plainSelection(sel) {
		names, vals := namesBuf[:len(sel.Selections)], valsBuf[:len(sel.Selections)]
		for i, s := range sel.Selections {
			names[i] = s.(*ast.Field).Name.Value
		}
		if err := scanJSONMembers(raw, names, vals); err != nil {
			return dst, err
		}
		for i, s := range sel.Selections {
			f := s.(*ast.Field)
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = w.appendField(dst, vals[i], selectionResponseKey(f), f.Name.Value, f.SelectionSet, nil, t)
			if err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	var fields []collectedField
	fields = w.collect(fields, sel, t, union, nil)
	for _, s := range more {
		fields = w.collect(fields, s, t, union, nil)
	}
	names, vals := namesBuf[:0], valsBuf[:0]
	if len(fields) > plainSelectionMax {
		names, vals = make([]string, 0, len(fields)), make([][]byte, 0, len(fields))
	}
	for i := range fields {
		names = append(names, fields[i].name)
		vals = append(vals, nil)
	}
	if err := scanJSONMembers(raw, names, vals); err != nil {
		return dst, err
	}
	for i := range fields {
		c := &fields[i]
		if i > 0 {
			dst = append(dst, ',')
		}
		var sel *ast.SelectionSet
		var more []*ast.SelectionSet
		if len(c.sels) > 0 {
			sel, more = c.sels[0], c.sels[1:]
		}
		var err error
		dst, err = w.appendField(dst, vals[i], c.key, c.name, sel, more, t)
		if err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// plainSelectionMax bounds plainSelection and the stack buffers
// appendObject scans into.
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

// appendField emits one `"key":value` pair for the field `name` of
// object type `t`. rawChild is the member's upstream value, nil when
// the upstream object has no such member.
func (w openAPIWalker) appendField(dst []byte, rawChild []byte, key, name string, sel *ast.SelectionSet, more []*ast.SelectionSet, t *ir.Type) ([]byte, error) {
	dst = appendJSONString(dst, key)
	dst = append(dst, ':')
	if name == "__typename" {
		if t != nil && w.prefix != "" {
			return appendJSONString(dst, w.prefix+t.Name), nil
		}
		return append(dst, "null"...), nil
	}
	if len(rawChild) == 0 {
		return append(dst, "null"...), nil
	}
	var childRef *ir.TypeRef
	if t != nil {
		for _, f := range t.Fields {
			if f.Name == name {
				childRef = &f.Type
				break
			}
		}
	}
	return w.appendValue(dst, rawChild, sel, more, childRef)
}

// collectedField is one response key after field collection: the
// field name and the sub-selection of every occurrence.
type collectedField struct {
	key  string
	name string
	sels []*ast.SelectionSet
}

// collect is the spec's CollectFields (§6.3.2) for the object type
// `t`: it follows inline fragments and named fragment spreads whose
// type condition applies, drops selections that @skip / @include
// exclude, and merges repeated response keys in first-seen order.
// `union` is set when `t` is a variant of that union; fragments are
// then gated by type condition. Outside a union, graphql-go's
// validation already proved every fragment applies to `t`.
func (w openAPIWalker) collect(out []collectedField, sel *ast.SelectionSet, t, union *ir.Type, visited []string) []collectedField {
	if sel == nil {
		return out
	}
	for _, s := range sel.Selections {
		switch n := s.(type) {
		case *ast.Field:
			if !w.included(n.Directives) {
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
			c := collectedField{key: key, name: n.Name.Value}
			if n.SelectionSet != nil {
				c.sels = []*ast.SelectionSet{n.SelectionSet}
			}
			out = append(out, c)
		case *ast.InlineFragment:
			if !w.included(n.Directives) || !w.fragmentApplies(n.TypeCondition, t, union) {
				continue
			}
			out = w.collect(out, n.SelectionSet, t, union, visited)
		case *ast.FragmentSpread:
			if n.Name == nil || !w.included(n.Directives) {
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
			frag, ok := w.frags[name].(*ast.FragmentDefinition)
			if !ok || !w.fragmentApplies(frag.TypeCondition, t, union) {
				continue
			}
			out = w.collect(out, frag.SelectionSet, t, union, visited)
		}
	}
	return out
}

// fragmentApplies reports whether a fragment with type condition
// `cond` applies to object type `t`. Type conditions carry the local
// prefixed name; the IR names do not.
func (w openAPIWalker) fragmentApplies(cond *ast.Named, t, union *ir.Type) bool {
	if cond == nil || cond.Name == nil || union == nil {
		return true
	}
	if t == nil {
		return false
	}
	bare := cond.Name.Value
	if w.prefix != "" && len(bare) > len(w.prefix) && bare[:len(w.prefix)] == w.prefix {
		bare = bare[len(w.prefix):]
	}
	return bare == t.Name || bare == union.Name
}

// included evaluates @skip and @include (§3.13.2, §3.13.3). @skip
// wins when both are present.
func (w openAPIWalker) included(dirs []*ast.Directive) bool {
	for _, d := range dirs {
		if d == nil || d.Name == nil {
			continue
		}
		switch d.Name.Value {
		case "skip":
			if w.directiveIf(d) {
				return false
			}
		case "include":
			if !w.directiveIf(d) {
				return false
			}
		}
	}
	return true
}

func (w openAPIWalker) directiveIf(d *ast.Directive) bool {
	for _, a := range d.Arguments {
		if a == nil || a.Name == nil || a.Name.Value != "if" {
			continue
		}
		switch v := a.Value.(type) {
		case *ast.BooleanValue:
			return v.Value
		case *ast.Variable:
			if v.Name != nil {
				b, _ := w.vars[v.Name.Value].(bool)
				return b
			}
		}
	}
	return false
}

// resolveUnionVariant runs the discriminator+heuristic chain on the
// shallow-unmarshaled object, returning the matched variant *ir.Type
// or nil if nothing matches.
func (w openAPIWalker) resolveUnionVariant(obj map[string]json.RawMessage, u *ir.Type) *ir.Type {
	if w.svc == nil {
		return nil
	}
	// 1. Discriminator-driven (Case 3a).
	if u.DiscriminatorProperty != "" {
		if rawDisc, ok := obj[u.DiscriminatorProperty]; ok {
			var discVal string
			if json.Unmarshal(rawDisc, &discVal) == nil {
				if mapped, ok := u.DiscriminatorMapping[discVal]; ok {
					if t, ok := w.svc.Types[mapped]; ok && t.TypeKind == ir.TypeObject {
						if w.variantMember(u, t.Name) {
							return t
						}
					}
				}
				// Identity fallback (Case 3a fallback) — discriminator
				// value equals variant Type name.
				if t, ok := w.svc.Types[discVal]; ok && t.TypeKind == ir.TypeObject {
					if w.variantMember(u, t.Name) {
						return t
					}
				}
			}
		}
	}
	// 2. Wire __typename (rare on openapi but cheap to honor).
	if rawTN, ok := obj["__typename"]; ok {
		var tn string
		if json.Unmarshal(rawTN, &tn) == nil {
			if t, ok := w.svc.Types[tn]; ok && t.TypeKind == ir.TypeObject {
				if w.variantMember(u, t.Name) {
					return t
				}
			}
		}
	}
	// 3. Required-fields heuristic (Case 3b).
	for _, variantName := range u.Variants {
		v, ok := w.svc.Types[variantName]
		if !ok || v.TypeKind != ir.TypeObject {
			continue
		}
		match := true
		for _, f := range v.Fields {
			if !f.Required {
				continue
			}
			if _, present := obj[f.Name]; !present {
				match = false
				break
			}
		}
		if match {
			return v
		}
	}
	return nil
}

func (w openAPIWalker) variantMember(u *ir.Type, name string) bool {
	for _, v := range u.Variants {
		if v == name {
			return true
		}
	}
	return false
}

// resolveType walks an IR TypeRef to its named Type entry. Returns
// nil for built-in scalars, maps, or when svc isn't set.
func (w openAPIWalker) resolveType(ref *ir.TypeRef) *ir.Type {
	if ref == nil || w.svc == nil || ref.Named == "" {
		return nil
	}
	if t, ok := w.svc.Types[ref.Named]; ok {
		return t
	}
	return nil
}

// appendList parses `raw` as a JSON array and walks each element
// with the same selection and element-type ref.
func (w openAPIWalker) appendList(dst []byte, raw json.RawMessage, sel *ast.SelectionSet, more []*ast.SelectionSet, outRef *ir.TypeRef) ([]byte, error) {
	i := skipJSONWhitespace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return dst, errMalformedJSON
	}
	dst = append(dst, '[')
	i = skipJSONWhitespace(raw, i+1)
	if i < len(raw) && raw[i] == ']' {
		return append(dst, ']'), nil
	}
	for n := 0; ; n++ {
		end, err := skipJSONValue(raw, i)
		if err != nil {
			return dst, err
		}
		if n > 0 {
			dst = append(dst, ',')
		}
		dst, err = w.appendValue(dst, raw[i:end], sel, more, outRef)
		if err != nil {
			return dst, err
		}
		i = skipJSONWhitespace(raw, end)
		if i >= len(raw) {
			return dst, errMalformedJSON
		}
		switch raw[i] {
		case ',':
			i = skipJSONWhitespace(raw, i+1)
		case ']':
			return append(dst, ']'), nil
		default:
			return dst, errMalformedJSON
		}
	}
}

var errMalformedJSON = errors.New("openapi: malformed JSON in upstream response")

// scanJSONMembers walks the JSON object raw once and sets vals[i] to
// the raw value of the member named names[i] (nil when absent; the
// last one wins when a key repeats, as encoding/json does). vals is
// subslices of raw, so nothing is allocated unless a key carries an
// escape sequence. raw must be valid JSON (DispatchAppend checks the
// whole body with json.Valid first); the scanner still bounds-checks.
func scanJSONMembers(raw []byte, names []string, vals [][]byte) error {
	i := skipJSONWhitespace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return errMalformedJSON
	}
	i = skipJSONWhitespace(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return nil
	}
	for {
		if i >= len(raw) || raw[i] != '"' {
			return errMalformedJSON
		}
		keyEnd, escaped, err := skipJSONString(raw, i)
		if err != nil {
			return err
		}
		key := raw[i+1 : keyEnd-1]
		var unescaped string
		if escaped {
			if unescaped, err = unquoteJSONString(raw[i:keyEnd]); err != nil {
				return err
			}
		}
		i = skipJSONWhitespace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return errMalformedJSON
		}
		i = skipJSONWhitespace(raw, i+1)
		end, err := skipJSONValue(raw, i)
		if err != nil {
			return err
		}
		for j, n := range names {
			if (escaped && n == unescaped) || (!escaped && n == string(key)) {
				vals[j] = raw[i:end]
			}
		}
		i = skipJSONWhitespace(raw, end)
		if i >= len(raw) {
			return errMalformedJSON
		}
		switch raw[i] {
		case ',':
			i = skipJSONWhitespace(raw, i+1)
		case '}':
			return nil
		default:
			return errMalformedJSON
		}
	}
}

// unquoteJSONString decodes a quoted JSON string. A separate function
// so its decode target is heap-allocated only when a key is escaped.
func unquoteJSONString(quoted []byte) (string, error) {
	var s string
	err := json.Unmarshal(quoted, &s)
	return s, err
}

func skipJSONWhitespace(raw []byte, i int) int {
	for i < len(raw) && isJSONWhitespace(raw[i]) {
		i++
	}
	return i
}

// skipJSONString returns the offset one past the closing quote of
// the string starting at raw[i], and whether it holds an escape.
func skipJSONString(raw []byte, i int) (end int, escaped bool, err error) {
	for j := i + 1; j < len(raw); j++ {
		switch raw[j] {
		case '\\':
			escaped = true
			j++
		case '"':
			return j + 1, escaped, nil
		}
	}
	return 0, false, errMalformedJSON
}

// skipJSONValue returns the offset one past the value starting at
// raw[i].
func skipJSONValue(raw []byte, i int) (int, error) {
	if i >= len(raw) {
		return 0, errMalformedJSON
	}
	switch raw[i] {
	case '"':
		end, _, err := skipJSONString(raw, i)
		return end, err
	case '{', '[':
		depth := 0
		for j := i; j < len(raw); j++ {
			switch raw[j] {
			case '"':
				end, _, err := skipJSONString(raw, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errMalformedJSON
	default:
		// Number, true, false or null.
		j := i
		for j < len(raw) && raw[j] != ',' && raw[j] != '}' && raw[j] != ']' && !isJSONWhitespace(raw[j]) {
			j++
		}
		if j == i {
			return 0, errMalformedJSON
		}
		return j, nil
	}
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
