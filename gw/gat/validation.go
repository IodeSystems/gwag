package gat

import (
	"github.com/IodeSystems/graphql-go/v2"

	"github.com/iodesystems/gwag/v2/gw/internal/gqlguard"
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
		rules = append(rules, gqlguard.MaxDepthRule(g.maxDepth))
	}
	if g.noIntrospection {
		rules = append(rules, gqlguard.NoIntrospectionRule)
	}
	return rules
}
