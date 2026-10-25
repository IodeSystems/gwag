package gat

// One document, ordered by its @export dependencies.
//
//	query ($ids: [String!]) {
//	  Shop {
//	    listOrders(limit: 50) { id customerId @export(as: "ids") }
//	    getCustomersById(ids: $ids) { id name }
//	  }
//	}
//
// getCustomersById reads $ids, which listOrders exports, so it has to
// run after listOrders. gat splits such a document into stages at plan
// time: each stage is a valid document of its own — the namespace
// containers, that stage's operations, the variables it uses — run
// through the ordinary plan cache and executor. Stages run in order,
// each one's exports filling the variables of the next, and their data
// merges into one ordinary GraphQL response. One request, one result
// type for codegen.
//
// The unit that moves between stages is an operation field. An export
// can feed operations; it cannot feed a field nested inside another
// operation's result — that would be a server-side join (links), which
// gat does not do.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/IodeSystems/graphql-go/v2/language/printer"
	"github.com/IodeSystems/graphql-go/v2/language/source"
)

// docStage is one stage of a staged document.
type docStage struct {
	query   string
	exports []exportPath
}

// stagedDoc is a document's stages in run order; err is set when the
// document's dependencies cannot be staged (reported to the client).
type stagedDoc struct {
	stages []docStage
	order  *keyOrder // the document's field order, for the merged response
	err    error
}

// keyOrder is the response-key order of one selection level, and of
// the levels under each key. Merging stages goes through maps, so the
// merged response is written back out in this order: fields come back
// in the order the query asked for them.
type keyOrder struct {
	keys     []string
	children map[string]*keyOrder
}

func newKeyOrder(set *ast.SelectionSet) *keyOrder {
	o := &keyOrder{children: map[string]*keyOrder{}}
	o.add(set)
	return o
}

func (o *keyOrder) add(set *ast.SelectionSet) {
	if set == nil {
		return
	}
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			k := responseKey(s)
			child, seen := o.children[k]
			if !seen {
				o.keys = append(o.keys, k)
				child = &keyOrder{children: map[string]*keyOrder{}}
				o.children[k] = child
			}
			child.add(s.SelectionSet)
		case *ast.InlineFragment:
			o.add(s.SelectionSet)
		}
	}
}

// stageUnit is one operation field and what it exchanges through
// variables.
type stageUnit struct {
	field   *ast.Field
	reads   map[string]bool
	exports []exportPath
	stage   int
}

// planStages splits the selected operation of query into dependency
// stages. It returns nil when the document needs no staging (no
// @export feeds another field of the same document).
func planStages(schema *graphql.Schema, query, operationName string) *stagedDoc {
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
	if err != nil {
		return nil // the plan reports the parse error
	}
	fragments := map[string]*ast.FragmentDefinition{}
	var op *ast.OperationDefinition
	for _, def := range doc.Definitions {
		switch d := def.(type) {
		case *ast.FragmentDefinition:
			fragments[d.Name.Value] = d
		case *ast.OperationDefinition:
			if op == nil && (operationName == "" || (d.Name != nil && d.Name.Value == operationName)) {
				op = d
			}
		}
	}
	if op == nil {
		return nil
	}
	root := schema.QueryType()
	switch op.Operation {
	case ast.OperationTypeMutation:
		root = schema.MutationType()
	case ast.OperationTypeSubscription:
		return nil
	}
	set := inlineFragments(op.SelectionSet, fragments, map[string]bool{})

	var units []*stageUnit
	var collect func(set *ast.SelectionSet, parent *graphql.Object, keys []string) error
	collect = func(set *ast.SelectionSet, parent *graphql.Object, keys []string) error {
		for _, sel := range set.Selections {
			switch s := sel.(type) {
			case *ast.InlineFragment:
				p := parent
				if s.TypeCondition != nil {
					if o, ok := schema.Type(s.TypeCondition.Name.Value).(*graphql.Object); ok {
						p = o
					}
				}
				if err := collect(s.SelectionSet, p, keys); err != nil {
					return err
				}
			case *ast.Field:
				path := append(append([]string(nil), keys...), responseKey(s))
				if child := containerType(parent, s.Name.Value); child != nil {
					if err := collect(s.SelectionSet, child, path); err != nil {
						return err
					}
					continue
				}
				u := &stageUnit{field: s, reads: map[string]bool{}}
				collectReads(s, u.reads)
				u.exports = fieldExports(s, keys)
				units = append(units, u)
			}
		}
		return nil
	}
	if err := collect(set, root, nil); err != nil {
		return &stagedDoc{err: err}
	}

	exporter := map[string][]*stageUnit{}
	for _, u := range units {
		for _, e := range u.exports {
			exporter[e.as] = append(exporter[e.as], u)
		}
	}
	// A field reading a variable that another field exports runs in a
	// later stage than every exporter of it. Iterate to a fixpoint; a
	// stage number past the unit count means a cycle.
	for _, u := range units {
		u.stage = 1
	}
	for changed, rounds := true, 0; changed; rounds++ {
		if rounds > len(units) {
			return &stagedDoc{err: fmt.Errorf("gat: @export dependencies form a cycle")}
		}
		changed = false
		for _, u := range units {
			for v := range u.reads {
				for _, ex := range exporter[v] {
					if ex == u {
						return &stagedDoc{err: fmt.Errorf("gat: %s reads $%s, which it exports itself", responseKey(u.field), v)}
					}
					if ex.stage+1 > u.stage {
						u.stage = ex.stage + 1
						changed = true
					}
				}
			}
		}
	}
	last := 1
	for _, u := range units {
		last = max(last, u.stage)
	}
	if last == 1 {
		return nil
	}

	staged := &stagedDoc{order: newKeyOrder(set)}
	for k := 1; k <= last; k++ {
		inStage := map[*ast.Field]bool{}
		var exports []exportPath
		for _, u := range units {
			if u.stage == k {
				inStage[u.field] = true
				exports = append(exports, u.exports...)
			}
		}
		pruned := pruneToStage(set, inStage)
		used := map[string]bool{}
		collectSetReads(pruned, used)
		var defs []*ast.VariableDefinition
		for _, d := range op.VariableDefinitions {
			if used[d.Variable.Name.Value] {
				defs = append(defs, d)
			}
		}
		stageOp := ast.NewOperationDefinition(&ast.OperationDefinition{
			Operation:           op.Operation,
			Name:                op.Name,
			VariableDefinitions: defs,
			Directives:          op.Directives,
			SelectionSet:        pruned,
		})
		text, _ := printer.Print(ast.NewDocument(&ast.Document{Definitions: []ast.Node{stageOp}})).(string)
		staged.stages = append(staged.stages, docStage{query: text, exports: exports})
	}
	return staged
}

