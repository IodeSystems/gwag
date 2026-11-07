package gqlguard

import "testing"

func TestSelectsIntrospection(t *testing.T) {
	for _, c := range []struct {
		q         string
		may, sure bool
	}{
		{`{ __schema { types { name } } }`, true, true},
		{`{ __type(name: "X") { name } }`, true, true},
		{`{ a { __typename } }`, false, false},
		{`{ __typename }`, false, false},
		{`{ a(s: "__schema") { b } }`, true, false},
		{`# __type
{ a }`, true, false},
		{`query A { a } query B { ...F } fragment F on Query { __schema { queryType { name } } }`, true, true},
		{`{ ... on Query { __type(name: "Q") { name } } }`, true, true},
		{`{ __schemas }`, false, false},
		{`{ __schema`, true, false}, // does not parse
		{`{ x__type }`, true, false},
	} {
		if got := MayIntrospect(c.q); got != c.may {
			t.Errorf("MayIntrospect(%q)=%v, want %v", c.q, got, c.may)
		}
		if got := SelectsIntrospection(c.q); got != c.sure {
			t.Errorf("SelectsIntrospection(%q)=%v, want %v", c.q, got, c.sure)
		}
	}
}

func TestMayIntrospectAllocs(t *testing.T) {
	q := `query Q($id: ID!) { pets { getPet(id: $id) { id name __typename owner { name } } } }`
	if n := testing.AllocsPerRun(100, func() { MayIntrospect(q) }); n != 0 {
		t.Errorf("MayIntrospect allocates %v", n)
	}
}
