package gateway

import "testing"

// With WithDocNormalization the plan cache must key on directives and
// variable defaults: queries differing only there get distinct plans
// (IodeSystems/graphql-go#9, fixed in v2.1.1).
func TestDocNormalization_DirectivesAndDefaultsNotShared(t *testing.T) {
	h, _ := limitsGateway(t, WithDocNormalization())

	post := func(q string) limitsResp {
		t.Helper()
		r := postGraphQL(t, h, map[string]any{"query": q})
		if r.Code != 200 || len(r.Errors) > 0 {
			t.Fatalf("%q: code=%d errors=%v", q, r.Code, r.Errors)
		}
		return r
	}
	has := func(r limitsResp) bool {
		m, _ := r.Data["test"].(map[string]any)
		_, ok := m["getThing"]
		return ok
	}

	// Directive: skip true first, then false.
	if has(post(`{ test { getThing(id:"x") @skip(if: true) { id } } }`)) {
		t.Fatal("@skip(if:true): field present")
	}
	if !has(post(`{ test { getThing(id:"x") @skip(if: false) { id } } }`)) {
		t.Fatal("@skip(if:false): field missing (plan shared with @skip(if:true))")
	}

	// Variable default: same shape, different defaults.
	if has(post(`query Q($s: Boolean = true) { test { getThing(id:"y") @skip(if: $s) { id } } }`)) {
		t.Fatal("default s=true: field present")
	}
	if !has(post(`query Q($s: Boolean = false) { test { getThing(id:"y") @skip(if: $s) { id } } }`)) {
		t.Fatal("default s=false: field missing (plan shared across defaults)")
	}
}
