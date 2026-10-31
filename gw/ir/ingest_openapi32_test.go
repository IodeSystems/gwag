package ir

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPI 3.2 adds the QUERY method as a Path Item field and custom
// methods under additionalOperations. HEAD / OPTIONS are not ingested
// and must be reported by UnsupportedOpenAPIOperations.
const openapi32Spec = `{
  "openapi": "3.2.0",
  "info": {"title": "q", "version": "1.0.0"},
  "paths": {
    "/things/search": {
      "get": {
        "operationId": "listThings",
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
      },
      "query": {
        "operationId": "searchThings",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {
            "type": "object", "properties": {"name": {"type": "string"}}
          }}}
        },
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
      },
      "head": {
        "operationId": "headThings",
        "responses": {"200": {"description": "ok"}}
      },
      "additionalOperations": {
        "PURGE": {
          "operationId": "purgeThings",
          "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
        },
        "LINK": {
          "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}
        }
      }
    },
    "/other": {
      "options": {"responses": {"200": {"description": "ok"}}}
    }
  }
}`

func loadOpenAPI32(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapi32Spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return doc
}

func TestOpenAPIIngest_32Methods(t *testing.T) {
	svc := IngestOpenAPI(loadOpenAPI32(t))

	type want struct {
		method string
		kind   OpKind
	}
	wants := []struct {
		name string
		want
	}{
		{"listThings", want{"GET", OpQuery}},
		{"searchThings", want{"QUERY", OpQuery}},
		{"LINK/things/search", want{"LINK", OpMutation}},
		{"purgeThings", want{"PURGE", OpMutation}},
	}
	if len(svc.Operations) != len(wants) {
		var names []string
		for _, o := range svc.Operations {
			names = append(names, o.Name)
		}
		t.Fatalf("ops=%v, want %d", names, len(wants))
	}
	for i, w := range wants {
		op := svc.Operations[i]
		if op.Name != w.name || op.HTTPMethod != w.method || op.Kind != w.kind || op.HTTPPath != "/things/search" {
			t.Errorf("op[%d]=%s %s kind=%v path=%s, want %s %s kind=%v", i, op.Name, op.HTTPMethod, op.Kind, op.HTTPPath, w.name, w.method, w.kind)
		}
		if op.Origin == nil {
			t.Errorf("%s: Origin not set", op.Name)
		}
	}
	search := svc.Operations[1]
	var body *Arg
	for _, a := range search.Args {
		if a.Name == "body" {
			body = a
		}
	}
	if body == nil || body.OpenAPILocation != "body" || !body.Required {
		t.Fatalf("searchThings body arg = %+v, want required body", body)
	}

	got := UnsupportedOpenAPIOperations(loadOpenAPI32(t))
	if strings.Join(got, ",") != "HEAD /things/search,OPTIONS /other" {
		t.Fatalf("UnsupportedOpenAPIOperations=%v", got)
	}
}

// A cross-kind render into a 3.0 document has no Path Item field for
// QUERY or custom methods; they go under x-additional-operations.
// Proto render must accept the same ops.
func TestOpenAPIRender_32MethodsCrossKind(t *testing.T) {
	svc := IngestOpenAPI(loadOpenAPI32(t))
	svc.Namespace, svc.Version = "q", "v1"
	PopulateSchemaIDs(svc)

	if _, err := RenderProtoFiles([]*Service{svc}); err != nil {
		t.Fatalf("RenderProtoFiles: %v", err)
	}

	svc.OriginKind, svc.Origin = KindGraphQL, nil
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	item := doc.Paths.Value("/things/search")
	if item == nil || item.Get == nil {
		t.Fatalf("GET /things/search missing")
	}
	if item.Query != nil || len(item.AdditionalOperations) != 0 {
		t.Fatalf("3.0 output carries 3.2 fields")
	}
	extra, _ := item.Extensions[xAdditionalOperations].(map[string]*openapi3.Operation)
	for _, m := range []string{"QUERY", "PURGE", "LINK"} {
		if extra[m] == nil {
			t.Errorf("%s missing from %s: %v", m, xAdditionalOperations, extra)
		}
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("rendered 3.0 doc invalid: %v", err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"x-additional-operations":{`) {
		t.Fatalf("marshalled doc lacks extension: %s", b)
	}
}
