package ir

import (
	"strings"
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

func TestMultiVersionPrefix(t *testing.T) {
	v1 := mkService("greeter", "v1")
	v1.Types["HelloRequest"] = &Type{Name: "HelloRequest", TypeKind: TypeObject}
	v1.Operations = []*Operation{
		{Name: "hello", Kind: OpQuery, Output: &TypeRef{Named: "HelloRequest"}},
	}

	v2 := mkService("greeter", "v2")
	v2.Types["HelloRequest"] = &Type{Name: "HelloRequest", TypeKind: TypeObject}
	v2.Operations = []*Operation{
		{Name: "hello", Kind: OpQuery, Output: &TypeRef{Named: "HelloRequest"}},
	}

	out := MultiVersionPrefix([]*Service{v1, v2})
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}

	// Find the renamed v1 in out.
	var renamed *Service
	for _, s := range out {
		if s.Version == "v1" {
			renamed = s
		}
	}
	if renamed == nil {
		t.Fatal("v1 missing from output")
	}
	if _, ok := renamed.Types["v1_HelloRequest"]; !ok {
		t.Errorf("v1's HelloRequest not prefixed; got types: %v", typeKeys(renamed.Types))
	}
	if got := renamed.Operations[0].Name; got != "v1_hello" {
		t.Errorf("v1's hello not prefixed: %q", got)
	}
	if got := renamed.Operations[0].Output.Named; got != "v1_HelloRequest" {
		t.Errorf("v1's hello output ref not rewritten: %q", got)
	}
	if !strings.Contains(renamed.Operations[0].Deprecated, "v2 is current") {
		t.Errorf("v1's hello missing deprecation marker: %q", renamed.Operations[0].Deprecated)
	}
}

func typeKeys(m map[string]*Type) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