func responseKey(f *ast.Field) string {
	if f.Alias != nil {
		return f.Alias.Value
	}
	return f.Name.Value
}

// containerType returns the object type of parent's field name when it
// is a namespace container (gw/ir names them "...Namespace") — a field
// that groups operations without running anything itself. Nil for an
// operation field.
func containerType(parent *graphql.Object, name string) *graphql.Object {
	if parent == nil {
		return nil
	}
	def, ok := parent.Fields()[name]
	if !ok {
		return nil
	}
	t := def.Type
	for {
		switch w := t.(type) {
		case *graphql.NonNull:
			t = w.OfType
			continue
		case *graphql.List:
			return nil
		}
		break
	}
	if o, ok := t.(*graphql.Object); ok && strings.HasSuffix(o.Name(), "Namespace") {
		return o
	}
	return nil
}

// inlineFragments returns set with every fragment spread replaced by
// an inline fragment of the same type condition, so a stage document
// carries no fragment definitions it might not use.
func inlineFragments(set *ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, inPath map[string]bool) *ast.SelectionSet {
	if set == nil {
		return nil
	}
	out := ast.NewSelectionSet(&ast.SelectionSet{})
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			f := *s
			f.SelectionSet = inlineFragments(s.SelectionSet, fragments, inPath)
			out.Selections = append(out.Selections, &f)
		case *ast.InlineFragment:
			in := *s
			in.SelectionSet = inlineFragments(s.SelectionSet, fragments, inPath)
			out.Selections = append(out.Selections, &in)
		case *ast.FragmentSpread:
			name := s.Name.Value
			frag := fragments[name]
			if frag == nil || inPath[name] {
				continue // unknown or cyclic: validation reports it
			}
			inPath[name] = true
			out.Selections = append(out.Selections, ast.NewInlineFragment(&ast.InlineFragment{
				TypeCondition: frag.TypeCondition,
				Directives:    s.Directives,
				SelectionSet:  inlineFragments(frag.SelectionSet, fragments, inPath),
			}))
			delete(inPath, name)
		}
	}
	return out
}

// pruneToStage keeps the fields in inStage and the containers above
// them; nil when nothing in set belongs to the stage.
func pruneToStage(set *ast.SelectionSet, inStage map[*ast.Field]bool) *ast.SelectionSet {
	if set == nil {
		return nil
	}
	out := ast.NewSelectionSet(&ast.SelectionSet{})
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			if inStage[s] {
				out.Selections = append(out.Selections, s)
				continue
			}
			if sub := pruneToStage(s.SelectionSet, inStage); sub != nil {
				f := *s
				f.SelectionSet = sub
				out.Selections = append(out.Selections, &f)
			}
		case *ast.InlineFragment:
			if sub := pruneToStage(s.SelectionSet, inStage); sub != nil {
				in := *s
				in.SelectionSet = sub
				out.Selections = append(out.Selections, &in)
			}
		}
	}
	if len(out.Selections) == 0 {
		return nil
	}
	return out
}

