package main

import (
	"fmt"
	"strings"
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

// TestParseSDL_EmptyBracesHint: the parse error for `{}` says to omit
// the braces.
func TestParseSDL_EmptyBracesHint(t *testing.T) {
	for _, parse := range []func(string) error{
		func(s string) error { _, err := parseSchemaModel(s); return err },
		func(s string) error { _, err := canonGraphQL(s); return err },
	} {
		err := parse("type Query { a: Int }\ntype Foo {}\n")
		if err == nil || !strings.Contains(err.Error(), "empty braces are invalid SDL; omit them (`type Foo {}` -> `type Foo`)") {
			t.Errorf("error: %v", err)
		}
		if err := parse("type Query { a(x: Int = 1 }"); err == nil || strings.Contains(err.Error(), "empty braces") {
			t.Errorf("unrelated error carries the hint: %v", err)
		}
	}
}

// TestParseSchemaModel_NullDefault: a `= null` default reads as "null"
// (was "<nil>").
func TestParseSchemaModel_NullDefault(t *testing.T) {
	m, err := parseSchemaModel("type Query { f(a: String = null): Int }\ninput In { b: String = null }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.objects["Query"].fields["f"].args["a"].defaultValue; got != "null" {
		t.Errorf("arg default %q", got)
	}
	if got := m.inputs["In"].fields["b"].defaultValue; got != "null" {
		t.Errorf("input field default %q", got)
	}
}
