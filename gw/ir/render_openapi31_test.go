package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
)

// openapi30Spec uses every 3.0 schema form the 3.1 conversion rewrites.
const openapi30Spec = `{
  "openapi": "3.0.3",
  "info": {"title": "shop", "version": "1.0.0"},
  "paths": {
    "/items/{id}": {
      "get": {
        "operationId": "getItem",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 0, "exclusiveMinimum": true, "nullable": true}}
        ],
        "responses": {
          "200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Item": {
        "type": "object",
        "required": ["id", "note"],
        "properties": {
          "id": {"type": "string", "example": "abc"},
          "note": {"type": "string", "nullable": true},
          "price": {"type": "number", "minimum": 0, "exclusiveMinimum": true, "maximum": 100, "exclusiveMaximum": false},
          "color": {"type": "string", "enum": ["red", "blue"], "nullable": true},
          "tags": {"type": "array", "items": {"type": "string", "nullable": true}}
        }
      }
    }
  }
}`

// openapi30RefSpec adds the 3.0 nullable-$ref idioms (allOf or a
// one-branch oneOf, + nullable).
const openapi30RefSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "shop", "version": "1.0.0"},
  "paths": {},
  "components": {
    "schemas": {
      "Item": {"type": "object", "properties": {"id": {"type": "string"}}},
      "Box": {
        "type": "object",
        "properties": {
          "item": {"allOf": [{"$ref": "#/components/schemas/Item"}], "nullable": true},
          "either": {"oneOf": [{"$ref": "#/components/schemas/Item"}], "nullable": true}
        }
      }
    }
  }
}`

func loadSpec(t *testing.T, raw []byte) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, raw)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("validate %s: %v\n%s", doc.OpenAPI, err, raw)
	}
	return doc
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func greeterService() *Service {
	svc := IngestProto(greeterv1.File_greeter_proto)[0]
	svc.Namespace = "greeter"
	svc.Version = "v1"
	return svc
}

// The zero options must give exactly what RenderOpenAPI gives today.
func TestRenderOpenAPIWith_DefaultUnchanged(t *testing.T) {
	synth := IngestOpenAPI(loadSpec(t, []byte(openapi30Spec)))
	synth.Origin = nil
	synth.Namespace, synth.Version = "shop", "v1"
	for name, svc := range map[string]*Service{
		"proto":  greeterService(),
		"origin": IngestOpenAPI(loadSpec(t, []byte(openapi30Spec))),
		"synth":  synth,
	} {
		t.Run(name, func(t *testing.T) {
			want, err := RenderOpenAPI(svc)
			if err != nil {
				t.Fatal(err)
			}
			got, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(mustJSON(t, want), mustJSON(t, got)) {
				t.Fatalf("default render changed:\nwant %s\ngot  %s", mustJSON(t, want), mustJSON(t, got))
			}
			if name != "origin" && got.OpenAPI != "3.0.0" {
				t.Fatalf("openapi = %q, want 3.0.0", got.OpenAPI)
			}
			loadSpec(t, mustJSON(t, got))
		})
	}
}

func TestRenderOpenAPIWith_31(t *testing.T) {
	doc := loadSpec(t, []byte(openapi30Spec))
	before := mustJSON(t, doc)
	svc := IngestOpenAPI(doc)

	out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: "3.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, mustJSON(t, doc)) {
		t.Fatal("3.1 render modified the origin spec")
	}
	raw := mustJSON(t, out)
	loaded := loadSpec(t, raw)
	if !loaded.IsOpenAPI31OrLater() || out.OpenAPI != OpenAPI31Version {
		t.Fatalf("openapi = %q, want %s", out.OpenAPI, OpenAPI31Version)
	}
	for _, bad := range []string{`"nullable"`, `"example"`, `"exclusiveMinimum":true`, `"exclusiveMaximum":false`} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("3.1 output contains %s:\n%s", bad, raw)
		}
	}

	props := out.Components.Schemas["Item"].Value.Properties
	typeOf := func(s *openapi3.Schema) []string { return []string(*s.Type) }
	if got := typeOf(props["note"].Value); fmt.Sprint(got) != "[string null]" {
		t.Errorf("note type = %v", got)
	}
	if got := props["id"].Value.Examples; len(got) != 1 || got[0] != "abc" {
		t.Errorf("id examples = %v", got)
	}
	price := props["price"].Value
	if price.Min != nil || price.ExclusiveMin.Value == nil || *price.ExclusiveMin.Value != 0 {
		t.Errorf("price lower bound = min %v exclusiveMin %+v", price.Min, price.ExclusiveMin)
	}
	if price.Max == nil || *price.Max != 100 || price.ExclusiveMax.IsSet() {
		t.Errorf("price upper bound = max %v exclusiveMax %+v", price.Max, price.ExclusiveMax)
	}
	color := props["color"].Value
	if fmt.Sprint(typeOf(color)) != "[string null]" || len(color.Enum) != 3 || color.Enum[2] != nil {
		t.Errorf("color = type %v enum %v", typeOf(color), color.Enum)
	}
	if got := typeOf(props["tags"].Value.Items.Value); fmt.Sprint(got) != "[string null]" {
		t.Errorf("tags item type = %v", got)
	}
	limit := out.Paths.Value("/items/{id}").Get.Parameters[1].Value.Schema.Value
	if fmt.Sprint(typeOf(limit)) != "[integer null]" || limit.ExclusiveMin.Value == nil {
		t.Errorf("limit = type %v exclusiveMin %+v", typeOf(limit), limit.ExclusiveMin)
	}
}

func TestRenderOpenAPIWith_31NullableRef(t *testing.T) {
	svc := IngestOpenAPI(loadSpec(t, []byte(openapi30RefSpec)))
	out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: "3.1"})
	if err != nil {
		t.Fatal(err)
	}
	loadSpec(t, mustJSON(t, out))
	props := out.Components.Schemas["Box"].Value.Properties
	for _, name := range []string{"item", "either"} {
		s := props[name].Value
		if len(s.AllOf) != 0 || len(s.OneOf) != 2 ||
			s.OneOf[0].Ref != "#/components/schemas/Item" ||
			!s.OneOf[1].Value.Type.Is(openapi3.TypeNull) {
			t.Errorf("%s = %s", name, mustJSON(t, s))
		}
	}
}

// Both nullable-$ref spellings ingest as a nullable Item field, and the
// 3.1 form (oneOf with a null branch) ingests back the same.
func TestIngestOpenAPI_NullableRef(t *testing.T) {
	svc := IngestOpenAPI(loadSpec(t, []byte(openapi30RefSpec)))
	out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: "3.1"})
	if err != nil {
		t.Fatal(err)
	}
	for label, s := range map[string]*Service{"3.0": svc, "3.1": IngestOpenAPI(loadSpec(t, mustJSON(t, out)))} {
		if n := len(s.Types["Box"].Fields); n != 2 {
			t.Fatalf("%s: Box has %d fields", label, n)
		}
		for _, f := range s.Types["Box"].Fields {
			if f.Type.Named != "Item" || !f.Nullable {
				t.Errorf("%s %s: type %s nullable %v", label, f.Name, typeRefString(f.Type), f.Nullable)
			}
		}
	}
}

func TestRenderOpenAPIWith_31Proto(t *testing.T) {
	svc := greeterService()
	out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: "3.1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.OpenAPI != OpenAPI31Version {
		t.Fatalf("openapi = %q", out.OpenAPI)
	}
	loadSpec(t, mustJSON(t, out))
}

// summarizeIR lists the operations and types of svc in a form that
// ignores Origin and other per-ingest pointers.
func summarizeIR(svc *Service) string {
	var lines []string
	for _, op := range svc.FlatOperations() {
		args := []string{}
		for _, a := range op.Args {
			args = append(args, fmt.Sprintf("%s:%s:%v:%s", a.Name, typeRefString(a.Type), a.Required, a.OpenAPILocation))
		}
		out := ""
		if op.Output != nil {
			out = typeRefString(*op.Output)
		}
		lines = append(lines, fmt.Sprintf("op %v %s %s %s(%s) %s", op.Kind, op.HTTPMethod, op.HTTPPath, op.Name, strings.Join(args, ","), out))
	}
	for name, ty := range svc.Types {
		fields := []string{}
		for _, f := range ty.Fields {
			fields = append(fields, fmt.Sprintf("%s:%s:rep=%v:req=%v:null=%v:item=%v", f.Name, typeRefString(f.Type), f.Repeated, f.Required, f.Nullable, f.ItemRequired))
		}
		sort.Strings(fields)
		enums := []string{}
		for _, e := range ty.Enum {
			enums = append(enums, e.Name)
		}
		lines = append(lines, fmt.Sprintf("type %s %v {%s} enum%v", name, ty.TypeKind, strings.Join(fields, " "), enums))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func typeRefString(r TypeRef) string {
	switch {
	case r.IsMap():
		return "map[" + typeRefString(r.Map.ValueType) + "]"
	case r.IsNamed():
		return r.Named
	}
	return fmt.Sprintf("%v%s", r.Builtin, r.ScalarName)
}

// A 3.1 render ingests back to the same operations and types as the
// 3.0 one.
func TestRenderOpenAPIWith_31RoundTrip(t *testing.T) {
	synth := IngestOpenAPI(loadSpec(t, []byte(openapi30Spec)))
	synth.Origin = nil
	synth.Namespace, synth.Version = "shop", "v1"
	for name, svc := range map[string]*Service{
		"proto":  greeterService(),
		"origin": IngestOpenAPI(loadSpec(t, []byte(openapi30Spec))),
		"synth":  synth,
	} {
		t.Run(name, func(t *testing.T) {
			doc30, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{})
			if err != nil {
				t.Fatal(err)
			}
			doc31, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: "3.1"})
			if err != nil {
				t.Fatal(err)
			}
			want := summarizeIR(IngestOpenAPI(loadSpec(t, mustJSON(t, doc30))))
			got := summarizeIR(IngestOpenAPI(loadSpec(t, mustJSON(t, doc31))))
			if want != got {
				t.Fatalf("3.1 round trip differs\n3.0:\n%s\n3.1:\n%s", want, got)
			}
			if !strings.Contains(got, "op ") {
				t.Fatalf("no operations ingested:\n%s", got)
			}
		})
	}
}
