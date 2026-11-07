package gqlguard

import (
	"strings"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/IodeSystems/graphql-go/v2/language/source"
)

// MayIntrospect reports whether query text contains __schema or
// __type (not __typename). A cheap test that does not allocate, run on
// every request; true can be a false positive (the name in a string
// or a comment), which SelectsIntrospection settles.
func MayIntrospect(query string) bool {
	for i := 0; ; {
		j := strings.Index(query[i:], "__")
		if j < 0 {
			return false
		}
		rest := query[i+j+2:]
		if strings.HasPrefix(rest, "schema") && !identChar(rest, 6) {
			return true
		}
		if strings.HasPrefix(rest, "type") && !identChar(rest, 4) {
			return true
		}
		i += j + 2
	}
}

// identChar reports whether s[i] continues a GraphQL name.
func identChar(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// SelectsIntrospection reports whether the document selects __schema
// or __type anywhere (any operation, any fragment). A document that
// does not parse selects nothing.
func SelectsIntrospection(query string) bool {
	if !MayIntrospect(query) {
		return false
	}
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(query)})})
	if err != nil {
		return false
	}
	for _, def := range doc.Definitions {
		var set *ast.SelectionSet
		switch d := def.(type) {
		case *ast.OperationDefinition:
			set = d.SelectionSet
		case *ast.FragmentDefinition:
			set = d.SelectionSet
		}
		if selectsMeta(set) {
			return true
		}
	}
	return false
}

func selectsMeta(set *ast.SelectionSet) bool {
	if set == nil {
		return false
	}
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name != nil && (s.Name.Value == "__schema" || s.Name.Value == "__type") {
				return true
			}
			if selectsMeta(s.SelectionSet) {
				return true
			}
		case *ast.InlineFragment:
			if selectsMeta(s.SelectionSet) {
				return true
			}
		}
	}
	return false
}
