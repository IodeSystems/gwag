package gat

// Dependent operations: batched requests with @export.
//
// A fused query runs independent operations together. "Fetch the
// orders, then the customers they name" depends on the first result,
// and GraphQL has no way for one field to read another's. The answer
// is a batch of operations run in order, where a field marked
// @export(as: "name") hands the values it resolved to the next
// operations as the variable $name:
//
//	[{"query": "{ Shop { listOrders(limit: 50) { id customerId @export(as: \"ids\") } } }"},
//	 {"query": "query($ids: [String!]!) { Shop { getCustomersById(ids: $ids) { id name } } }"}]
//
// Each operation is an ordinary one — same middlewares, same context,
// same limits — and the second gets ONE list, so there is no N+1 and
// nothing for gat to resolve on the server's behalf. This is why gat
// has no links or nested resolvers.

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/IodeSystems/graphql-go/v2/language/source"
)

// exportDirective is declared on the schema so documents using it
// validate (and graphql-codegen accepts them). The executor ignores it;
// the batch handler reads the marked fields out of each result.
var exportDirective = graphql.NewDirective(graphql.DirectiveConfig{
	Name: "export",
	Description: "In a batched request, hand the values this field resolved to the " +
		"operations after this one as the variable $as. Values are deduplicated; " +
		"a field under a list exports a list.",
	Locations: []string{graphql.DirectiveLocationField},
	Args: graphql.FieldConfigArgument{
		"as": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	},
})

// exportPath is one @export in an operation: the variable it feeds and
// the response keys leading to the field from the data root.
type exportPath struct {
	as   string
	keys []string
}

// exportCache keeps each document's export paths so a repeated batch
// does not re-parse its documents. Bounded: past the cap new documents
// are parsed but not kept.
type exportCache struct {
	mu     sync.RWMutex
	paths  map[exportKey][]exportPath
	stages map[exportKey]*stagedDoc // nil value: needs no staging
}

type exportKey struct{ query, operationName string }

const exportCacheEntries = 1024

// exportsFor returns the @export paths of the operation, nil when the
// document has none (the common case, answered without parsing).
func (c *exportCache) exportsFor(query, operationName string) []exportPath {
	if !strings.Contains(query, "@export") {
		return nil
	}
	key := exportKey{query, operationName}
	c.mu.RLock()
	paths, ok := c.paths[key]
	c.mu.RUnlock()
	if ok {
		return paths
	}
	paths = parseExports(query, operationName)
	c.mu.Lock()
	if c.paths == nil {
		c.paths = make(map[exportKey][]exportPath)
	}
	if len(c.paths) < exportCacheEntries {
		c.paths[key] = paths
	}
	c.mu.Unlock()
	return paths
}

// stagesFor returns the document's dependency stages (staging.go), nil
// when it needs none — without parsing unless the text contains
// "@export".
func (c *exportCache) stagesFor(schema *graphql.Schema, query, operationName string) *stagedDoc {
	if !strings.Contains(query, "@export") {
		return nil
	}
	key := exportKey{query, operationName}
	c.mu.RLock()
	st, ok := c.stages[key]
	c.mu.RUnlock()
	if ok {
		return st
	}
	st = planStages(schema, query, operationName)
	c.mu.Lock()
	if c.stages == nil {
		c.stages = make(map[exportKey]*stagedDoc)
	}
	if len(c.stages) < exportCacheEntries {
		c.stages[key] = st
	}
	c.mu.Unlock()
	return st
}

// parseExports finds the @export fields of the selected operation. A
// document that fails to parse has none: the operation itself reports
// the parse error.
func parseExports(query, operationName string) []exportPath {
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
	if err != nil {
		return nil
	}
	fragments := map[string]*ast.FragmentDefinition{}
	var op *ast.OperationDefinition
	for _, def := range doc.Definitions {
		switch d := def.(type) {
		case *ast.FragmentDefinition:
			fragments[d.Name.Value] = d
		case *ast.OperationDefinition:
			if operationName == "" || (d.Name != nil && d.Name.Value == operationName) {
				if op == nil {
					op = d
				}
			}
		}
	}
	if op == nil {
		return nil
	}
	var out []exportPath
	var walk func(set *ast.SelectionSet, keys []string, inPath map[string]bool)
	walk = func(set *ast.SelectionSet, keys []string, inPath map[string]bool) {
		if set == nil {
			return
		}
		for _, sel := range set.Selections {
			switch s := sel.(type) {
			case *ast.Field:
				key := s.Name.Value
				if s.Alias != nil {
					key = s.Alias.Value
				}
				path := append(append([]string(nil), keys...), key)
				for _, d := range s.Directives {
					if d.Name.Value != "export" {
						continue
					}
					for _, a := range d.Arguments {
						if a.Name.Value == "as" {
							if v, ok := a.Value.(*ast.StringValue); ok {
								out = append(out, exportPath{as: v.Value, keys: path})
							}
						}
					}
				}
				walk(s.SelectionSet, path, inPath)
			case *ast.InlineFragment:
				walk(s.SelectionSet, keys, inPath)
			case *ast.FragmentSpread:
				name := s.Name.Value
				if f := fragments[name]; f != nil && !inPath[name] {
					inPath[name] = true
					walk(f.SelectionSet, keys, inPath)
					delete(inPath, name)
				}
			}
		}
	}
	walk(op.SelectionSet, nil, map[string]bool{})
	return out
}

// collectExports reads the exported values out of one operation's
// response and adds them to vars. A path that passes through a list,
// or a name exported more than once, yields a list; a single value
// stays a scalar. Repeated scalars are dropped, so ids sent on to a
// batch lookup go once each.
func collectExports(paths []exportPath, payload []byte, vars map[string]any) {
	var resp struct {
		Data any `json:"data"`
	}
	if json.Unmarshal(payload, &resp) != nil {
		return
	}
	type acc struct {
		vals []any
		list bool
		seen map[any]bool
	}
	byName := map[string]*acc{}
	var order []string
	for _, p := range paths {
		a := byName[p.as]
		if a == nil {
			a = &acc{seen: map[any]bool{}}
			byName[p.as] = a
			order = append(order, p.as)
		} else {
			a.list = true
		}
		var visit func(v any, keys []string)
		visit = func(v any, keys []string) {
			if arr, ok := v.([]any); ok {
				a.list = true
				for _, e := range arr {
					visit(e, keys)
				}
				return
			}
			if len(keys) == 0 {
				if v == nil {
					return
				}
				switch v.(type) {
				case string, float64, bool:
					if a.seen[v] {
						return
					}
					a.seen[v] = true
				}
				a.vals = append(a.vals, v)
				return
			}
			if m, ok := v.(map[string]any); ok {
				visit(m[keys[0]], keys[1:])
			}
		}
		visit(resp.Data, p.keys)
	}
	for _, name := range order {
		a := byName[name]
		switch {
		case a.list:
			if a.vals == nil {
				a.vals = []any{}
			}
			vars[name] = a.vals
		case len(a.vals) == 1:
			vars[name] = a.vals[0]
		}
	}
}
