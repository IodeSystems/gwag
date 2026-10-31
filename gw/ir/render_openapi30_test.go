package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// openapi31Rich uses every 3.1 construct the 3.0 down-conversion
// rewrites or keeps aside.
const openapi31Rich = `{
  "openapi": "3.1.0",
  "jsonSchemaDialect": "https://spec.openapis.org/oas/3.1/dialect/base",
  "info": {"title": "shop", "version": "1.0.0", "summary": "a shop",
    "license": {"name": "Apache 2.0", "identifier": "Apache-2.0"}},
  "security": [{"bearer": []}, {"mtls": []}],
  "paths": {
    "/items/{id}": {
      "get": {
        "operationId": "getItem",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "limit", "in": "query", "schema": {"type": ["integer", "null"], "exclusiveMinimum": 0}},
          {"$ref": "#/components/parameters/Page", "description": "page, overridden"}
        ],
        "responses": {
          "200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}}
        }
      }
    },
    "/items": {
      "post": {
        "operationId": "createItem",
        "security": [{"mtls": []}],
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {
          "201": {"description": "made", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Pet"}}}}}
        }
      },
      "delete": {"operationId": "clearItems"}
    },
    "/ops": {"$ref": "#/components/pathItems/Ops", "summary": "ops here"}
  },
  "webhooks": {
    "itemCreated": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {"200": {"description": "ok"}}
      }
    }
  },
  "components": {
    "securitySchemes": {
      "bearer": {"type": "http", "scheme": "bearer"},
      "mtls": {"type": "mutualTLS"}
    },
    "parameters": {
      "Page": {"name": "page", "in": "query", "schema": {"type": "integer", "minimum": 1, "examples": [1]}}
    },
    "pathItems": {
      "Ops": {
        "get": {
          "operationId": "listOps",
          "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"type": "string"}}}}}}
        }
      }
    },
    "schemas": {
      "Item": {
        "type": "object",
        "$comment": "an item",
        "required": ["id", "note"],
        "$defs": {"Local": {"type": "object", "properties": {"x": {"type": "string"}}}},
        "properties": {
          "id": {"type": "string", "examples": ["a1", "b2"]},
          "sku": {"type": "string", "examples": ["s1"]},
          "note": {"type": ["string", "null"]},
          "count": {"type": "integer", "exclusiveMinimum": 0, "maximum": 100},
          "price": {"type": "number", "minimum": 5, "exclusiveMinimum": 1},
          "ratio": {"type": "number", "exclusiveMaximum": 1, "maximum": 2},
          "kind": {"const": "item"},
          "color": {"type": ["string", "null"], "enum": ["red", "blue", null]},
          "multi": {"type": ["string", "integer"]},
          "multiNull": {"type": ["string", "integer", "null"]},
          "onlyNull": {"type": "null"},
          "parent": {"oneOf": [{"$ref": "#/components/schemas/Item"}, {"type": "null"}]},
          "owner": {"$ref": "#/components/schemas/Owner", "description": "who owns it"},
          "blob": {"type": "string", "contentEncoding": "base64"},
          "image": {"type": "string", "contentMediaType": "image/png"},
          "jsonText": {"type": "string", "contentMediaType": "application/json"},
          "tuple": {"type": "array", "prefixItems": [{"type": "string"}, {"type": "integer"}], "items": false},
          "tags": {"type": "array", "items": {"type": "string"}, "contains": {"const": "x"}, "minContains": 1},
          "extra": {"type": "object", "patternProperties": {"^x-": {"type": "string"}},
            "propertyNames": {"pattern": "^[a-z]"}, "unevaluatedProperties": false},
          "cond": {"type": "object", "if": {"required": ["a"]}, "then": {"required": ["b"]}, "else": {"required": ["c"]}},
          "dep": {"type": "object", "dependentRequired": {"a": ["b"]}},
          "local": {"$ref": "#/components/schemas/Item/$defs/Local"},
          "anything": true
        }
      },
      "Owner": {"type": "object", "properties": {"name": {"type": "string"}}},
      "Cat": {"type": "object", "properties": {"kind": {"type": "string"}, "meow": {"type": "boolean"}}},
      "Dog": {"type": "object", "properties": {"kind": {"type": "string"}, "bark": {"type": "boolean"}}},
      "Pet": {"oneOf": [{"$ref": "#/components/schemas/Cat"}, {"$ref": "#/components/schemas/Dog"}],
        "discriminator": {"propertyName": "kind"}}
    }
  }
}`

