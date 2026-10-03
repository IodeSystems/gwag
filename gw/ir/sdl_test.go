package ir

import (
	"strings"
	"testing"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
)

// TestWriteDescriptionParses checks the description escaping with a
// parser: the SDL must parse, and the parsed description must equal
// the input.
func TestWriteDescriptionParses(t *testing.T) {
	cases := []string{
		`plain`,
		`ends with a quote: "x"`,
		`ends with two quotes: ""`,
		`ends with an escaped quote: \"`,
		`"x" starts with a quote`,
		`has a "quoted" word in the middle`,
		`has a triple """ quote`,
		`ends with a triple """`,
		`four """" quotes`,
		"two\nlines ending in a quote \"",
	}
	for _, desc := range cases {
		t.Run(desc, func(t *testing.T) {
			var b strings.Builder
			writeDescription(&b, "", desc)
			b.WriteString("type T {\n")
			writeDescription(&b, "  ", desc)
			b.WriteString("  f: Int\n}\n")
			sdl := b.String()
			doc, err := parser.Parse(parser.ParseParams{Source: sdl})
			if err != nil {
				t.Fatalf("parse %q: %v", sdl, err)
			}
			def, ok := doc.Definitions[0].(*ast.ObjectDefinition)
			if !ok || def.Description == nil {
				t.Fatalf("no description parsed from %q", sdl)
			}
			if got := def.Description.Value; got != desc {
				t.Errorf("type description = %q, want %q (sdl %q)", got, desc, sdl)
			}
			if f := def.Fields[0]; f.Description == nil || f.Description.Value != desc {
				t.Errorf("field description = %+v, want %q (sdl %q)", f.Description, desc, sdl)
			}
		})
	}
}
