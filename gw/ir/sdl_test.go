package ir

import (
	"strings"
	"testing"

	"github.com/IodeSystems/graphql-go/v3"
	"github.com/IodeSystems/graphql-go/v3/language/ast"
	"github.com/IodeSystems/graphql-go/v3/language/parser"
	"github.com/IodeSystems/graphql-go/v3/language/source"
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

// TestPrintSchemaSDL_ExplicitNullDefault: a `= null` default
// (graphql.ExplicitNull) prints as null, and the SDL parses.
func TestPrintSchemaSDL_ExplicitNullDefault(t *testing.T) {
	in := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:   "In",
		Fields: graphql.InputObjectConfigFieldMap{"a": &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: graphql.ExplicitNull}},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{"f": &graphql.Field{
			Type: graphql.String,
			Args: graphql.FieldConfigArgument{
				"s":   &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: graphql.ExplicitNull},
				"inp": &graphql.ArgumentConfig{Type: in},
			},
		}},
	})})
	if err != nil {
		t.Fatal(err)
	}
	sdl := PrintSchemaSDL(&schema)
	for _, want := range []string{"s: String = null", "a: String = null"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL lacks %q:\n%s", want, sdl)
		}
	}
	if _, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(sdl)})}); err != nil {
		t.Fatalf("printed SDL does not parse: %v\n%s", err, sdl)
	}
}
