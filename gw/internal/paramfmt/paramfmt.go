// Package paramfmt formats GraphQL argument values as the text of an
// upstream request: OpenAPI path and query parameters and multipart
// fields. Shared by gwag (gw/openapi.go) and gat (gw/gat/openapi.go).
package paramfmt

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// String formats v for a URL parameter or form field. Numbers never
// use exponent form: float64(3e9) is "3000000000", 2.5 is "2.5", and
// a json.Number keeps its digits. ok is false for nil: a null
// argument sends no parameter.
func String(v any) (s string, ok bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), true
	case json.Number:
		return string(x), true
	}
	return fmt.Sprint(v), true
}