// openapi32Spec uses the 3.2 members the down-conversion moves.
const openapi32Rich = `{
  "openapi": "3.2.0",
  "$self": "https://example.com/openapi.json",
  "info": {"title": "search", "version": "1.0.0"},
  "servers": [{"url": "https://example.com", "name": "prod"}],
  "tags": [{"name": "items", "summary": "Items", "kind": "nav"}, {"name": "sub", "parent": "items"}],
  "paths": {
    "/search": {
      "query": {
        "operationId": "search",
        "tags": ["items"],
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Filter"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Hit"}}}}}}
      },
      "additionalOperations": {
        "COPY": {
          "operationId": "copySearch",
          "parameters": [{"$ref": "#/components/parameters/Dest"}],
          "responses": {"204": {"summary": "copied"}}
        }
      }
    },
    "/hits": {
      "get": {
        "operationId": "listHits",
        "parameters": [
          {"name": "q", "in": "querystring", "content": {"application/x-www-form-urlencoded": {"schema": {"type": "object"}}}},
          {"name": "n", "in": "query", "schema": {"type": "integer"}}
        ],
        "responses": {"200": {"description": "ok", "content": {"application/json": {
          "schema": {"type": "array", "items": {"$ref": "#/components/schemas/Hit"}},
          "examples": {"one": {"dataValue": [{"id": "h1"}], "serializedValue": "[{\"id\":\"h1\"}]"}}
        }}}}
      }
    },
    "/events": {
      "get": {
        "operationId": "events",
        "responses": {"200": {"description": "stream", "content": {"text/event-stream": {"itemSchema": {
          "type": "object", "required": ["data"],
          "properties": {"event": {"type": "string"}, "data": {"type": "string", "contentMediaType": "application/json",
            "contentSchema": {"$ref": "#/components/schemas/Hit"}}}
        }}}}}
      }
    }
  },
  "components": {
    "parameters": {"Dest": {"name": "dest", "in": "header", "schema": {"type": "string"}}},
    "securitySchemes": {
      "oauth": {"type": "oauth2", "deprecated": true, "oauth2MetadataUrl": "https://example.com/.well-known/oauth-authorization-server",
        "flows": {"clientCredentials": {"tokenUrl": "https://example.com/token", "scopes": {}},
          "deviceAuthorization": {"deviceAuthorizationUrl": "https://example.com/device", "tokenUrl": "https://example.com/token", "scopes": {}}}}
    },
    "schemas": {
      "Filter": {"type": "object", "properties": {"q": {"type": "string"}}},
      "Hit": {"type": "object", "xml": {"nodeType": "element"}, "properties": {"id": {"type": "string"}, "score": {"type": ["number", "null"]}}},
      "Animal": {"oneOf": [{"$ref": "#/components/schemas/Hit"}, {"$ref": "#/components/schemas/Filter"}],
        "discriminator": {"propertyName": "kind", "defaultMapping": "#/components/schemas/Hit"}}
    }
  }
}`

// loadRaw loads a spec without validating it.
func loadRaw(t *testing.T, raw []byte) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, raw)
	}
	return doc
}

// jget returns the value at the slash-separated path in the JSON of v
// (list indexes as numbers), or nil.
func jget(t *testing.T, v any, path string) any {
	t.Helper()
	var cur any
	if err := json.Unmarshal(mustJSON(t, v), &cur); err != nil {
		t.Fatal(err)
	}
	for _, seg := range strings.Split(path, "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			cur = c[seg]
		case []any:
			var i int
			if _, err := fmt.Sscan(seg, &i); err != nil || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	return cur
}

func jstr(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

func downcast(t *testing.T, src, version string) (*openapi3.T, *Service) {
	t.Helper()
	doc := loadRaw(t, []byte(src))
	before := mustJSON(t, doc)
	svc := IngestOpenAPI(doc)
	out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: version})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, mustJSON(t, doc)) {
		t.Fatal("conversion modified the origin spec")
	}
	return out, svc
}

