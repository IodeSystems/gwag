package main

import (
	"fmt"
	"testing"
)

// TestCompat_SDLEmptyBraces pins how `gwag ctl schema diff` / snapshot
// parse SDL with empty definition bodies on graphql-go v2.1.1.
//
// v2.1.1 behaviour; expected to change in graphql-go v3: `type Foo {}`
// is a syntax error. Flip the "{}" entries when v3 lands. The brace-less
// `type Foo` form is already an error on v2.1.1 (this parser requires
// braces on an object definition).
func TestCompat_SDLEmptyBraces(t *testing.T) {
	cases := []struct{ name, sdl, want string }{
		{"object-empty-braces", "type Foo {}", "parse=ok canon=ok"},
		{"input-empty-braces", "input Foo {}", "parse=ok canon=ok"},
		{"interface-empty-braces", "interface Foo {}", "parse=ok canon=ok"},
		{"enum-empty-braces", "enum Foo {}", "parse=ok canon=ok"},
		{"object-no-braces", "type Foo", "parse=error canon=error"},
		{"object-one-field", "type Foo { a: Int }", "parse=ok canon=ok"},
		{"query-empty-braces-with-others", "type Query { a: Int }\ntype Foo {}\n", "parse=ok canon=ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, perr := parseSchemaModel(c.sdl)
			_, cerr := canonGraphQL(c.sdl)
			got := fmt.Sprintf("parse=%s canon=%s", errStr(perr), errStr(cerr))
			if got != c.want {
				t.Errorf("sdl %q:\n  got  %s\n  want %s", c.sdl, got, c.want)
			}
		})
	}
}

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}
