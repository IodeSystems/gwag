package ir

import (
	"reflect"
	"testing"
)

func TestDropNullArgs(t *testing.T) {
	in := map[string]any{
		"a":    nil,
		"b":    "x",
		"body": map[string]any{"name": nil, "n": 1, "sub": map[string]any{"x": nil}},
		"list": []any{nil, map[string]any{"y": nil, "z": 2}},
	}
	want := map[string]any{
		"b":    "x",
		"body": map[string]any{"n": 1, "sub": map[string]any{}},
		"list": []any{nil, map[string]any{"z": 2}},
	}
	got := DropNullArgs(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if in["a"] != nil || len(in) != 4 || len(in["body"].(map[string]any)) != 3 {
		t.Fatal("input modified")
	}
	clean := map[string]any{"b": "x", "l": []any{nil, 1}}
	if got := DropNullArgs(clean); reflect.ValueOf(got).Pointer() != reflect.ValueOf(clean).Pointer() {
		t.Fatal("copied a map with no null key")
	}
	if got := DropNullArgs(nil); got != nil {
		t.Fatalf("nil: %#v", got)
	}
}
