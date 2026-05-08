package ir

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// minimal OpenAPI spec with one path, one schema component, and a
// query param + body so the ingest/render roundtrip exercises
// every Arg location case.
const openapiTestSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "petstore", "version": "1.0.0", "description": "demo"},
  "paths": {
    "/pets/{id}": {
      "get": {
        "operationId": "getPet",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "verbose", "in": "query", "schema": {"type": "boolean"}}
        ],
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
          }
        }
      },
      "post": {
        "operationId": "updatePet",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
        },
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
          }
        }
      }
    },
    "/pets": {
      "post": {
        "operationId": "createPet",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
        },
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["id", "name"],
        "properties": {
          "id": {"type": "string"},
          "name": {"type": "string"},
          "tag": {"type": "string"},
          "ageYears": {"type": "integer", "format": "int32"},
          "weightG": {"type": "integer", "format": "int64"}
        }
      }
    }
  }
}`

// loadOpenAPI parses the test spec into a kin-openapi document.
// Helper kept tiny so the test files stay focused on assertions.
func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiTestSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return doc
}

func TestOpenAPIIngest(t *testing.T) {
	doc := loadOpenAPI(t)
	svc := IngestOpenAPI(doc)

	if got := len(svc.Operations); got != 3 {
		t.Fatalf("Operations = %d, want 3 (getPet, updatePet, createPet)", got)
	}

	// Lookup each op + check the canonical fields look right.
	byName := map[string]*Operation{}
	for _, op := range svc.Operations {
		byName[op.Name] = op
	}
	get, ok := byName["getPet"]
	if !ok {
		t.Fatal("getPet missing")
	}
	if get.HTTPMethod != "GET" || get.HTTPPath != "/pets/{id}" {
		t.Errorf("getPet path/method = %s %s, want GET /pets/{id}", get.HTTPMethod, get.HTTPPath)
	}
	if get.Kind != OpQuery {
		t.Errorf("getPet Kind = %v, want OpQuery", get.Kind)
	}
	if got := len(get.Args); got != 2 {
		t.Errorf("getPet Args = %d, want 2", got)
	}

	post, ok := byName["createPet"]
	if !ok {
		t.Fatal("createPet missing")
	}
	if post.Kind != OpMutation {
		t.Errorf("createPet Kind = %v, want OpMutation", post.Kind)
	}
	// Body landed as an Arg with location "body".
	bodyFound := false
	for _, a := range post.Args {
		if a.OpenAPILocation == "body" {
			bodyFound = true
			if a.Type.Named != "Pet" {
				t.Errorf("createPet body Type.Named = %q, want Pet", a.Type.Named)
			}
		}
	}
	if !bodyFound {
		t.Errorf("createPet missing body arg; got %#v", post.Args)
	}

	// Components → Types.
	pet, ok := svc.Types["Pet"]
	if !ok {
		t.Fatal("Pet missing from Types")
	}
	if pet.TypeKind != TypeObject {
		t.Errorf("Pet TypeKind = %v, want TypeObject", pet.TypeKind)
	}
	if got := len(pet.Fields); got != 5 {
		t.Errorf("Pet has %d fields, want 5", got)
	}
	// Required fields tracked.
	requiredCount := 0
	for _, f := range pet.Fields {
		if f.Required {
			requiredCount++
		}
		if f.Name == "weightG" {
			if f.Type.Builtin != ScalarInt64 {
				t.Errorf("weightG Type.Builtin = %v, want ScalarInt64", f.Type.Builtin)
			}
			if f.Format != "int64" {
				t.Errorf("weightG Format = %q, want int64", f.Format)
			}
		}
	}
	if requiredCount != 2 {
		t.Errorf("required field count = %d, want 2", requiredCount)
	}
}

func TestOpenAPIRoundtripOriginShortcut(t *testing.T) {
	doc := loadOpenAPI(t)
	svc := IngestOpenAPI(doc)

	// Same-kind shortcut: render returns the captured *openapi3.T.
	out, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	if out != doc {
		t.Errorf("expected same-kind render to return the captured *openapi3.T verbatim")
	}
}

func TestOpenAPIRoundtripSynthesis(t *testing.T) {
	// Force synthesis by clearing the Origin so the renderer goes
	// through the canonical-field path.
	doc := loadOpenAPI(t)
	svc := IngestOpenAPI(doc)
	svc.Origin = nil

	out, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	// We expect getPet under GET /pets/{id}, updatePet under POST
	// /pets/{id}, createPet under POST /pets — all together with
	// the Pet schema in components.
	if out.Paths == nil {
		t.Fatal("synthesized doc has no paths")
	}
	pi := out.Paths.Value("/pets/{id}")
	if pi == nil {
		t.Fatal("synthesized doc missing /pets/{id}")
	}
	if pi.Get == nil || pi.Get.OperationID != "getPet" {
		t.Errorf("synthesized GET op = %#v, want getPet", pi.Get)
	}
	if pi.Post == nil || pi.Post.OperationID != "updatePet" {
		t.Errorf("synthesized POST op = %#v, want updatePet", pi.Post)
	}
	createPath := out.Paths.Value("/pets")
	if createPath == nil || createPath.Post == nil || createPath.Post.OperationID != "createPet" {
		t.Errorf("synthesized POST /pets missing createPet; got %#v", createPath)
	}
	if _, ok := out.Components.Schemas["Pet"]; !ok {
		t.Errorf("synthesized doc missing components.schemas.Pet")
	}
}

// openapiUnionSpec exercises the oneOf ingest path: top-level union
// in components.schemas with $ref'd named variants + a discriminator.
// The IR captures variants by name; the spec discriminator survives
// in Origin (same-kind round-trip) but isn't on the canonical Type.
const openapiUnionSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "zoo", "version": "1.0.0"},
  "paths": {
    "/animal": {
      "get": {
        "operationId": "getAnimal",
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Animal"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Cat": {"type": "object", "properties": {"meow": {"type": "boolean"}}},
      "Dog": {"type": "object", "properties": {"bark": {"type": "boolean"}}},
      "Animal": {
        "oneOf": [
          {"$ref": "#/components/schemas/Cat"},
          {"$ref": "#/components/schemas/Dog"}
        ],
        "discriminator": {"propertyName": "kind"}
      }
    }
  }
}`