// collectReads records every variable f's arguments and directives,
// and those of its subtree, reference.
func collectReads(f *ast.Field, into map[string]bool) {
	for _, a := range f.Arguments {
		valueReads(a.Value, into)
	}
	for _, d := range f.Directives {
		for _, a := range d.Arguments {
			valueReads(a.Value, into)
		}
	}
	collectSetReads(f.SelectionSet, into)
}

func collectSetReads(set *ast.SelectionSet, into map[string]bool) {
	if set == nil {
		return
	}
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			collectReads(s, into)
		case *ast.InlineFragment:
			for _, d := range s.Directives {
				for _, a := range d.Arguments {
					valueReads(a.Value, into)
				}
			}
			collectSetReads(s.SelectionSet, into)
		}
	}
}

func valueReads(v ast.Value, into map[string]bool) {
	switch x := v.(type) {
	case *ast.Variable:
		into[x.Name.Value] = true
	case *ast.ListValue:
		for _, e := range x.Values {
			valueReads(e, into)
		}
	case *ast.ObjectValue:
		for _, f := range x.Fields {
			valueReads(f.Value, into)
		}
	}
}

// fieldExports returns the @export paths in f's subtree (f included),
// as response-key paths from the data root.
func fieldExports(f *ast.Field, parentKeys []string) []exportPath {
	var out []exportPath
	var walk func(f *ast.Field, keys []string)
	walk = func(f *ast.Field, keys []string) {
		path := append(append([]string(nil), keys...), responseKey(f))
		for _, d := range f.Directives {
			if d.Name.Value != "export" {
				continue
			}
			for _, a := range d.Arguments {
				if s, ok := a.Value.(*ast.StringValue); ok && a.Name.Value == "as" {
					out = append(out, exportPath{as: s.Value, keys: path})
				}
			}
		}
		var sub func(set *ast.SelectionSet)
		sub = func(set *ast.SelectionSet) {
			if set == nil {
				return
			}
			for _, sel := range set.Selections {
				switch s := sel.(type) {
				case *ast.Field:
					walk(s, path)
				case *ast.InlineFragment:
					sub(s.SelectionSet)
				}
			}
		}
		sub(f.SelectionSet)
	}
	walk(f, parentKeys)
	return out
}

// mergeStageResults merges the stages' responses into one: data trees
// merged key by key and written in the document's field order, errors
// concatenated in stage order.
func mergeStageResults(payloads [][]byte, order *keyOrder) []byte {
	var (
		data   map[string]any
		errs   []any
		failed bool
	)
	for _, p := range payloads {
		var r struct {
			Data   map[string]any `json:"data"`
			Errors []any          `json:"errors"`
		}
		if json.Unmarshal(p, &r) != nil {
			continue
		}
		errs = append(errs, r.Errors...)
		if r.Data == nil {
			failed = failed || len(r.Errors) > 0
			continue
		}
		if data == nil {
			data = map[string]any{}
		}
		mergeData(data, r.Data)
	}
	b := []byte{'{'}
	if data != nil || !failed {
		b = append(b, `"data":`...)
		b = appendOrdered(b, data, order)
	}
	if len(errs) > 0 {
		if len(b) > 1 {
			b = append(b, ',')
		}
		e, _ := json.Marshal(errs)
		b = append(append(b, `"errors":`...), e...)
	}
	return append(b, '}')
}

// appendOrdered writes v as JSON, objects' keys in o's order (keys o
// does not know follow, sorted).
func appendOrdered(dst []byte, v any, o *keyOrder) []byte {
	switch x := v.(type) {
	case map[string]any:
		dst = append(dst, '{')
		first := true
		write := func(k string) {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			kb, _ := json.Marshal(k)
			dst = append(append(dst, kb...), ':')
			var child *keyOrder
			if o != nil {
				child = o.children[k]
			}
			dst = appendOrdered(dst, x[k], child)
		}
		done := map[string]bool{}
		if o != nil {
			for _, k := range o.keys {
				if _, ok := x[k]; ok {
					write(k)
					done[k] = true
				}
			}
		}
		rest := make([]string, 0, len(x))
		for k := range x {
			if !done[k] {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			write(k)
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendOrdered(dst, e, o)
		}
		return append(dst, ']')
	default:
		b, _ := json.Marshal(x)
		return append(dst, b...)
	}
}

func mergeData(dst, src map[string]any) {
	for k, v := range src {
		if dm, ok := dst[k].(map[string]any); ok {
			if sm, ok := v.(map[string]any); ok {
				mergeData(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