func TestDowncast31To30(t *testing.T) {
	// kin-openapi does not know components.pathItems, so the source
	// is loaded without Validate.
	out, _ := downcast(t, openapi31Rich, "3.0")
	if out.OpenAPI != OpenAPI30Version {
		t.Fatalf("openapi = %q", out.OpenAPI)
	}
	raw := mustJSON(t, out)
	loaded := loadSpec(t, raw)
	if !loaded.IsOpenAPI30() {
		t.Fatalf("not 3.0: %s", loaded.OpenAPI)
	}

	item := "components/schemas/Item/properties/"
	for path, want := range map[string]string{
		item + "note":                          `{"nullable":true,"type":"string"}`,
		item + "count":                         `{"exclusiveMinimum":true,"maximum":100,"minimum":0,"type":"integer"}`,
		item + "price":                         `{"minimum":5,"type":"number","x-gwag-oas31":{"exclusiveMinimum":1}}`,
		item + "ratio":                         `{"exclusiveMaximum":true,"maximum":1,"type":"number"}`,
		item + "kind":                          `{"enum":["item"]}`,
		item + "color":                         `{"enum":["red","blue",null],"nullable":true,"type":"string"}`,
		item + "multi":                         `{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
		item + "multiNull":                     `{"anyOf":[{"type":"string"},{"type":"integer"}],"nullable":true}`,
		item + "onlyNull":                      `{"enum":[null],"nullable":true}`,
		item + "parent":                        `{"allOf":[{"$ref":"#/components/schemas/Item"}],"nullable":true}`,
		item + "owner":                         `{"allOf":[{"$ref":"#/components/schemas/Owner"}],"description":"who owns it"}`,
		item + "id":                            `{"example":"a1","type":"string","x-gwag-oas31":{"examples":["a1","b2"]}}`,
		item + "sku":                           `{"example":"s1","type":"string"}`,
		item + "blob":                          `{"format":"byte","type":"string","x-gwag-oas31":{"contentEncoding":"base64"}}`,
		item + "image":                         `{"format":"binary","type":"string","x-gwag-oas31":{"contentMediaType":"image/png"}}`,
		item + "jsonText":                      `{"type":"string","x-gwag-oas31":{"contentMediaType":"application/json"}}`,
		item + "tuple":                         `{"items":{},"type":"array","x-gwag-oas31":{"items":false,"prefixItems":[{"type":"string"},{"type":"integer"}]}}`,
		item + "tags":                          `{"items":{"type":"string"},"type":"array","x-gwag-oas31":{"contains":{"const":"x"},"minContains":1}}`,
		item + "extra":                         `{"type":"object","x-gwag-oas31":{"patternProperties":{"^x-":{"type":"string"}},"propertyNames":{"pattern":"^[a-z]"},"unevaluatedProperties":false}}`,
		item + "cond":                          `{"type":"object","x-gwag-oas31":{"else":{"required":["c"]},"if":{"required":["a"]},"then":{"required":["b"]}}}`,
		item + "dep":                           `{"type":"object","x-gwag-oas31":{"dependentRequired":{"a":["b"]}}}`,
		item + "local":                         `{"$ref":"#/components/schemas/Local"}`,
		item + "anything":                      `{}`,
		"components/schemas/Item/x-gwag-oas31": `{"$comment":"an item"}`,
		"components/schemas/Local":             `{"properties":{"x":{"type":"string"}},"type":"object"}`,

		"paths/~1items~1{id}/get/parameters/1/schema": `{"exclusiveMinimum":true,"minimum":0,"nullable":true,"type":"integer"}`,
		"paths/~1items~1{id}/get/parameters/2":        `{"$ref":"#/components/parameters/Page","x-gwag-oas31":{"description":"page, overridden"}}`,
		"components/parameters/Page/schema":           `{"example":1,"minimum":1,"type":"integer"}`,
		"paths/~1items/delete/responses":              `{"default":{"description":""}}`,
		"paths/~1ops/summary":                         `"ops here"`,
		"paths/~1ops/get/operationId":                 `"listOps"`,
		"info/x-gwag-oas31":                           `{"summary":"a shop"}`,
		"info/license/x-gwag-oas31":                   `{"identifier":"Apache-2.0"}`,
		"x-webhooks/itemCreated/post/requestBody/content/application~1json/schema": `{"$ref":"#/components/schemas/Item"}`,
		"components/x-gwag-oas31/securitySchemes/mtls":                             `{"type":"mutualTLS"}`,
		"components/x-gwag-oas31/pathItems/Ops/get/operationId":                    `"listOps"`,
		"security":                        `[{"bearer":[]}]`,
		"x-gwag-oas31/security":           `[{"bearer":[]},{"mtls":[]}]`,
		"paths/~1items/post/security":     `null`,
		"paths/~1items/post/x-gwag-oas31": `{"security":[{"mtls":[]}]}`,
	} {
		if got := jstr(t, jget(t, out, path)); got != want {
			t.Errorf("%s:\n got %s\nwant %s", path, got, want)
		}
	}
	for _, gone := range []string{"webhooks", "jsonSchemaDialect", "components/pathItems", "paths/~1ops/$ref"} {
		if v := jget(t, out, gone); v != nil {
			t.Errorf("3.0 output has %s: %s", gone, jstr(t, v))
		}
	}
}

// irLines is summarizeIR with one line per field, so a difference
// names the field.
func irLines(svc *Service) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(summarizeIR(svc), "\n") {
		if !strings.HasPrefix(l, "type ") {
			m[l] = true
			continue
		}
		head, rest, _ := strings.Cut(l, " {")
		fields, enum, _ := strings.Cut(rest, "} ")
		m[head+" "+enum] = true
		name := strings.Fields(head)[1]
		for _, f := range strings.Fields(fields) {
			m["field "+name+"."+f] = true
		}
	}
	return m
}

// irDiff lists the IR lines that differ between a and b, "-" for a
// only and "+" for b only.
func irDiff(a, b *Service) []string {
	ma, mb := irLines(a), irLines(b)
	var out []string
	for l := range ma {
		if !mb[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range mb {
		if !ma[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	return out
}

func checkIRDiff(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("IR difference:\n got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The 3.0 document ingests to the same IR as the 3.1 source, except
// where the 3.0 form says more than gwag reads from the 3.1 one.
func TestDowncast31To30_IR(t *testing.T) {
	out, svc := downcast(t, openapi31Rich, "3.0")
	down := IngestOpenAPI(loadSpec(t, mustJSON(t, out)))
	checkIRDiff(t, irDiff(svc, down), []string{
		// (12 is ScalarUpload, 1 ScalarString.) contentMediaType
		// image/png ingests as String; its 3.0 form, format binary, as
		// Upload.
		"+ field Item.image:12:rep=false:req=false:null=false:item=false",
		// A $defs entry is not a component, so the 3.1 IR has a Named
		// Local with no Type behind it. Hoisted, it is one.
		"+ field Local.x:1:rep=false:req=false:null=false:item=false",
		"+ type Local 0 enum[]",
		"- field Item.image:1:rep=false:req=false:null=false:item=false",
	})
	for _, op := range []string{"getItem", "createItem", "clearItems", "listOps"} {
		if !strings.Contains(summarizeIR(down), " "+op+"(") {
			t.Errorf("op %s missing:\n%s", op, summarizeIR(down))
		}
	}
}

// jsonDiff lists the JSON pointers at which a and b differ.
func jsonDiff(t *testing.T, a, b any) []string {
	t.Helper()
	var x, y any
	_ = json.Unmarshal(mustJSON(t, a), &x)
	_ = json.Unmarshal(mustJSON(t, b), &y)
	var out []string
	var walk func(p string, x, y any)
	walk = func(p string, x, y any) {
		mx, okx := x.(map[string]any)
		my, oky := y.(map[string]any)
		if okx && oky {
			keys := map[string]bool{}
			for k := range mx {
				keys[k] = true
			}
			for k := range my {
				keys[k] = true
			}
			for k := range keys {
				walk(p+"/"+escapePointer(k), mx[k], my[k])
			}
			return
		}
		if jstr(t, x) != jstr(t, y) {
			out = append(out, p+": "+jstr(t, x)+" -> "+jstr(t, y))
		}
	}
	walk("", x, y)
	sort.Strings(out)
	return out
}

// 3.1 → 3.0 → 3.1 gives back the source, except where noted.
func TestDowncast31RoundTrip(t *testing.T) {
	src := loadRaw(t, []byte(openapi31Rich))
	down, _ := downcast(t, openapi31Rich, "3.0")
	up, err := RenderOpenAPIWith(IngestOpenAPI(loadSpec(t, mustJSON(t, down))), RenderOpenAPIOptions{Version: "3.1"})
	if err != nil {
		t.Fatal(err)
	}
	// Everything else is back as it was, including what 3.0 kept in
	// x-gwag-oas31 (prefixItems, if/then/else, info.summary, mutualTLS,
	// components.pathItems, ...). What differs is the same schema in
	// another spelling, or (marked) something 3.0 could not hold.
	checkIRDiff(t, jsonDiff(t, src, up), []string{
		`/components/schemas/Item/$defs: {"Local":{"properties":{"x":{"type":"string"}},"type":"object"}} -> null`,
		`/components/schemas/Item/properties/anything: true -> {}`,
		`/components/schemas/Item/properties/blob/format: null -> "byte"`,
		`/components/schemas/Item/properties/image/format: null -> "binary"`,
		`/components/schemas/Item/properties/kind/const: "item" -> null`,
		`/components/schemas/Item/properties/kind/enum: null -> ["item"]`,
		`/components/schemas/Item/properties/local/$ref: "#/components/schemas/Item/$defs/Local" -> "#/components/schemas/Local"`,
		`/components/schemas/Item/properties/multi/anyOf: null -> [{"type":"string"},{"type":"integer"}]`,
		`/components/schemas/Item/properties/multi/type: ["string","integer"] -> null`,
		`/components/schemas/Item/properties/multiNull/anyOf: null -> [{"type":"string"},{"type":"integer"},{"type":"null"}]`,
		`/components/schemas/Item/properties/multiNull/type: ["string","integer","null"] -> null`,
		`/components/schemas/Item/properties/onlyNull/enum: null -> [null]`,
		`/components/schemas/Item/properties/onlyNull/type: "null" -> null`,
		`/components/schemas/Item/properties/owner/$ref: "#/components/schemas/Owner" -> null`,
		`/components/schemas/Item/properties/owner/allOf: null -> [{"$ref":"#/components/schemas/Owner"}]`,
		// maximum 2 beside exclusiveMaximum 1 allows nothing more.
		`/components/schemas/Item/properties/ratio/maximum: 2 -> null`,
		`/components/schemas/Local: null -> {"properties":{"x":{"type":"string"}},"type":"object"}`,
		`/jsonSchemaDialect: "https://spec.openapis.org/oas/3.1/dialect/base" -> null`,
		// Added: 3.0 requires responses.
		`/paths/~1items/delete/responses: null -> {"default":{"description":""}}`,
		`/paths/~1ops/$ref: "#/components/pathItems/Ops" -> null`,
		`/paths/~1ops/get: null -> {"operationId":"listOps","responses":{"200":{"content":{"application/json":{"schema":{"items":{"type":"string"},"type":"array"}}},"description":"ok"}}}`,
		// Lossy for 3.0 tools: webhooks live in x-webhooks.
		`/webhooks: {"itemCreated":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Item"}}}},"responses":{"200":{"description":"ok"}}}}} -> null`,
		`/x-webhooks: null -> {"itemCreated":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Item"}}}},"responses":{"200":{"description":"ok"}}}}}`,
	})
}

