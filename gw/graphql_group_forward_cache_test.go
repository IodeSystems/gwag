package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
)

// graphqlGroupDispatcher caches its printed upstream query per
// (field, operation) AST node. Different selections, arguments,
// aliases and variables must each forward their own text, with the
// plan cache on and off.
func TestGraphQLIngest_GroupForwardPrintCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"plan cache on", nil},
		{"plan cache off", []Option{WithoutDocCache()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := newNestedRemoteFixture(t)
			rf.queryHandler = func(query string, vars map[string]any) any {
				return map[string]any{
					"greeter": map[string]any{
						"hello": map[string]any{"greeting": "Hello"},
						"echo":  "ack",
					},
					"g2": map[string]any{
						"echo": "ack2",
					},
				}
			}
			gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test"))}, tc.opts...)...)
			t.Cleanup(gw.Close)
			if err := gw.AddGraphQL(rf.server.URL, As("svc")); err != nil {
				t.Fatalf("AddGraphQL: %v", err)
			}
			srv := httptest.NewServer(gw.Handler())
			t.Cleanup(srv.Close)

			post := func(body map[string]any) string {
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
				return string(out)
			}
			cases := []struct {
				body map[string]any
				want []string
				not  []string
				resp string
			}{
				{map[string]any{"query": `{ svc { greeter { echo(msg: "a") } } }`}, []string{`echo(msg: "a")`}, []string{"hello"}, ""},
				{map[string]any{"query": `{ svc { greeter { echo(msg: "b") } } }`}, []string{`echo(msg: "b")`}, []string{`"a"`}, ""},
				{map[string]any{"query": `{ svc { greeter { echo(msg: "b") hello(name: "n") { greeting } } } }`}, []string{"hello", "greeting"}, nil, "Hello"},
				{map[string]any{"query": `query Q($m: String!) { svc { greeter { echo(msg: $m) } } }`, "variables": map[string]any{"m": "v"}}, []string{"$m: String!", "echo(msg: $m)"}, nil, ""},
				{map[string]any{"query": `{ svc { g2: greeter { echo(msg: "c") } } }`}, []string{"g2: greeter"}, nil, "ack2"},
			}
			// Twice: the second pass is served from the cache.
			for pass := 0; pass < 2; pass++ {
				for i, c := range cases {
					out := post(c.body)
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
					if c.resp != "" && !strings.Contains(out, c.resp) {
						t.Errorf("pass %d case %d: response %s, missing %q", pass, i, out, c.resp)
					}
				}
			}
		})
	}
}

// Subscription forwarding caches per (field, operation) node too.
// Distinct subscriptions print distinct text; a repeat of the same
// parsed node hits the cache.
func TestGraphQLIngest_SubscriptionForwardPrintCache(t *testing.T) {
	d := &graphQLDispatcher{
		mirror:          newGraphQLMirror(&graphQLSource{namespace: "pets"}),
		remoteFieldName: "tick",
	}
	info := func(src string) *graphql.ResolveInfo {
		t.Helper()
		doc, err := parser.Parse(parser.ParseParams{Source: src})
		if err != nil {
			t.Fatal(err)
		}
		op := doc.Definitions[0].(*ast.OperationDefinition)
		return &graphql.ResolveInfo{
			FieldASTs: []*ast.Field{op.SelectionSet.Selections[0].(*ast.Field)},
			Operation: op,
		}
	}
	a := info(`subscription { pets_tick }`)
	b := info(`subscription S($n: Int) { pets_tick(n: $n) }`)
	qa, err := d.forwardSubscriptionQuery(a)
	if err != nil {
		t.Fatal(err)
	}
	qb, err := d.forwardSubscriptionQuery(b)
	if err != nil {
		t.Fatal(err)
	}
	if qa == qb || !strings.Contains(qb, "$n: Int") || strings.Contains(qa, "$n") {
		t.Fatalf("stale or wrong text: %q / %q", qa, qb)
	}
	if !strings.HasPrefix(strings.TrimSpace(qa), "subscription") {
		t.Fatalf("not a subscription: %q", qa)
	}
	if again, _ := d.forwardSubscriptionQuery(a); again != qa {
		t.Fatalf("repeat differs: %q vs %q", again, qa)
	}
	if n := len(d.printed.m); n != 2 {
		t.Fatalf("cache entries = %d, want 2", n)
	}
}
