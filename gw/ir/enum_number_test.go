package ir

import (
	"encoding/json"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Regression guard for the enum-number bug: OpenAPI and MCP ingest used to
// append EnumValues without setting Number, leaving every value at 0. The
// proto renderer reads EnumValue.Number verbatim, so the synthesised proto
// enum had N values all numbered 0 — which fails FileDescriptorSet
// construction with "conflicting non-aliased values on number 0" and kept
// gat servers from starting (any string enum field, e.g. a status code, hit
// it). The contract (EnumValue.Number doc): non-proto origins number values
// 0-based in declaration order.
func assertEnumNumbersDistinct(t *testing.T, svc *Service) {
	t.Helper()
	enumsChecked := 0
	for name, ty := range svc.Types {
		if ty.TypeKind != TypeEnum {
			continue
		}
		enumsChecked++
		seen := map[int32]string{}
		for i, ev := range ty.Enum {
			if prev, dup := seen[ev.Number]; dup {
				t.Errorf("enum %s: %q and %q both have Number %d (proto3 requires distinct non-aliased numbers)",
					name, prev, ev.Name, ev.Number)
			}
			seen[ev.Number] = ev.Name
			if ev.Number != int32(i) {
				t.Errorf("enum %s value %q: Number = %d, want %d (0-based, declaration order)",
					name, ev.Name, ev.Number, i)
			}
		}
	}
	if enumsChecked == 0 {
		t.Fatal("no enum types ingested — test spec is wrong")
	}
}

const enumNumberSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "enums", "version": "1.0.0", "description": "enum number regression"},
  "paths": {
    "/thing": {
      "get": {
        "operationId": "getThing",
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {
              "type": "object",
              "properties": {
                "color": {"$ref": "#/components/schemas/Color"},
                "status": {"type": "string", "enum": ["OK", "PENDING", "FAILED"]}
              }
            }}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Color": {"type": "string", "enum": ["RED", "GREEN", "BLUE"]}
    }
  }
}`

// Covers both OpenAPI ingest sites: the named-component enum (Color) and the
// inline property enum (status).
func TestOpenAPIEnumValuesGetDistinctNumbers(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(enumNumberSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	assertEnumNumbersDistinct(t, IngestOpenAPI(doc))
}

// Covers the MCP ingest site: an enum-typed tool input property.
func TestMCPEnumValuesGetDistinctNumbers(t *testing.T) {
	data := json.RawMessage(`{"tools":[
	  {"name":"set_mode","description":"set mode","inputSchema":{"type":"object",
	    "properties":{"mode":{"type":"string","enum":["AUTO","MANUAL","OFF"]}},
	    "required":["mode"]}}
	]}`)
	svc, err := IngestMCP(data)
	if err != nil {
		t.Fatalf("IngestMCP: %v", err)
	}
	assertEnumNumbersDistinct(t, svc)
}
