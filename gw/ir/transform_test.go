package ir

import (
	"testing"
)

func mkService(ns, ver string) *Service {
	return &Service{
		Namespace: ns, Version: ver,
		Types: map[string]*Type{},
	}
}

func TestFilter(t *testing.T) {
	a := mkService("greeter", "v1")
	b := mkService("greeter", "v2")
	c := mkService("library", "v1")
	all := []*Service{a, b, c}

	if got := Filter(all, nil); len(got) != 3 {
		t.Errorf("nil selectors: got %d, want 3", len(got))
	}

	// Match exact ns:ver.
	sels, _ := ParseSelectors("greeter:v1")
	got := Filter(all, sels)
	if len(got) != 1 || got[0] != a {
		t.Errorf("ns:ver: got %d entries, want 1 (greeter v1)", len(got))
	}

	// Bare namespace matches all versions.
	sels, _ = ParseSelectors("greeter")
	got = Filter(all, sels)
	if len(got) != 2 {
		t.Errorf("bare ns: got %d, want 2", len(got))
	}

	// Multiple selectors compose.
	sels, _ = ParseSelectors("greeter:v1,library")
	got = Filter(all, sels)
	if len(got) != 2 {
		t.Errorf("two selectors: got %d, want 2", len(got))
	}
}

func TestHideInternal(t *testing.T) {
	a := mkService("greeter", "v1")
	b := mkService("_internal", "v1")
	b.Internal = true
	out := HideInternal([]*Service{a, b})
	if len(out) != 1 || out[0] != a {
		t.Errorf("HideInternal kept %d services, want 1", len(out))
	}
}

func TestHidesStripFieldsByType(t *testing.T) {
	svc := mkService("svc", "v1")
	svc.Types["Pet"] = &Type{
		Name: "Pet", TypeKind: TypeObject,
		Fields: []*Field{
			{Name: "id", Type: TypeRef{Builtin: ScalarString}},
			{Name: "owner", Type: TypeRef{Named: "Owner"}},
			{Name: "name", Type: TypeRef{Builtin: ScalarString}},
		},
	}
	svc.Types["Owner"] = &Type{Name: "Owner", TypeKind: TypeObject}

	Hides([]*Service{svc}, map[string]bool{"Owner": true})

	pet := svc.Types["Pet"]
	if got := len(pet.Fields); got != 2 {
		t.Fatalf("Pet has %d fields after Hides, want 2", got)
	}
	for _, f := range pet.Fields {
		if f.Type.Named == "Owner" {
			t.Errorf("Owner field still present")
		}
	}
}

