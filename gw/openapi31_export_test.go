package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// openAPI31ExportSpec is a 3.1 source with the forms 3.0 spells
// differently.
const openAPI31ExportSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "test", "version": "1.0.0"},
  "paths": {
    "/things/{id}": {
      "get": {
        "operationId": "getThing",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"id": {"type": "string", "examples": ["t1"]}, "note": {"type": ["string", "null"]}}
        }}}}}
      }
    }
  }
}`

func schemaOpenAPIGateway(t *testing.T, spec string, opts ...Option) *Gateway {
	t.Helper()
	opts = append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored"))}, opts...)
	gw := New(opts...)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(spec), To("http://127.0.0.1:1"), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	return gw
}

func getSchemaOpenAPI(t *testing.T, gw *Gateway, query string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	gw.SchemaOpenAPIHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/schema/openapi"+query, nil))
	return rr
}

// specOf returns the one spec in a SchemaOpenAPIHandler response.
func specOf(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var out map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	for _, byVer := range out["test"] {
		return byVer
	}
	t.Fatalf("no test spec in %s", body)
	return nil
}

func TestSchemaOpenAPIHandler_Version(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  string
		opts  []Option
		query string
		want  string
		code  int
	}{
		{"default", minimalOpenAPISpec, nil, "", `"openapi": "3.0.0"`, http.StatusOK},
		{"query31", minimalOpenAPISpec, nil, "?openapi=3.1", `"openapi": "3.1.0"`, http.StatusOK},
		{"option", minimalOpenAPISpec, []Option{WithOpenAPIVersion("3.1")}, "", `"openapi": "3.1.0"`, http.StatusOK},
		{"option_query30", minimalOpenAPISpec, []Option{WithOpenAPIVersion("3.1")}, "?openapi=3.0", `"openapi": "3.0.0"`, http.StatusOK},
		{"bad", minimalOpenAPISpec, nil, "?openapi=2.0", "", http.StatusBadRequest},
		{"no32", minimalOpenAPISpec, nil, "?openapi=3.2", "", http.StatusBadRequest},

		{"31_default", openAPI31ExportSpec, nil, "", `"openapi": "3.1.0"`, http.StatusOK},
		{"31_query30", openAPI31ExportSpec, nil, "?openapi=3.0", `"openapi": "3.0.3"`, http.StatusOK},
		{"31_option30", openAPI31ExportSpec, []Option{WithOpenAPIVersion("3.0")}, "", `"openapi": "3.0.3"`, http.StatusOK},
		{"31_option30_query31", openAPI31ExportSpec, []Option{WithOpenAPIVersion("3.0")}, "?openapi=3.1", `"openapi": "3.1.0"`, http.StatusOK},

		{"32_default", openAPI32Spec, nil, "", `"openapi": "3.2.0"`, http.StatusOK},
		{"32_query30", openAPI32Spec, nil, "?openapi=3.0", `"openapi": "3.0.3"`, http.StatusOK},
		{"32_query31", openAPI32Spec, nil, "?openapi=3.1", `"openapi": "3.1.0"`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := schemaOpenAPIGateway(t, tc.spec, tc.opts...)
			rr := getSchemaOpenAPI(t, gw, tc.query)
			if rr.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rr.Code, tc.code, rr.Body.String())
			}
			if tc.want == "" {
				return
			}
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("want %s, got %.300s", tc.want, rr.Body.String())
			}
			// Every version served is a valid document of that version.
			loader := openapi3.NewLoader()
			doc, err := loader.LoadFromData(specOf(t, rr.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Validate(loader.Context); err != nil {
				t.Fatalf("validate %s: %v", doc.OpenAPI, err)
			}
		})
	}
}

// Without ?openapi and without the option, a 3.1 or 3.2 source is
// served exactly as before the down-conversion existed: the source
// document as kin-openapi re-encodes it.
func TestSchemaOpenAPIHandler_DefaultIsSource(t *testing.T) {
	for _, spec := range []string{openAPI31ExportSpec, openAPI32Spec} {
		gw := schemaOpenAPIGateway(t, spec)
		got := specOf(t, getSchemaOpenAPI(t, gw, "").Body.Bytes())
		src, err := openapi3.NewLoader().LoadFromData([]byte(spec))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(src)
		var compact bytes.Buffer
		if err := json.Compact(&compact, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(compact.Bytes(), want) {
			t.Errorf("default output is not the source:\n got %s\nwant %s", compact.Bytes(), want)
		}
	}
}

// ?openapi=3.0 of a 3.2 source keeps QUERY and the custom method
// under x-additional-operations.
func TestSchemaOpenAPIHandler_32To30(t *testing.T) {
	gw := schemaOpenAPIGateway(t, openAPI32Spec)
	spec := string(specOf(t, getSchemaOpenAPI(t, gw, "?openapi=3.0").Body.Bytes()))
	for _, want := range []string{`"x-additional-operations"`, `"QUERY"`, `"PURGE"`, `"searchThings"`, `"purgeThing"`} {
		if !strings.Contains(spec, want) {
			t.Errorf("lacks %s:\n%s", want, spec)
		}
	}
	for _, bad := range []string{`"query":`, `"additionalOperations"`} {
		if strings.Contains(spec, bad) {
			t.Errorf("has %s:\n%s", bad, spec)
		}
	}
}

func TestWithOpenAPIVersion_Bad(t *testing.T) {
	for _, v := range []string{"3.2", "2.0", "x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithOpenAPIVersion(%q) did not panic", v)
				}
			}()
			WithOpenAPIVersion(v)
		}()
	}
}