func TestDowncast32(t *testing.T) {
	// kin-openapi v0.152 does not know several 3.2 members
	// (deviceAuthorization, tag parent, ...): the source is not validated.
	for _, version := range []string{"3.0", "3.1"} {
		t.Run(version, func(t *testing.T) {
			out, svc := downcast(t, openapi32Rich, version)
			loaded := loadSpec(t, mustJSON(t, out))
			if openAPIMajorMinor(loaded.OpenAPI) != version {
				t.Fatalf("openapi = %q", loaded.OpenAPI)
			}
			for path, want := range map[string]string{
				"paths/~1search/x-additional-operations/QUERY/operationId":  `"search"`,
				"paths/~1search/x-additional-operations/COPY/operationId":   `"copySearch"`,
				"paths/~1search/x-additional-operations/COPY/responses/204": `{"description":"","x-gwag-oas32":{"summary":"copied"}}`,
				"paths/~1search/query":                `null`,
				"paths/~1search/additionalOperations": `null`,
				"paths/~1events/get/responses/200/content/text~1event-stream/x-item-schema/properties/data/contentSchema": `{"$ref":"#/components/schemas/Hit"}`,
				"paths/~1hits/get/parameters":                                           `[{"in":"query","name":"n","schema":{"type":"integer"}}]`,
				"paths/~1hits/get/x-gwag-oas32/parameters/0/in":                         `"querystring"`,
				"paths/~1hits/get/responses/200/content/application~1json/examples/one": `{"value":[{"id":"h1"}],"x-gwag-oas32":{"dataValue":[{"id":"h1"}],"serializedValue":"[{\"id\":\"h1\"}]"}}`,
				"x-gwag-oas32":                     `{"$self":"https://example.com/openapi.json"}`,
				"servers/0":                        `{"url":"https://example.com","x-gwag-oas32":{"name":"prod"}}`,
				"tags":                             `[{"name":"items","x-gwag-oas32":{"kind":"nav","summary":"Items"}},{"name":"sub","x-gwag-oas32":{"parent":"items"}}]`,
				"components/securitySchemes/oauth": `{"flows":{"clientCredentials":{"scopes":{},"tokenUrl":"https://example.com/token"},"x-gwag-oas32":{"deviceAuthorization":{"deviceAuthorizationUrl":"https://example.com/device","scopes":{},"tokenUrl":"https://example.com/token"}}},"type":"oauth2","x-gwag-oas32":{"deprecated":true,"oauth2MetadataUrl":"https://example.com/.well-known/oauth-authorization-server"}}`,
				"components/schemas/Hit/xml":       `{"x-gwag-oas32":{"nodeType":"element"}}`,
				"components/schemas/Animal/discriminator": `{"propertyName":"kind","x-gwag-oas32":{"defaultMapping":"#/components/schemas/Hit"}}`,
			} {
				if got := jstr(t, jget(t, out, path)); got != want {
					t.Errorf("%s:\n got %s\nwant %s", path, got, want)
				}
			}
			score := jstr(t, jget(t, out, "components/schemas/Hit/properties/score"))
			if want := map[string]string{"3.0": `{"nullable":true,"type":"number"}`, "3.1": `{"type":["number","null"]}`}[version]; score != want {
				t.Errorf("score = %s, want %s", score, want)
			}
			// QUERY, the custom method and the stream read back from
			// their extensions. A querystring parameter (the whole query
			// string as one value) has no 3.0 / 3.1 form; it is kept in
			// x-gwag-oas32 and not read back.
			checkIRDiff(t, irDiff(svc, IngestOpenAPI(loaded)), []string{
				"+ op 0 GET /hits listHits(n:3:false:query) Hit",
				"- op 0 GET /hits listHits(q:1:false:querystring,n:3:false:query) Hit",
			})
			sum := summarizeIR(svc)
			for _, want := range []string{"op 0 QUERY /search search(", "op 1 COPY /search copySearch(", "op 2 GET /events events("} {
				if !strings.Contains(sum, want) {
					t.Errorf("source IR lacks %q:\n%s", want, sum)
				}
			}
		})
	}
}

