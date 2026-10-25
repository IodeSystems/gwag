package gat

import (
	"fmt"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/gqlerrors"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/kinds"
	"github.com/IodeSystems/graphql-go/v2/language/visitor"
)

// MaxDepth refuses queries whose fields nest deeper than n (n <= 0: no
// limit). Every field level counts, the namespace field included:
// `{ Demo { getProject { owner { name } } } }` is 4 deep. The check is
// a validation rule, so it runs once per distinct query and its verdict
// is cached with the plan.
//
// Call before the schema builds. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) MaxDepth(n int) *Gateway {
	g.maxDepth = n
	return g
}

// DisableIntrospection refuses queries selecting __schema or __type
// (__typename stays allowed) and stops serving {prefix}/schema/*, so a
// production endpoint does not hand out its schema. Clients generate
// their code from the schema at build time, against a gateway where it
// is on. Under enforced TrustedDocuments introspection cannot run
// anyway; this covers gateways that still accept query text.
//
// Call before RegisterHuma. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) DisableIntrospection() *Gateway {
	g.noIntrospection = true
	return g
}

// validationRules are the extra rules the plan cache runs with the
// spec rules for this gateway's settings.
func (g *Gateway) validationRules() []graphql.ValidationRuleFn {
	var rules []graphql.ValidationRuleFn
	if g.maxDepth > 0 {
		rules = append(rules, maxDepthRule(g.maxDepth))
	}
	if g.noIntrospection {
		rules = append(rules, noIntrospectionRule)
	}
	return rules
}

func maxDepthRule(limit int) graphql.ValidationRuleFn {
	return func(ctx *graphql.ValidationContext) *graphql.ValidationRuleInstance {
		return &graphql.ValidationRuleInstance{VisitorOpts: &visitor.VisitorOptions{
			KindFuncMap: map[string]visitor.NamedVisitFuncs{
				kinds.OperationDefinition: {Kind: func(p visitor.VisitFuncParams) (string, interface{}) {
					if op, ok := p.Node.(*ast.OperationDefinition); ok {
						if d := selectionDepth(ctx, op.SelectionSet, map[string]bool{}); d > limit {
							ctx.ReportError(gqlerrors.NewError(
								fmt.Sprintf("query depth %d exceeds the limit of %d", d, limit),
								[]ast.Node{op}, "", nil, []int{}, nil))
						}
					}
					return visitor.ActionSkip, nil
				}},
			},
		}}
	}
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

func noIntrospectionRule(ctx *graphql.ValidationContext) *graphql.ValidationRuleInstance {
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
