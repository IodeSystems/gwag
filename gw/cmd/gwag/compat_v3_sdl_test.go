package main

import (
	"fmt"
	"testing"
)

// TestCompat_SDLEmptyBraces pins how `gwag ctl schema diff` / snapshot
// parse SDL with empty definition bodies.
//
// v3: graphql-go v3 follows the spec: `type Foo {}` is a syntax error
// and the brace-less `type Foo` parses. On v2.1.1 it was the other way
// round.
func TestCompat_SDLEmptyBraces(t *testing.T) {
	cases := []struct{ name, sdl, want string }{
		{"object-empty-braces", "type Foo {}", "parse=error canon=error"},
		{"input-empty-braces", "input Foo {}", "parse=error canon=error"},
		{"interface-empty-braces", "interface Foo {}", "parse=error canon=error"},
		{"enum-empty-braces", "enum Foo {}", "parse=error canon=error"},
		{"object-no-braces", "type Foo", "parse=ok canon=ok"},
		{"object-one-field", "type Foo { a: Int }", "parse=ok canon=ok"},
		{"query-empty-braces-with-others", "type Query { a: Int }\ntype Foo {}\n", "parse=error canon=error"},
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
