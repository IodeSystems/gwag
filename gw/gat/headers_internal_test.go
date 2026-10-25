package gat

import "testing"

func TestCacheControlRestrict(t *testing.T) {
	merge := func(a, b string) string {
		d := parseCacheControl(a)
		d.restrict(parseCacheControl(b))
		return d.String()
	}
	for _, c := range []struct{ a, b, want string }{
		{"public, max-age=60", "public, max-age=30", "public, max-age=30"},
		{"public, max-age=60", "private, max-age=120", "private, max-age=60"},
		{"public, max-age=60, s-maxage=600", "public, max-age=60, s-maxage=300", "public, max-age=60, s-maxage=300"},
		{"public, max-age=60, s-maxage=600", "private, max-age=60", "private, max-age=60"},
		{"max-age=60", "no-store", "no-store"},
		{"max-age=60", "no-cache", "no-cache, max-age=60"},
		{"public, max-age=60, immutable", "public, max-age=60", "public, max-age=60"},
		{"public, max-age=60, immutable", "public, max-age=30, immutable", "public, max-age=30, immutable"},
		{"max-age=60, stale-while-revalidate=30", "max-age=60", "max-age=60"},
		{"max-age=60, stale-while-revalidate=30", "max-age=60, stale-while-revalidate=10", "max-age=60, stale-while-revalidate=10"},
		{"max-age=60", "max-age=60, must-revalidate", "max-age=60, must-revalidate"},
		{"public", "public, max-age=10", "public, max-age=10"},
		{"max-age=bogus", "max-age=60", "max-age=0"},
	} {
		if got := merge(c.a, c.b); got != c.want {
			t.Errorf("%q + %q = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestETagMatches(t *testing.T) {
	for _, c := range []struct {
		inm, etag string
		want      bool
	}{
		{`"a"`, `"a"`, true},
		{`W/"a"`, `"a"`, true},
		{`"b", W/"a"`, `W/"a"`, true},
		{`*`, `"a"`, true},
		{`"b"`, `"a"`, false},
		{`"a"`, ``, false},
		{``, `"a"`, false},
	} {
		if got := etagMatches(c.inm, c.etag); got != c.want {
			t.Errorf("etagMatches(%q, %q) = %v", c.inm, c.etag, got)
		}
	}
}
