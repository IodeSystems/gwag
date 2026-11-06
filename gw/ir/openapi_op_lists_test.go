package ir

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Cross-kind export of an op-level list: a list arg and a list output
// export as arrays. They used to export as their element schema.
func TestOpenAPIExport_OpLevelLists(t *testing.T) {
	const data = `{"__schema": {
  "queryType": {"name": "Query"},
  "types": [
    {"kind": "OBJECT", "name": "Query", "fields": [{
      "name": "users",
      "args": [{"name": "ids", "type": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}}],
      "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "User"}}}}
    }]},
    {"kind": "OBJECT", "name": "User", "fields": [{"name": "id", "args": [], "type": {"kind": "SCALAR", "name": "ID"}}]}
  ]
}}`
	svc, err := IngestGraphQL([]byte(data))
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}
	svc.Namespace, svc.Version = "u", "v1"
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	var op *openapi3.Operation
	for _, item := range doc.Paths.Map() {
		for _, o := range item.Operations() {
			if o.OperationID == "users" {
				op = o
			}
		}
	}
	if op == nil {
		t.Fatal("users op missing")
	}
	isArray := func(s *openapi3.SchemaRef) bool {
		return s != nil && s.Value != nil && s.Value.Type != nil && s.Value.Type.Is("array")
	}
	resp := op.Responses.Status(200)
	if resp == nil || !isArray(resp.Value.Content["application/json"].Schema) {
		t.Errorf("response is not an array: %+v", resp)
	} else if items := resp.Value.Content["application/json"].Schema.Value.Items; items == nil || items.Ref != "#/components/schemas/User" {
		t.Errorf("response items = %+v, want $ref User", items)
	}
	var ids *openapi3.Parameter
	for _, p := range op.Parameters {
		if p.Value.Name == "ids" {
			ids = p.Value
		}
	}
	if ids == nil || !isArray(ids.Schema) {
		t.Errorf("ids param is not an array: %+v", ids)
	}
}
