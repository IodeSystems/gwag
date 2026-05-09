package gateway

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

// docCache is a bounded LRU of parsed + validated GraphQL documents,
// keyed on the raw query string. graphql-go's parse + validate path
// allocates ~24 KB per request just from the visitor, dominating CPU
// and GC at high RPS for clients that re-issue the same query (every
// real client does). On hit we skip both parse and ValidateDocument
// and feed the cached AST directly to graphql.Execute.
//
// Each entry is bound to the *graphql.Schema pointer it was validated
// against. On schema rebuild (g.schema.Load() pointer changes), stale
// entries fall out at lookup — self-cleaning, no schema-watch
// goroutine needed.
//
// Per-query bytes are bounded by maxQueryBytes (queries above that cap
// skip the cache); entry count bounded by maxEntries with classic
// list+map LRU eviction.
//
// Variables and operationName are NOT part of the key:
//   - variables are runtime values, irrelevant to AST or validation;
//   - operationName picks an operation in a multi-op document at
//     Execute time, not validation time, so the cached AST is
//     reusable across operationName values.
type docCache struct {
	mu            sync.Mutex
	maxEntries    int
	maxQueryBytes int
	entries       map[string]*list.Element
	order         *list.List

	hits   atomic.Uint64
	misses atomic.Uint64
}

type docCacheItem struct {
	key string
	e   *docCacheEntry
}

type docCacheEntry struct {
	schema *graphql.Schema
	doc    *ast.Document
	plan   *graphql.Plan // nil when validation failed (errs is set)
	errs   []gqlerrors.FormattedError
}

func newDocCache(maxEntries, maxQueryBytes int) *docCache {
	return &docCache{
		maxEntries:    maxEntries,
		maxQueryBytes: maxQueryBytes,
		entries:       make(map[string]*list.Element, maxEntries),
		order:         list.New(),
	}
}

// shouldCache returns true when a query of the given byte length
// should be considered for the cache. Above maxQueryBytes we skip
// outright to keep total memory bounded without per-byte accounting.
func (c *docCache) shouldCache(querySize int) bool {
	if c == nil {
		return false
	}
	return c.maxQueryBytes <= 0 || querySize <= c.maxQueryBytes
}

func (c *docCache) lookup(schema *graphql.Schema, query string) (*docCacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[query]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	item := el.Value.(*docCacheItem)
	if item.e.schema != schema {
		c.order.Remove(el)
		delete(c.entries, query)
		c.misses.Add(1)
		return nil, false
	}
	c.order.MoveToFront(el)
	c.hits.Add(1)
	return item.e, true
}

func (c *docCache) store(schema *graphql.Schema, query string, doc *ast.Document, plan *graphql.Plan, errs []gqlerrors.FormattedError) {
	if c == nil || c.maxEntries <= 0 {
		return
	}
	if !c.shouldCache(len(query)) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[query]; ok {
		item := el.Value.(*docCacheItem)
		item.e.schema = schema
		item.e.doc = doc
		item.e.plan = plan
		item.e.errs = errs
		c.order.MoveToFront(el)
		return
	}
	item := &docCacheItem{key: query, e: &docCacheEntry{schema: schema, doc: doc, plan: plan, errs: errs}}
	el := c.order.PushFront(item)
	c.entries[query] = el
	for c.order.Len() > c.maxEntries {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		oldItem := oldest.Value.(*docCacheItem)
		c.order.Remove(oldest)
		delete(c.entries, oldItem.key)
	}
}
