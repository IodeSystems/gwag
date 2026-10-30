// Package gqlguard holds the request limits gwag and gat share:
// validation rules (depth, operation count, introspection) and
// trusted documents. The rules run in the plan cache, so each is
// checked once per distinct query and its verdict is cached with the
// plan; nothing here runs per request on a cache hit.
package gqlguard

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/gqlerrors"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/kinds"
	"github.com/IodeSystems/graphql-go/v2/language/visitor"
)

// MaxDepthRule refuses queries whose fields nest deeper than limit.
// Every field level counts, the namespace field included:
// `{ Demo { getProject { owner { name } } } }` is 4 deep.
func MaxDepthRule(limit int) graphql.ValidationRuleFn {
	return func(ctx *graphql.ValidationContext) *graphql.ValidationRuleInstance {
		return onOperation(func(op *ast.OperationDefinition) {
			if d := selectionDepth(ctx, op.SelectionSet, map[string]bool{}); d > limit {
				ctx.ReportError(gqlerrors.NewError(
					fmt.Sprintf("query depth %d exceeds the limit of %d", d, limit),
					[]ast.Node{op}, "", nil, []int{}, nil))
			}
		})
	}
}

// MaxOperationsRule refuses queries that select more than limit
// operations, aliases and fragment spreads included: a document that
// aliases one operation a thousand times is refused before anything
// runs. An operation is a field outside the namespace containers
// (object types gw/ir names "...Namespace") whose parent is a root
// type or a container. @skip / @include are not evaluated, so the
// count is an upper bound.
func MaxOperationsRule(limit int) graphql.ValidationRuleFn {
	return func(ctx *graphql.ValidationContext) *graphql.ValidationRuleInstance {
		return onOperation(func(op *ast.OperationDefinition) {
			var root *graphql.Object
			schema := ctx.Schema()
			switch op.Operation {
			case ast.OperationTypeMutation:
				root = schema.MutationType()
			case ast.OperationTypeSubscription:
				root = schema.SubscriptionType()
			default:
				root = schema.QueryType()
			}
			if root == nil {
				return
			}
			if n := countOperations(ctx, root, op.SelectionSet, map[string]bool{}); n > limit {
				ctx.ReportError(gqlerrors.NewError(
					fmt.Sprintf("request selects %d operations, over the limit of %d", n, limit),
					[]ast.Node{op}, "", nil, []int{}, nil))
			}
		})
	}
}

// NoIntrospectionRule refuses __schema and __type; __typename stays
// allowed.
func NoIntrospectionRule(ctx *graphql.ValidationContext) *graphql.ValidationRuleInstance {
	return &graphql.ValidationRuleInstance{VisitorOpts: &visitor.VisitorOptions{
		KindFuncMap: map[string]visitor.NamedVisitFuncs{
			kinds.Field: {Kind: func(p visitor.VisitFuncParams) (string, interface{}) {
				if f, ok := p.Node.(*ast.Field); ok && f.Name != nil {
					if name := f.Name.Value; name == "__schema" || name == "__type" {
						ctx.ReportError(gqlerrors.NewError(
							"introspection is disabled", []ast.Node{f}, "", nil, []int{}, nil))
					}
				}
				return visitor.ActionNoChange, nil
			}},
		},
	}}
}

func onOperation(check func(*ast.OperationDefinition)) *graphql.ValidationRuleInstance {
	return &graphql.ValidationRuleInstance{VisitorOpts: &visitor.VisitorOptions{
		KindFuncMap: map[string]visitor.NamedVisitFuncs{
			kinds.OperationDefinition: {Kind: func(p visitor.VisitFuncParams) (string, interface{}) {
				if op, ok := p.Node.(*ast.OperationDefinition); ok {
					check(op)
				}
				return visitor.ActionSkip, nil
			}},
		},
	}}
}

// selectionDepth is the deepest field nesting under set, following
// fragment spreads. inPath holds the fragments on the current path so a
// cycle (reported by the spec's NoFragmentCycles rule) cannot recurse
// forever.
func selectionDepth(ctx *graphql.ValidationContext, set *ast.SelectionSet, inPath map[string]bool) int {
	if set == nil {
		return 0
	}
	deepest := 0
	for _, sel := range set.Selections {
		var d int
		switch s := sel.(type) {
		case *ast.Field:
			d = 1 + selectionDepth(ctx, s.SelectionSet, inPath)
		case *ast.InlineFragment:
			d = selectionDepth(ctx, s.SelectionSet, inPath)
		case *ast.FragmentSpread:
			name := s.Name.Value
			frag := ctx.Fragment(name)
			if frag == nil || inPath[name] {
				continue
			}
			inPath[name] = true
			d = selectionDepth(ctx, frag.SelectionSet, inPath)
			delete(inPath, name)
		}
		deepest = max(deepest, d)
	}
	return deepest
}

