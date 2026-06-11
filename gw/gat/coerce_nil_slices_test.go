package gat

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// coerceNilSlices replaces nil slices with empty ones so they marshal as `[]`
// not null — the runtime half of the non-null-list contract (a nil/empty Go
// slice must not serialize to JSON null, which would violate `[T!]!`).
func TestCoerceNilSlices(t *testing.T) {
	type Inner struct {
		Tags []string `json:"tags"`
	}
	type Body struct {
		Items  []int    `json:"items"`            // nil → []
		Filled []string `json:"filled"`           // kept
		Nested []Inner  `json:"nested"`           // nil → []
		One    Inner    `json:"one"`              // its nil Tags → []
		Ptr    *Inner   `json:"ptr"`              // recurse through pointer
		NilPtr *Inner   `json:"nilPtr,omitempty"` // nil pointer: skipped
	}
	out := &Body{
		Filled: []string{"a"},
		One:    Inner{},  // Tags nil
		Ptr:    &Inner{}, // Tags nil
	}
	coerceNilSlices(reflect.ValueOf(out))

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, want := range []string{`"items":[]`, `"nested":[]`, `"tags":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %s in %s", want, got)
		}
	}
	if strings.Contains(got, "null") {
		t.Errorf("no slice should serialize as null: %s", got)
	}
	if !strings.Contains(got, `"filled":["a"]`) {
		t.Errorf("non-nil slice should be preserved: %s", got)
	}
}