// A 3.2 content entry that is a $ref to components.mediaTypes is
// inlined; the component moves to x-gwag-oas32.
func TestDowncast32MediaTypeRef(t *testing.T) {
	const spec = `{
	  "openapi": "3.2.0",
	  "info": {"title": "m", "version": "1"},
	  "paths": {"/x": {"get": {"operationId": "x", "responses": {"200": {"description": "ok",
	    "content": {"application/json": {"$ref": "#/components/mediaTypes/Thing"}}}}}}},
	  "components": {"mediaTypes": {"Thing": {"schema": {"type": ["string", "null"]}}}}
	}`
	out, _ := downcast(t, spec, "3.0")
	loadSpec(t, mustJSON(t, out))
	for path, want := range map[string]string{
		"paths/~1x/get/responses/200/content/application~1json": `{"schema":{"nullable":true,"type":"string"}}`,
		"components/x-gwag-oas32/mediaTypes/Thing":              `{"schema":{"type":["string","null"]}}`,
	} {
		if got := jstr(t, jget(t, out, path)); got != want {
			t.Errorf("%s:\n got %s\nwant %s", path, got, want)
		}
	}
}

// No version, or the version a document already has, leaves it as it
// is: a 3.1 or 3.2 source stays byte for byte what it was.
func TestRenderOpenAPIWith_AsDeclared(t *testing.T) {
	for name, src := range map[string]string{"3.1": openapi31Rich, "3.2": openapi32Rich, "3.0": openapi30Spec} {
		doc := loadRaw(t, []byte(src))
		svc := IngestOpenAPI(doc)
		for _, v := range []string{"", name} {
			if v == "3.1" || v == "3.2" {
				continue // 3.1 re-renders (OpenAPI31Version); 3.2 is not an option
			}
			out, err := RenderOpenAPIWith(svc, RenderOpenAPIOptions{Version: v})
			if err != nil {
				t.Fatal(err)
			}
			if out != doc {
				t.Errorf("%s with %q: not the source document", name, v)
			}
		}
	}
	synth := greeterService()
	out, err := RenderOpenAPIWith(synth, RenderOpenAPIOptions{Version: "3.0"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := RenderOpenAPI(synth)
	if !bytes.Equal(mustJSON(t, want), mustJSON(t, out)) || out.OpenAPI != "3.0.0" {
		t.Errorf("synthesized 3.0 changed: %s", out.OpenAPI)
	}
}

func TestParseOpenAPIVersion(t *testing.T) {
	for _, tc := range []struct{ q, def, want string }{
		{"", "", ""}, {"", "3.0", "3.0"}, {"", "3.1", "3.1"},
		{"3.0", "3.1", "3.0"}, {"3.0.3", "", "3.0"}, {"3.1", "", "3.1"}, {"3.1.1", "3.0", "3.1"},
	} {
		got, err := ParseOpenAPIVersion(tc.q, tc.def)
		if err != nil || got.Version != tc.want {
			t.Errorf("ParseOpenAPIVersion(%q, %q) = %q, %v; want %q", tc.q, tc.def, got.Version, err, tc.want)
		}
	}
	for _, bad := range []string{"2.0", "3.2", "3", "3.x", "v3.1", "3.1.0.1"} {
		if _, err := ParseOpenAPIVersion(bad, ""); err == nil {
			t.Errorf("ParseOpenAPIVersion(%q) accepted", bad)
		}
	}
	if _, err := ParseOpenAPIVersion("", "3.2"); err == nil {
		t.Error("default 3.2 accepted")
	}
}

// A synthesized 3.0 spec keeps QUERY under x-additional-operations;
// ingest reads it back as the same operation.
func TestIngestOpenAPI_XAdditionalOperations(t *testing.T) {
	svc := IngestOpenAPI(loadRaw(t, []byte(openapi32Rich)))
	svc.Origin = nil
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatal(err)
	}
	back := summarizeIR(IngestOpenAPI(loadRaw(t, mustJSON(t, doc))))
	for _, want := range []string{"op 0 QUERY /search search(body:Filter:false:body) Hit", "op 1 COPY /search copySearch("} {
		if !strings.Contains(back, want) {
			t.Errorf("lacks %q:\n%s", want, back)
		}
	}
}
