package gqlgenimpl

// The hand-written GraphQL baseline: gqlgen serving the same two
// operations gat projects out of huma.
//
// This is what "don't use gat, write GraphQL natively" costs. gqlgen
// resolves through generated, type-specialised code — no reflection,
// no IR — so it is the honest ceiling for a Go GraphQL server on this
// workload. The gap to gat is what the translation layer buys and
// costs.

import (
	"net/http"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/iodesystems/gwag/compare/gatbench/gqlgenimpl/generated"
)

// New returns a mux with the gqlgen server mounted at /graphql.
//
// Caching is configured the way gqlgen's own recommended server does:
// a parsed-query LRU and an automatic-persisted-query cache. Without
// them gqlgen re-parses on every request, which would make this a
// comparison of cache configuration rather than of executors.
func New() *http.ServeMux {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &Resolver{},
	}))
	srv.AddTransport(transport.POST{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	srv.Use(extension.Introspection{})
	srv.Use(extension.AutomaticPersistedQuery{Cache: lru.New[string](100)})

	mux := http.NewServeMux()
	mux.Handle("/graphql", srv)
	return mux
}

// Queries the benchmarks fire. Same selection set as the gat queries,
// minus gat's namespace wrapper — a hand-written schema has no
// per-service namespace because it fronts exactly one service.
const (
	GetQuery  = `{ getProject(id: "p1") { id name tags } }`
	ListQuery = `{ listProjects(limit: 25) { projects { id name tags } } }`
)
