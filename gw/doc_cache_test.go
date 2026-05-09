package gateway

import (
	"testing"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

func TestDocCacheLookupMissThenHit(t *testing.T) {
	c := newDocCache(8, 0)
	schema := &graphql.Schema{}
	if _, ok := c.lookup(schema, "{x}"); ok {
		t.Fatal("empty cache should miss")
	}
	c.store(schema, "{x}", &ast.Document{}, nil)
	e, ok := c.lookup(schema, "{x}")
	if !ok || e == nil {
		t.Fatalf("expected hit, got ok=%v entry=%v", ok, e)
	}
	if c.hits.Load() != 1 || c.misses.Load() != 1 {
		t.Fatalf("hits=%d misses=%d, want 1/1", c.hits.Load(), c.misses.Load())
	}
}

func TestDocCacheSchemaInvalidates(t *testing.T) {
	c := newDocCache(8, 0)
	schemaA := &graphql.Schema{}
	schemaB := &graphql.Schema{}
	c.store(schemaA, "{x}", &ast.Document{}, nil)
	if _, ok := c.lookup(schemaB, "{x}"); ok {
		t.Fatal("entry validated against schemaA must not satisfy schemaB lookup")
	}
	// And the stale entry must be gone.
	if _, ok := c.lookup(schemaA, "{x}"); ok {
		t.Fatal("stale entry should have been evicted on first cross-schema lookup")
	}
}

func TestDocCacheLRUEviction(t *testing.T) {
	c := newDocCache(2, 0)
	schema := &graphql.Schema{}
	c.store(schema, "a", &ast.Document{}, nil)
	c.store(schema, "b", &ast.Document{}, nil)
	c.store(schema, "c", &ast.Document{}, nil) // evicts "a" (oldest)
	if _, ok := c.lookup(schema, "a"); ok {
		t.Fatal("LRU eviction: 'a' should have been evicted")
	}
	if _, ok := c.lookup(schema, "b"); !ok {
		t.Fatal("'b' should still be cached")
	}
	// Looking up 'b' moved it to the front; 'c' is now LRU.
	// Storing 'd' should evict 'c'.
	c.store(schema, "d", &ast.Document{}, nil)
	if _, ok := c.lookup(schema, "c"); ok {
		t.Fatal("'c' should have been evicted after 'b' moved to front")
	}
	if _, ok := c.lookup(schema, "b"); !ok {
		t.Fatal("'b' should still be cached after MoveToFront")
	}
	if _, ok := c.lookup(schema, "d"); !ok {
		t.Fatal("'d' should be cached")
	}
}

func TestDocCacheSkipsLargeQueries(t *testing.T) {
	c := newDocCache(8, 16) // 16-byte cap
	schema := &graphql.Schema{}
	tooBig := "0123456789abcdefXX" // 18 bytes
	c.store(schema, tooBig, &ast.Document{}, nil)
	if _, ok := c.lookup(schema, tooBig); ok {
		t.Fatal("over-cap query should not have been cached")
	}
	c.store(schema, "small", &ast.Document{}, nil)
	if _, ok := c.lookup(schema, "small"); !ok {
		t.Fatal("under-cap query should be cached")
	}
}

func TestDocCacheNilSafe(t *testing.T) {
	var c *docCache
	if _, ok := c.lookup(nil, "x"); ok {
		t.Fatal("nil cache lookup must return false")
	}
	// store on nil is a no-op
	c.store(nil, "x", &ast.Document{}, nil)
}
