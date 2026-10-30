package gat

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// IntegerScalar is the huma schema for a Go integer type that gat
// renders in GraphQL as its own scalar named name: a JSON number
// (whatever LongAsNumber says), exact within ±(2^53-1), accepting a
// number or a decimal string as input. REST and OpenAPI see
// `type: integer, format: int64` bounded to that range; proto sees
// int64. Return it from the type's huma.SchemaProvider method:
//
//	type Cents int64
//
//	func (Cents) Schema(huma.Registry) *huma.Schema {
//		return gat.IntegerScalar("Cents", "Money in integer cents.")
//	}
//
// The SDL declares `scalar Cents`, so GraphQL codegen can map it to a
// number type. The name is not namespace-prefixed.
//
// Stability: experimental
func IntegerScalar(name, description string) *huma.Schema {
	lo, hi := -maxSafeInteger, maxSafeInteger
	return &huma.Schema{
		Type:        huma.TypeInteger,
		Format:      "int64",
		Description: description,
		Minimum:     &lo,
		Maximum:     &hi,
		Extensions:  map[string]any{ir.ScalarExtension: name},
	}
}

// maxSafeInteger is 2^53-1, the largest integer a JSON number holds
// exactly in every client.
const maxSafeInteger = float64(1<<53 - 1)
