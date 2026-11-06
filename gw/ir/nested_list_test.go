package ir

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A list whose element is a list (OpenAPI array of array, GraphQL
// `[[T]]`) used to flatten to `[T]`: the IR had one Repeated flag per
// slot and the inner list was dropped.

const nestedListSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "geo", "version": "1.0.0"},
  "paths": {
    "/regions": {
      "post": {
        "operationId": "putRegion",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}}}
      }
    }
  },
  "components": {"schemas": {
    "Point": {"type": "object", "required": ["lon"], "properties": {"lon": {"type": "number"}}},
    "Region": {
      "type": "object",
      "required": ["points", "cubes", "rings"],
      "properties": {
        "points": {"type": "array", "items": {"type": "array", "items": {"type": "number"}}},
        "cubes":  {"type": "array", "items": {"type": "array", "nullable": true, "items": {"type": "array", "items": {"type": "integer"}}}},
        "rings":  {"type": "array", "items": {"type": "array", "items": {"$ref": "#/components/schemas/Point"}}}
      }
    }
  }}
}`

func nestedListService(t *testing.T) *Service {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(nestedListSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Namespace = "geo"
	svc.Version = "v1"
	svc.ServiceName = "GeoService"
	return svc
}

func sdlOf(t *testing.T, svc *Service) string {
	t.Helper()
	schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return PrintSchemaSDL(schema)
}

func TestNestedList_OpenAPIIngest(t *testing.T) {
	sdl := sdlOf(t, nestedListService(t))
	for _, want := range []string{
		"points: [[Float!]!]!",
		"cubes: [[[Int!]!]]!",
		"rings: [[geo_Point!]!]!",
		"points: [[Float!]!]!", // input side
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}
}

func TestNestedList_GraphQLIngest(t *testing.T) {
	const data = `{"__schema": {
  "queryType": {"name": "Query"},
  "types": [{"kind": "OBJECT", "name": "Query", "fields": [{
    "name": "grid",
    "args": [{"name": "seed", "type": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "SCALAR", "name": "Int"}}}}}],
    "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}}}}
  }]}]
}}`
	svc, err := IngestGraphQL([]byte(data))
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}
	svc.Namespace = "g"
	svc.Version = "v1"
	sdl := sdlOf(t, svc)
	if !strings.Contains(sdl, "grid(seed: [[Int]!]): [[Int!]]!") {
		t.Errorf("SDL lost the nesting:\n%s", sdl)
	}
}

func TestNestedList_OpenAPIExport(t *testing.T) {
	svc := nestedListService(t)
	// Drop the origin so the renderer synthesises the schema from IR.
	for _, op := range svc.Operations {
		op.Origin = nil
		op.OriginKind = KindUnset
	}
	for _, ty := range svc.Types {
		ty.Origin = nil
	}
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	region := doc.Components.Schemas["Region"]
	if region == nil || region.Value == nil {
		t.Fatalf("Region missing: %v", doc.Components.Schemas)
	}
	cubes := region.Value.Properties["cubes"].Value
	depth := 0
	for s := cubes; s != nil && s.Type != nil && s.Type.Is("array"); depth++ {
		s = s.Items.Value
	}
	if depth != 3 {
		t.Errorf("cubes array depth = %d, want 3", depth)
	}
}

func TestNestedList_ProtoExport(t *testing.T) {
	fds, err := RenderProtoFiles([]*Service{nestedListService(t)})
	if err != nil {
		t.Fatalf("RenderProtoFiles: %v", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("descriptor does not resolve: %v", err)
	}
	var found bool
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		msgs := fd.Messages()
		for i := 0; i < msgs.Len(); i++ {
			if msgs.Get(i).Name() != "Region" {
				continue
			}
			found = true
			cubes := msgs.Get(i).Fields().ByName("cubes")
			// cubes: repeated CubesRow; CubesRow.value: repeated ValueRow;
			// ValueRow.value: repeated int.
			row := cubes.Message()
			if !cubes.IsList() || row == nil || row.Name() != "CubesRow" {
				t.Fatalf("cubes = %v", cubes)
			}
			inner := row.Fields().ByName("value")
			if !inner.IsList() || inner.Message() == nil {
				t.Fatalf("CubesRow.value = %v", inner)
			}
			leaf := inner.Message().Fields().ByName("value")
			if !leaf.IsList() || leaf.Message() != nil {
				t.Fatalf("leaf = %v", leaf)
			}
		}
		return true
	})
	if !found {
		t.Fatal("Region message missing")
	}
}

func TestNestedList_MCPIngest(t *testing.T) {
	svc, err := IngestMCP([]byte(`{"tools":[{"name":"draw","inputSchema":{"type":"object",
	  "properties":{"ring":{"type":"array","items":{"type":"array","items":{"type":"number"}}}}}}]}`))
	if err != nil {
		t.Fatalf("IngestMCP: %v", err)
	}
	a := svc.Operations[0].Args[0]
	if !a.Repeated || !a.Type.IsList() || a.Type.List.Elem.Builtin != ScalarDouble {
		t.Errorf("ring arg = %+v", a)
	}
}
