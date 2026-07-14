package ir

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// A header/cookie parameter whose name isn't a valid GraphQL identifier (e.g. hyphenated transport
// headers like X-Forwarded-For / User-Agent) must be omitted from the ingested Args rather than
// carried through to panic graphql.NewSchema. Valid-named params (path/query/header/cookie) stay.
const openapiHeaderNameSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "hdr", "version": "1.0.0", "description": "demo"},
  "paths": {
    "/thing/{id}": {
      "get": {
        "operationId": "getThing",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "q", "in": "query", "schema": {"type": "string"}},
          {"name": "traceid", "in": "header", "schema": {"type": "string"}},
          {"name": "X-Forwarded-For", "in": "header", "schema": {"type": "string"}},
          {"name": "User-Agent", "in": "header", "schema": {"type": "string"}},
          {"name": "x-weird-cookie", "in": "cookie", "schema": {"type": "string"}}
        ],
        "responses": {
          "200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}
        }
      }
    }
  }
}`

func TestOpenAPIIngest_SkipsNonIdentifierHeaderParams(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiHeaderNameSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := IngestOpenAPI(doc)

	var op *Operation
	for _, o := range svc.Operations {
		if o.Name == "getThing" {
			op = o
		}
	}
	if op == nil {
		t.Fatal("getThing missing")
	}

	got := map[string]bool{}
	for _, a := range op.Args {
		got[a.Name] = true
	}

	// Kept: identifier-safe params in every location.
	for _, want := range []string{"id", "q", "traceid"} {
		if !got[want] {
			t.Errorf("Args missing %q; got %v", want, keys(got))
		}
	}
	// Dropped: hyphenated header/cookie names that GraphQL can't represent.
	for _, bad := range []string{"X-Forwarded-For", "User-Agent", "x-weird-cookie"} {
		if got[bad] {
			t.Errorf("Args unexpectedly kept non-identifier param %q — it would panic graphql.NewSchema", bad)
		}
	}
}

func TestIsGraphQLName(t *testing.T) {
	valid := []string{"traceid", "_x", "Foo9", "a_b_c", "X9"}
	invalid := []string{"", "X-Forwarded-For", "User-Agent", "9lives", "a b", "content.type", "café"}
	for _, s := range valid {
		if !isGraphQLName(s) {
			t.Errorf("isGraphQLName(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isGraphQLName(s) {
			t.Errorf("isGraphQLName(%q) = true, want false", s)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