func TestOpenAPIIngest_OneOf(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiUnionSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}

	svc := IngestOpenAPI(doc)
	animal, ok := svc.Types["Animal"]
	if !ok {
		t.Fatal("Animal missing from Types")
	}
	if animal.TypeKind != TypeUnion {
		t.Fatalf("Animal TypeKind = %v, want TypeUnion", animal.TypeKind)
	}
	if got, want := animal.Variants, []string{"Cat", "Dog"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Animal Variants = %v, want %v", got, want)
	}
	if animal.DiscriminatorProperty != "kind" {
		t.Errorf("Animal DiscriminatorProperty = %q, want %q", animal.DiscriminatorProperty, "kind")
	}
}

// TestOpenAPIIngest_DiscriminatorMapping covers the mapping-shaped
// path: $ref-style mapping values get stripped to bare schema names
// so the canonical DiscriminatorMapping holds variant identifiers,
// not URIs.
func TestOpenAPIIngest_DiscriminatorMapping(t *testing.T) {
	const spec = `{
  "openapi": "3.0.0",
  "info": {"title": "zoo", "version": "1.0.0"},
  "paths": {},
  "components": {
    "schemas": {
      "Cat": {"type": "object", "properties": {"meow": {"type": "boolean"}}},
      "Dog": {"type": "object", "properties": {"bark": {"type": "boolean"}}},
      "Animal": {
        "oneOf": [
          {"$ref": "#/components/schemas/Cat"},
          {"$ref": "#/components/schemas/Dog"}
        ],
        "discriminator": {
          "propertyName": "kind",
          "mapping": {
            "feline": "#/components/schemas/Cat",
            "canine": "Dog"
          }
        }
      }
    }
  }
}`
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)
	animal := svc.Types["Animal"]
	if animal == nil {
		t.Fatal("Animal missing")
	}
	if got, want := animal.DiscriminatorMapping["feline"], "Cat"; got != want {
		t.Errorf("feline → %q, want %q", got, want)
	}
	if got, want := animal.DiscriminatorMapping["canine"], "Dog"; got != want {
		t.Errorf("canine → %q, want %q", got, want)
	}
}