// countOperations counts the operation fields selected under parent
// (a root type or a namespace container), descending only into
// containers. Fragments count once per spread.
func countOperations(ctx *graphql.ValidationContext, parent *graphql.Object, set *ast.SelectionSet, inPath map[string]bool) int {
	if set == nil {
		return 0
	}
	n := 0
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			if strings.HasPrefix(s.Name.Value, "__") {
				continue
			}
			def := parent.Fields()[s.Name.Value]
			if def == nil {
				continue // unknown field: the spec rules report it
			}
			if c := container(def.Type); c != nil {
				n += countOperations(ctx, c, s.SelectionSet, inPath)
			} else {
				n++
			}
		case *ast.InlineFragment:
			n += countOperations(ctx, parent, s.SelectionSet, inPath)
		case *ast.FragmentSpread:
			name := s.Name.Value
			frag := ctx.Fragment(name)
			if frag == nil || inPath[name] {
				continue
			}
			inPath[name] = true
			n += countOperations(ctx, parent, frag.SelectionSet, inPath)
			delete(inPath, name)
		}
	}
	return n
}

// container returns t as a namespace container object, or nil.
func container(t graphql.Output) *graphql.Object {
	if nn, ok := t.(*graphql.NonNull); ok {
		t = nn.OfType
	}
	if o, ok := t.(*graphql.Object); ok && strings.HasSuffix(o.Name(), "Namespace") {
		return o
	}
	return nil
}

// ParseManifest reads a persisted-documents manifest: a JSON object of
// document id → document text, as graphql-codegen's client preset
// writes it.
func ParseManifest(data []byte) (map[string]string, error) {
	var docs map[string]string
	if err := json.Unmarshal(data, &docs); err != nil {
		return nil, fmt.Errorf("trusted documents manifest: %w", err)
	}
	for id, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			return nil, fmt.Errorf("trusted documents manifest: document %q is empty", id)
		}
	}
	return docs, nil
}

// PersistedID is the document id a request names, from documentId or
// the Apollo APQ extension; "" when it names none.
func PersistedID(documentID string, extensions map[string]any) string {
	if documentID != "" {
		return documentID
	}
	pq, _ := extensions["persistedQuery"].(map[string]any)
	if h, _ := pq["sha256Hash"].(string); h != "" {
		return "sha256:" + h
	}
	return ""
}

// TrustedDocs is a set of persisted documents and the plans built for
// them, kept per (id, operationName).
type TrustedDocs struct {
	docs    map[string]string
	Enforce bool

	mu     sync.RWMutex
	schema *graphql.Schema // the schema plans was built against
	plans  map[trustedKey]graphql.PlanResult
}

type trustedKey struct{ id, operationName string }

func NewTrustedDocs(docs map[string]string, enforce bool) *TrustedDocs {
	return &TrustedDocs{
		docs:    docs,
		Enforce: enforce,
		plans:   make(map[trustedKey]graphql.PlanResult, len(docs)),
	}
}

// Lookup returns the document for id, trying it as sent and then
// without an algorithm prefix, so "sha256:abc" and "abc" match either
// manifest spelling. The returned key is the manifest's spelling.
func (t *TrustedDocs) Lookup(id string) (key, doc string, ok bool) {
	if doc, ok := t.docs[id]; ok {
		return id, doc, true
	}
	if _, bare, ok := strings.Cut(id, ":"); ok {
		if doc, ok := t.docs[bare]; ok {
			return bare, doc, true
		}
	}
	return "", "", false
}

// Plan returns the cached plan for (key, operationName) against
// schema, building it through cache on first use. A different schema
// (a gateway rebuilt its schema) drops the kept plans. Bounded:
// operationName comes from the client, so past a few entries per
// document new keys are planned but not kept.
func (t *TrustedDocs) Plan(cache *graphql.PlanCache, schema *graphql.Schema, key, doc, operationName string) graphql.PlanResult {
	k := trustedKey{key, operationName}
	t.mu.RLock()
	pr, ok := t.plans[k]
	same := t.schema == schema
	t.mu.RUnlock()
	if ok && same {
		return pr
	}
	pr = cache.Get(schema, doc, operationName)
	t.mu.Lock()
	if t.schema != schema {
		t.schema = schema
		clear(t.plans)
	}
	if len(t.plans) < 4*len(t.docs)+16 {
		t.plans[k] = pr
	}
	t.mu.Unlock()
	return pr
}
