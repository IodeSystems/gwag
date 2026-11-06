package ir

import (
	"fmt"
	"strings"
	"testing"
)

// nullabilityIntrospection holds one object whose fields cover the
// list and scalar nullability shapes GraphQL can spell.
const nullabilityIntrospection = `{
  "__schema": {
    "queryType": {"name": "Query"},
    "mutationType": null,
    "subscriptionType": null,
    "types": [
      {"kind": "OBJECT", "name": "Query", "fields": [
        {"name": "box", "args": [], "type": {"kind": "OBJECT", "name": "Box"}}
      ]},
      {"kind": "OBJECT", "name": "Item", "fields": [
        {"name": "id", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}
      ]},
      {"kind": "OBJECT", "name": "Box", "fields": [
        {"name": "a", "args": [], "type": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "SCALAR", "name": "Int"}}}}},
        {"name": "b", "args": [], "type": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}}},
        {"name": "c", "args": [], "type": {"kind": "LIST", "ofType": {"kind": "SCALAR", "name": "Int"}}},
        {"name": "d", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "OBJECT", "name": "Item"}}}},
        {"name": "e", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "Item"}}}}},
        {"name": "f", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}},
        {"name": "g", "args": [], "type": {"kind": "SCALAR", "name": "Int"}}
      ]}
    ]
  }
}`

// fieldShape spells a field's nullability the way GraphQL does.
func fieldShape(f *Field) string {
	return fmt.Sprintf("%s:%s rep=%v req=%v item=%v", f.Name, nestedShape(f.Type), f.Repeated, f.Required && !f.Nullable, f.ItemRequired)
}

func nestedShape(r TypeRef) string {
	if r.IsList() {
		return fmt.Sprintf("[%s item=%v]", nestedShape(r.List.Elem), r.List.ItemRequired)
	}
	return typeRefString(r)
}

func boxShapes(t *testing.T, svc *Service) map[string]string {
	t.Helper()
	box := svc.Types["Box"]
	if box == nil {
		t.Fatalf("Box missing: %v", svc.Types)
	}
	m := map[string]string{}
	for _, f := range box.Fields {
		m[f.Name] = fieldShape(f)
	}
	return m
}

// GraphQL list element nullability survives GraphQL → OpenAPI export
// → OpenAPI re-ingest, in 3.0 and 3.1 output.
func TestRenderOpenAPI_ListNullabilityRoundTrip(t *testing.T) {
	svc, err := IngestGraphQL([]byte(nullabilityIntrospection))
	if err != nil {
		t.Fatal(err)
	}
	svc.Namespace, svc.Version = "nb", "v1"
	want := boxShapes(t, svc)

	for _, version := range []string{"", "3.1"} {
		t.Run("v"+version, func(t *testing.T) {
			doc, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: version})
			if err != nil {
				t.Fatal(err)
			}
			raw := mustJSON(t, doc)
			got := boxShapes(t, IngestOpenAPI(loadSpec(t, raw)))
			for name, w := range want {
				if got[name] != w {
					t.Errorf("field %s:\n got  %s\n want %s", name, got[name], w)
				}
			}
			if version == "3.1" {
				s := string(raw)
				if strings.Contains(s, `"nullable"`) {
					t.Errorf("3.1+ output keeps 3.0 nullable:\n%s", s)
				}
				if !strings.Contains(s, `"null"`) {
					t.Errorf("3.1+ output has no null type:\n%s", s)
				}
			}
		})
	}
}

func TestRenderOpenAPI_ListNullabilityShape(t *testing.T) {
	svc, err := IngestGraphQL([]byte(nullabilityIntrospection))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatal(err)
	}
	props := doc.Components.Schemas["Box"].Value.Properties
	if props["b"].Value.Items.Value.Nullable {
		t.Error("[Int!] items marked nullable")
	}
	if !props["c"].Value.Items.Value.Nullable {
		t.Error("[Int] items not nullable")
	}
	if d := props["d"].Value.Items.Value; !d.Nullable || len(d.AllOf) != 1 {
		t.Error("[Item] items not a nullable allOf wrapper")
	}
	if props["e"].Value.Items.Ref == "" {
		t.Error("[Item!] items should stay a bare $ref")
	}
	if props["g"].Value.Nullable {
		t.Error("optional Int field marked nullable: absence is not null")
	}
	// [[Int]!]: the non-null inner list holds nullable ints.
	inner := props["a"].Value.Items.Value
	if inner.Nullable {
		t.Error("[[Int]!] inner list marked nullable")
	}
	if !inner.Items.Value.Nullable {
		t.Error("[[Int]!] innermost ints not nullable")
	}
}

// Field.Nullable (a required field that may be null) renders nullable,
// for scalars, refs and lists.
func TestRenderOpenAPI_FieldNullable(t *testing.T) {
	tr := func(f *Field) *Field { f.Required = true; f.Nullable = true; return f }
	svc := &Service{Namespace: "n", Version: "v1", Types: map[string]*Type{
		"Item": {Name: "Item", TypeKind: TypeObject},
		"Box": {Name: "Box", TypeKind: TypeObject, Fields: []*Field{
			tr(&Field{Name: "n", Type: TypeRef{Builtin: ScalarInt32}}),
			tr(&Field{Name: "r", Type: TypeRef{Named: "Item"}}),
			tr(&Field{Name: "l", Type: TypeRef{Builtin: ScalarInt32}, Repeated: true, ItemRequired: true}),
			{Name: "s", Type: TypeRef{Builtin: ScalarInt32}, Required: true},
		}},
	}}
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatal(err)
	}
	p := doc.Components.Schemas["Box"].Value.Properties
	if !p["n"].Value.Nullable {
		t.Error("n not nullable")
	}
	if v := p["r"].Value; !v.Nullable || len(v.AllOf) != 1 {
		t.Error("r not a nullable allOf wrapper")
	}
	if !p["l"].Value.Nullable || p["l"].Value.Items.Value.Nullable {
		t.Error("l: list nullable, items not")
	}
	if p["s"].Value.Nullable {
		t.Error("s marked nullable")
	}
}