// TestOpenAPIIngest_InlineOneOf covers the inline-union ingest path:
// a property whose schema is `{oneOf: [Foo, Bar]}` (not a top-level
// component) gets a synthesised "FooOrBar" TypeUnion in svc.Types
// and the property's Type points at it. Anonymous (non-$ref)
// variants still fall through to scalar — IR has no name story.
func TestOpenAPIIngest_InlineOneOf(t *testing.T) {
	const spec = `{
  "openapi": "3.0.0",
  "info": {"title": "zoo", "version": "1.0.0"},
  "paths": {},
  "components": {
    "schemas": {
      "Cat": {"type": "object", "properties": {"meow": {"type": "boolean"}}},
      "Dog": {"type": "object", "properties": {"bark": {"type": "boolean"}}},
      "Kennel": {
        "type": "object",
        "properties": {
          "occupant": {
            "oneOf": [
              {"$ref": "#/components/schemas/Cat"},
              {"$ref": "#/components/schemas/Dog"}
            ]
          }
        }
      }
    }
  }
}`
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)

	// Synthesised name registered in svc.Types.
	syn, ok := svc.Types["CatOrDog"]
	if !ok {
		keys := []string{}
		for k := range svc.Types {
			keys = append(keys, k)
		}
		t.Fatalf("missing synthesised CatOrDog; types = %v", keys)
	}
	if syn.TypeKind != TypeUnion {
		t.Errorf("CatOrDog kind = %v, want TypeUnion", syn.TypeKind)
	}
	if got, want := syn.Variants, []string{"Cat", "Dog"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("CatOrDog variants = %v, want %v", got, want)
	}

	// Field `occupant` on Kennel points at the synthesised name.
	kennel, ok := svc.Types["Kennel"]
	if !ok {
		t.Fatal("Kennel missing")
	}
	var occupant *Field
	for _, f := range kennel.Fields {
		if f.Name == "occupant" {
			occupant = f
		}
	}
	if occupant == nil {
		t.Fatal("Kennel.occupant missing")
	}
	if got, want := occupant.Type.Named, "CatOrDog"; got != want {
		t.Errorf("occupant.Type.Named = %q, want %q", got, want)
	}
}

// TestOpenAPIRoundtripSynthesis_OneOf exercises the cross-kind
// render: clear Origin so the renderer takes the synthesis path,
// then verify oneOf comes back out with $ref-shaped variants.
func TestOpenAPIRoundtripSynthesis_OneOf(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiUnionSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Origin = nil
	for _, t := range svc.Types {
		t.Origin = nil
	}

	out, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	animalRef, ok := out.Components.Schemas["Animal"]
	if !ok || animalRef.Value == nil {
		t.Fatal("synthesized doc missing components.schemas.Animal")
	}
	if got := len(animalRef.Value.OneOf); got != 2 {
		t.Fatalf("Animal OneOf len = %d, want 2", got)
	}
	for i, want := range []string{
		"#/components/schemas/Cat",
		"#/components/schemas/Dog",
	} {
		if got := animalRef.Value.OneOf[i].Ref; got != want {
			t.Errorf("OneOf[%d].Ref = %q, want %q", i, got, want)
		}
	}
	// Discriminator survives via the canonical fields, not Origin.
	if animalRef.Value.Discriminator == nil {
		t.Fatal("synthesized Animal missing discriminator")
	}
	if got := animalRef.Value.Discriminator.PropertyName; got != "kind" {
		t.Errorf("discriminator.propertyName = %q, want %q", got, "kind")
	}
}
