package ir

import (
	"encoding/json"
	"strconv"

	"github.com/IodeSystems/graphql-go"
	"github.com/IodeSystems/graphql-go/language/ast"
)

// StandardScalars constructs the Long + JSON scalars used by every
// IR-rendered GraphQL schema. Per-source IRTypeBuilders share these
// so the final graphql.Schema sees one named instance — graphql-go
// rejects two scalars sharing a Name even when they're equivalently
// shaped.
//
// Stability: stable
func StandardScalars() (long *graphql.Scalar, jsonValue *graphql.Scalar) {
	long = graphql.NewScalar(graphql.ScalarConfig{
		Name: "Long",
		Description: "64-bit integer encoded as a decimal string. " +
			"OpenAPI integer fields with format=int64/uint64 land here; " +
			"graphql-go's built-in Int is signed 32-bit and would lose " +
			"precision (or null out entirely) for values above 2^31.",
		Serialize: func(v any) any {
			switch x := v.(type) {
			case float64:
				return strconv.FormatInt(int64(x), 10)
			case int64:
				return strconv.FormatInt(x, 10)
			case uint64:
				return strconv.FormatUint(x, 10)
			case int:
				return strconv.Itoa(x)
			case string:
				return x
			case json.Number:
				return x.String()
			}
			return nil
		},
		ParseValue: func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any {
			switch x := v.(type) {
			case *ast.StringValue:
				return x.Value
			case *ast.IntValue:
				return x.Value
			}
			return nil
		},
	})
	jsonValue = graphql.NewScalar(graphql.ScalarConfig{
		Name:         "JSON",
		Description:  "Untyped JSON value (used as a fallback for OpenAPI schemas that can't be mapped exactly).",
		Serialize:    func(v any) any { return v },
		ParseValue:   func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any { return v },
	})
	return long, jsonValue
}
