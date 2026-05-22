package gat

// Coverage for the int64-as-string coercion in assignValue. protojson
// encodes proto int64 / uint64 as JSON strings (to dodge JavaScript's
// Number.MAX_SAFE_INTEGER cap); the bind path must accept those when
// the consumer's Go field type is numeric, both for top-level args
// and for fields nested inside a body struct.

import (
	"reflect"
	"testing"
)

// TestAssignValue_StringIntoInt64 — scalar path/query case.
func TestAssignValue_StringIntoInt64(t *testing.T) {
	var dst int64
	if err := assignValue(reflect.ValueOf(&dst).Elem(), "1234567890123"); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst != 1234567890123 {
		t.Errorf("dst = %d; want 1234567890123", dst)
	}
}

func TestAssignValue_StringIntoInt(t *testing.T) {
	var dst int
	if err := assignValue(reflect.ValueOf(&dst).Elem(), "5"); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst != 5 {
		t.Errorf("dst = %d; want 5", dst)
	}
}

func TestAssignValue_StringIntoUint64(t *testing.T) {
	var dst uint64
	if err := assignValue(reflect.ValueOf(&dst).Elem(), "9007199254740993"); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst != 9007199254740993 {
		t.Errorf("dst = %d", dst)
	}
}

// TestAssignValue_OverflowRejected — a value that doesn't fit the
// destination int kind is a hard error, not silent truncation.
func TestAssignValue_OverflowRejected(t *testing.T) {
	var dst int8
	if err := assignValue(reflect.ValueOf(&dst).Elem(), "1000"); err == nil {
		t.Errorf("expected overflow error for int8 = 1000")
	}
}

// TestAssignValue_NumberStillWorks — non-string numeric input
// (json.Unmarshal already handled the simple path) keeps working.
func TestAssignValue_NumberStillWorks(t *testing.T) {
	var dst int64
	if err := assignValue(reflect.ValueOf(&dst).Elem(), float64(42)); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst != 42 {
		t.Errorf("dst = %d; want 42", dst)
	}
}

// TestAssignValue_NestedBodyStruct — the protojson-encoded body has
// stringified int64s scattered through a nested struct; deep walk
// applies the same coercion to each field.
func TestAssignValue_NestedBodyStruct(t *testing.T) {
	type inner struct {
		Tokens int64 `json:"tokens"`
		Note   string
	}
	type body struct {
		ContextTokens int64 `json:"context_tokens"`
		Nested        inner
	}
	var dst body
	// Simulate gat's protojson-derived map.
	src := map[string]any{
		"context_tokens": "200000",
		"nested": map[string]any{
			"tokens": "42",
			"Note":   "hi",
		},
	}
	if err := assignValue(reflect.ValueOf(&dst).Elem(), src); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst.ContextTokens != 200000 {
		t.Errorf("ContextTokens = %d; want 200000", dst.ContextTokens)
	}
	if dst.Nested.Tokens != 42 {
		t.Errorf("Nested.Tokens = %d; want 42", dst.Nested.Tokens)
	}
	if dst.Nested.Note != "hi" {
		t.Errorf("Nested.Note = %q; want hi", dst.Nested.Note)
	}
}

// TestAssignValue_SliceOfStructs — repeated body fields land as
// []any whose elements are map[string]any; recurse on each.
func TestAssignValue_SliceOfStructs(t *testing.T) {
	type item struct {
		ID    string `json:"id"`
		Count int64  `json:"count"`
	}
	type body struct {
		Items []item `json:"items"`
	}
	var dst body
	src := map[string]any{
		"items": []any{
			map[string]any{"id": "a", "count": "1"},
			map[string]any{"id": "b", "count": "2"},
		},
	}
	if err := assignValue(reflect.ValueOf(&dst).Elem(), src); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if len(dst.Items) != 2 || dst.Items[0].Count != 1 || dst.Items[1].Count != 2 {
		t.Errorf("slice walk failed: %+v", dst.Items)
	}
}

// TestAssignValue_NonNumericStringFallsThrough — string into string
// destination still works (don't get overzealous with coercion).
func TestAssignValue_NonNumericStringFallsThrough(t *testing.T) {
	var dst string
	if err := assignValue(reflect.ValueOf(&dst).Elem(), "hello"); err != nil {
		t.Fatalf("assignValue: %v", err)
	}
	if dst != "hello" {
		t.Errorf("dst = %q", dst)
	}
}
