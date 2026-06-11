package ir

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// nullableSpec: a required field that is also `nullable:true` (must be present,
// value may be null) alongside a plain required field. GraphQL has one
// nullability axis, so the rule is: non-null (`T!`) iff required AND not
// nullable. Before the fix, ingest dropped `nullable` and both rendered `T!`,
// so returning null for the nullable field errored at query time.
const nullableSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "nullable", "version": "1.0.0", "description": "nullable mapping"},
  "paths": {
    "/thing": {
      "get": {
        "operationId": "getThing",
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Thing": {
        "type": "object",
        "required": ["id", "note"],
        "properties": {
          "id":   {"type": "string"},
          "note": {"type": "string", "nullable": true}
        }
      }
    }
  }
}`

func TestOpenAPINullableRequiredRendersNullable(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(nullableSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)
	// ingest leaves namespacing identity for the caller (gat sets these from
	// the huma config); the runtime schema folds operations under Namespace.
	svc.Namespace = "thing"
	svc.Version = "v1"
	svc.ServiceName = "ThingService"

	// IR: both required; only `note` is nullable.
	thing := svc.Types["Thing"]
	if thing == nil {
		t.Fatal("Thing type missing")
	}
	byName := map[string]*Field{}
	for _, f := range thing.Fields {
		byName[f.Name] = f
	}
	if f := byName["id"]; f == nil || !f.Required || f.Nullable {
		t.Errorf("id: want required & non-nullable, got %#v", f)
	}
	if f := byName["note"]; f == nil || !f.Required || !f.Nullable {
		t.Errorf("note: want required & nullable, got %#v", f)
	}

	// Rendered GraphQL SDL: id non-null, note nullable.
	schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	sdl := PrintSchemaSDL(schema)
	if !strings.Contains(sdl, "id: String!") {
		t.Errorf("expected `id: String!` (required, non-nullable) in SDL:\n%s", sdl)
	}
	if strings.Contains(sdl, "note: String!") {
		t.Errorf("`note` is nullable but rendered non-null (`String!`) in SDL:\n%s", sdl)
	}
	if !strings.Contains(sdl, "note: String\n") {
		t.Errorf("expected `note: String` (nullable) in SDL:\n%s", sdl)
	}
}

// arrayItemSpec: a $ref-item array and a scalar-item array (both non-null
// elements → `[T!]`), plus a nullable-element array (`items.nullable:true` →
// `[T]`). Before the fix, ingest set Repeated but never ItemRequired, so every
// list rendered `[T]` (nullable elements) regardless of the item schema.
const arrayItemSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "arr", "version": "1.0.0", "description": "array item nullability"},
  "paths": {
    "/box": {
      "get": {
        "operationId": "getBox",
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Box"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}}},
      "Box": {
        "type": "object",
        "required": ["items", "tags", "maybeTags"],
        "properties": {
          "items":     {"type": "array", "items": {"$ref": "#/components/schemas/Item"}},
          "tags":      {"type": "array", "items": {"type": "string"}},
          "maybeTags": {"type": "array", "items": {"type": "string", "nullable": true}}
        }
      }
    }
  }
}`

func TestOpenAPIArrayItemNonNull(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(arrayItemSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Namespace = "box"
	svc.Version = "v1"
	svc.ServiceName = "BoxService"

	box := svc.Types["Box"]
	if box == nil {
		t.Fatal("Box type missing")
	}
	byName := map[string]*Field{}
	for _, f := range box.Fields {
		byName[f.Name] = f
	}
	// $ref item and non-nullable scalar item → ItemRequired; nullable item → not.
	if f := byName["items"]; f == nil || !f.Repeated || !f.ItemRequired {
		t.Errorf("items: want repeated & item-required, got %#v", f)
	}
	if f := byName["tags"]; f == nil || !f.Repeated || !f.ItemRequired {
		t.Errorf("tags: want repeated & item-required, got %#v", f)
	}
	if f := byName["maybeTags"]; f == nil || !f.Repeated || f.ItemRequired {
		t.Errorf("maybeTags: want repeated & NOT item-required, got %#v", f)
	}

	schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	sdl := PrintSchemaSDL(schema)
	// Required + ItemRequired → `[T!]!`. The element `!` is the fix; the outer `!`
	// follows from the field being in `required`.
	if !strings.Contains(sdl, "items: [box_Item!]!") {
		t.Errorf("expected `items: [box_Item!]!` (non-null elements) in SDL:\n%s", sdl)
	}
	if !strings.Contains(sdl, "tags: [String!]!") {
		t.Errorf("expected `tags: [String!]!` (non-null elements) in SDL:\n%s", sdl)
	}
	// nullable item → no inner `!` (list still non-null since required).
	if !strings.Contains(sdl, "maybeTags: [String]!") {
		t.Errorf("expected `maybeTags: [String]!` (nullable elements) in SDL:\n%s", sdl)
	}
}

// nullableArraySpec: required arrays typed `["array","null"]` — exactly what Huma
// emits for a Go `[]T` (a nil slice can serialize to null). Default rendering
// keeps the list nullable (`[T!]`); NonNullRequiredLists renders it non-null
// (`[T!]!`), relied upon together with a dispatch that coerces nil → `[]`.
const nullableArraySpec = `{
  "openapi": "3.0.0",
  "info": {"title": "na", "version": "1.0.0", "description": "nullable array"},
  "paths": {"/box": {"get": {"operationId": "getBox", "responses": {"200": {"description": "ok",
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Box"}}}}}}}},
  "components": {"schemas": {
    "Item": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}}},
    "Box": {"type": "object", "required": ["items"],
      "properties": {"items": {"type": "array", "nullable": true, "items": {"$ref": "#/components/schemas/Item"}}}}
  }}
}`

func TestNonNullRequiredListsOption(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(nullableArraySpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	render := func(nonNull bool) string {
		svc := IngestOpenAPI(doc)
		svc.Namespace, svc.Version, svc.ServiceName = "box", "v1", "BoxService"
		schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{NonNullRequiredLists: nonNull})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return PrintSchemaSDL(schema)
	}
	// Default (off): nullable list, non-null elements.
	if sdl := render(false); !strings.Contains(sdl, "items: [box_Item!]\n") {
		t.Errorf("default: expected `items: [box_Item!]` (nullable list), got:\n%s", sdl)
	}
	// On: non-null list.
	if sdl := render(true); !strings.Contains(sdl, "items: [box_Item!]!") {
		t.Errorf("NonNullRequiredLists: expected `items: [box_Item!]!`, got:\n%s", sdl)
	}
}
