package gateway

import (
	"fmt"
	"net/http"
	"testing"
)

// TestCompat_SkipIfNull pins @skip(if:) / @include(if:) with a null
// variable (graphql-go v3.1.1: `Argument "if" of non-null type
// "Boolean!" must not be null.`; a request error at the root, a field
// error on the parent below it). gwag's own collector below OpenAPI and
// proto ops answers the same. An omitted variable takes its default.
func TestCompat_SkipIfNull(t *testing.T) {
	type sc struct{ name, query, vars string }
	run := func(t *testing.T, h http.Handler, want map[string]string, cases []sc) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				status, body := compatPost(t, h, c.query, c.vars)
				compatCheck(t, want, c.name, fmt.Sprintf("http=%d %s", status, body))
			})
		}
	}
	t.Run("openapi", func(t *testing.T) {
		f := newCompatOpenAPI(t)
		run(t, f.h, compatSkipOpenAPIWant, []sc{
			{"root/null", `query($s: Boolean = true){ t @skip(if: $s) { qry { id } } }`, `{"s":null}`},
			{"root/absent", `query($s: Boolean = true){ t @skip(if: $s) { qry { id } } }`, `{}`},
			{"nested/null", `query($s: Boolean = true){ t { qry { id @include(if: $s) } } }`, `{"s":null}`},
			{"nested/absent", `query($s: Boolean = true){ t { qry { id @include(if: $s) } } }`, `{}`},
			{"nested/false", `query($s: Boolean = true){ t { qry { id @include(if: $s) } } }`, `{"s":false}`},
			{"nested-fragment/null", `query($s: Boolean = true){ t { qry { ...F } } } fragment F on t_Res { id @skip(if: $s) }`, `{"s":null}`},
			{"aliased/null", `query($s: Boolean = true){ x: t { y: qry { id @include(if: $s) } } }`, `{"s":null}`},
		})
	})
	t.Run("proto", func(t *testing.T) {
		gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("t")))
		t.Cleanup(gw.Close)
		if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
			t.Fatal(err)
		}
		run(t, gw.Handler(), compatSkipProtoWant, []sc{
			{"root/null", `query($s: Boolean = true){ col @skip(if: $s) { get { a } } }`, `{"s":null}`},
			{"root/absent", `query($s: Boolean = true){ col @skip(if: $s) { get { a } } }`, `{}`},
			{"nested/null", `query($s: Boolean = true){ col { get { a @include(if: $s) } } }`, `{"s":null}`},
			{"nested/absent", `query($s: Boolean = true){ col { get { a @include(if: $s) } } }`, `{}`},
			{"nested/false", `query($s: Boolean = true){ col { get { a @include(if: $s) } } }`, `{"s":false}`},
		})
	})
	t.Run("graphql", func(t *testing.T) {
		f := newCompatGraphQL(t)
		run(t, f.h, compatSkipGraphQLWant, []sc{
			{"root/null", `query($s: Boolean = true){ g @skip(if: $s) { echo } }`, `{"s":null}`},
			{"root/absent", `query($s: Boolean = true){ g @skip(if: $s) { echo } }`, `{}`},
			{"nested/null", `query($s: Boolean = true){ g { echo @skip(if: $s) } }`, `{"s":null}`},
			{"nested/absent", `query($s: Boolean = true){ g { echo @skip(if: $s) } }`, `{}`},
		})
	})
}
