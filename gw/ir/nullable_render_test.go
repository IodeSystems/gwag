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
