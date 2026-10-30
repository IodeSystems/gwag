package gat

import (
	"sync"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/IodeSystems/graphql-go/v2/language/source"

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
	if g.maxOps > 0 {
		rules = append(rules, gqlguard.MaxOperationsRule(g.maxOps))
	}
	if g.maxDepth > 0 {
		rules = append(rules, gqlguard.MaxDepthRule(g.maxDepth))
	}
	if g.noIntrospection {
		rules = append(rules, gqlguard.NoIntrospectionRule)
	}
	return rules
}

// opCountCache holds how many operations a batched document selects
// (gqlguard.CountOperations), so the batch-wide MaxOperations check
// parses each distinct document once. Counts depend on the schema; a
// replaced one drops them.
type opCountCache struct {
	mu     sync.RWMutex
	counts map[exportKey]int
	schema *graphql.Schema
}

// count returns the operations the document's selected operation
// selects; 0 for a document that does not parse (planning reports it).
func (c *opCountCache) count(schema *graphql.Schema, query, operationName string) int {
	key := exportKey{query, operationName}
	c.mu.RLock()
	n, ok := c.counts[key]
	same := c.schema == schema
	c.mu.RUnlock()
	if ok && same {
		return n
	}
	if doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})}); err == nil {
		n = gqlguard.CountOperations(schema, doc, operationName)
	}
	c.mu.Lock()
	if c.counts == nil || c.schema != schema {
		c.counts = make(map[exportKey]int)
		c.schema = schema
	}
	if len(c.counts) < exportCacheEntries {
		c.counts[key] = n
	}
	c.mu.Unlock()
	return n
}
