package ir

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const openapiSpecWithDocs = `{
  "openapi": "3.0.0",
  "info": {"title": "petstore", "version": "1.0.0"},
  "paths": {
    "/pets/{id}": {
      "get": {
        "operationId": "getPet",
        "summary": "Look up a pet by id.",
        "description": "Returns the full Pet record including tag and weight.",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "description": "Stable identifier issued at create time.",
            "schema": {"type": "string"}
          }
        ],
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
        "description": "A pet record persisted in the registry.",
        "required": ["id"],
        "properties": {
          "id": {"type": "string", "description": "Stable identifier."},
          "name": {"type": "string"}
        }
      }
    }
  }
}`

// TestOpenAPIIngest_DescriptionsLand pins that description / summary
// fields on the OpenAPI spec land in IR Description for ops, args,
// types, and fields. MCP search corpus depends on this for OpenAPI-
// source services.
func TestOpenAPIIngest_DescriptionsLand(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiSpecWithDocs))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)

	var getPet *Operation
	for _, op := range svc.Operations {
		if op.Name == "getPet" {
			getPet = op
			break
		}
	}
	if getPet == nil {
		t.Fatal("getPet missing")
	}
	if !strings.Contains(getPet.Description, "tag and weight") {
		t.Errorf("getPet.Description = %q, want operation description copied", getPet.Description)
	}
	var idArg *Arg
	for _, a := range getPet.Args {
		if a.Name == "id" {
			idArg = a
			break
		}
	}
	if idArg == nil {
		t.Fatal("getPet id arg missing")
	}
	if !strings.Contains(idArg.Description, "create time") {
		t.Errorf("getPet.id.Description = %q, want parameter description", idArg.Description)
	}

	pet, ok := svc.Types["Pet"]
	if !ok {
		t.Fatal("Pet type missing")
	}
	if !strings.Contains(pet.Description, "registry") {
		t.Errorf("Pet.Description = %q, want schema description", pet.Description)
	}
	var idField *Field
	for _, f := range pet.Fields {
		if f.Name == "id" {
			idField = f
			break
		}
	}
	if idField == nil {
		t.Fatal("Pet.id field missing")
	}
	if !strings.Contains(idField.Description, "Stable identifier") {
		t.Errorf("Pet.id.Description = %q, want field description", idField.Description)
	}
}
