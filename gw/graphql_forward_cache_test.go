package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream query text is cached per (field, operation) AST node.
// Queries that differ in arguments, selection, variables or operation
// must each forward their own text, with the plan cache on and off,
// and a fragment shared by two operations must carry the variable
// definitions it uses from each operation. $y is used only by the
// @include the gateway evaluates, so it is not forwarded: an upstream
// rejects a defined variable the query does not use.
func TestGraphQLIngest_ForwardPrintCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"plan cache on", nil},
		{"plan cache off", []Option{WithoutDocCache()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := newRemoteFixture(t)
			rf.queryHandler = func(string, map[string]any) any {
				return map[string]any{"user": map[string]any{"id": "1", "name": "a", "role": "ADMIN"}}
			}
			gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test"))}, tc.opts...)...)
			t.Cleanup(gw.Close)
			if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
				t.Fatalf("AddGraphQL: %v", err)
			}
			srv := httptest.NewServer(gw.Handler())
			t.Cleanup(srv.Close)

			post := func(body map[string]any) {
				t.Helper()
				b, _ := json.Marshal(body)
				resp, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(string(b)))
				if err != nil {
					t.Fatal(err)
				}
				out, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if strings.Contains(string(out), "errors") {
					t.Fatalf("response errors: %s", out)
				}
			}
			// The forwarded field sits in a fragment both operations
			// spread, with different variable definitions. Today the
			// plan cache parses once per (query, operationName), so the
			// two never share a node; the cache key keeps the operation
			// so this stays right if that changes.
			const frag = `fragment F on PetsQueryNamespace { user(id: $x) { id name } }
query A($x: ID!) { pets { ...F } }
query B($x: ID!, $y: Boolean = true) { pets @include(if: $y) { ...F } }`
			cases := []struct {
				body map[string]any
				want []string
				not  []string
			}{
				{map[string]any{"query": `{ pets { user(id: "1") { id } } }`}, []string{`user(id: "1")`}, []string{"name"}},
				{map[string]any{"query": `{ pets { user(id: "2") { id } } }`}, []string{`user(id: "2")`}, nil},
				{map[string]any{"query": `{ pets { user(id: "2") { id name } } }`}, []string{"name"}, nil},
				{map[string]any{"query": `query Q($v: ID!) { pets { user(id: $v) { id } } }`, "variables": map[string]any{"v": "3"}}, []string{"$v: ID!", "user(id: $v)"}, nil},
				{map[string]any{"query": frag, "operationName": "A", "variables": map[string]any{"x": "4"}}, []string{"$x: ID!"}, []string{"$y"}},
				{map[string]any{"query": frag, "operationName": "B", "variables": map[string]any{"x": "5"}}, []string{"$x: ID!"}, []string{"$y"}},
			}
			// Twice: the second pass is served from the cache.
			for pass := 0; pass < 2; pass++ {
				for i, c := range cases {
					post(c.body)
					got := *rf.lastQuery.Load()
					for _, w := range c.want {
						if !strings.Contains(got, w) {
							t.Errorf("pass %d case %d: forwarded %q, missing %q", pass, i, got, w)
						}
					}
					for _, n := range c.not {
						if strings.Contains(got, n) {
							t.Errorf("pass %d case %d: forwarded %q, has %q", pass, i, got, n)
						}
					}
				}
			}
		})
	}
}
