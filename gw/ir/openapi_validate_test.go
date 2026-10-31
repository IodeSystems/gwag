package ir

import (
	"bytes"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const validateSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Blob": {"type": "string", "format": "byte"},
    "Node": {"type": "object", "properties": {
      "sig": {"$ref": "#/components/schemas/Blob"},
      "next": {"$ref": "#/components/schemas/Node"}
    }},
    "Extra": {"type": "object", "properties": {"extra": {"$ref": "#/components/schemas/Blob"}}}
  }},
  "paths": {
    "/checked": {"post": {
      "operationId": "checked",
      "parameters": [{"name": "tag", "in": "query", "schema": {"type": "array", "items": {"type": "string", "format": "byte"}}}],
      "requestBody": {"content": {"application/json": {"schema": {
        "allOf": [{"$ref": "#/components/schemas/Extra"}],
        "type": "object",
        "properties": {
          "node": {"$ref": "#/components/schemas/Node"},
          "named": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/Blob"}},
          "maybe": {"oneOf": [{"$ref": "#/components/schemas/Blob"}, {"type": "null"}]},
          "either": {"oneOf": [{"$ref": "#/components/schemas/Blob"}, {"type": "integer"}, {"type": "string"}]},
          "plain": {"type": "string"}
        }
      }}}},
      "responses": {"200": {"description": "ok"}}
    }},
    "/form": {"post": {
      "operationId": "form",
      "requestBody": {"content": {"multipart/form-data": {"schema": {
        "type": "object", "properties": {"file": {"type": "string", "format": "byte"}}
      }}}},
      "responses": {"200": {"description": "ok"}}
    }},
    "/none": {"get": {
      "operationId": "none",
      "parameters": [{"name": "q", "in": "query", "schema": {"type": "string"}}],
      "responses": {"200": {"description": "ok"}}
    }}
  }
}`

func validateOp(t *testing.T, path string) *openapi3.Operation {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(validateSpec))
	if err != nil {
		t.Fatal(err)
	}
	return doc.Paths.Value(path).Operations()["POST"]
}

func TestOpenAPIValidator(t *testing.T) {
	v := NewOpenAPIValidator(validateOp(t, "/checked"), "checked")
	if v == nil {
		t.Fatal("no validator")
	}
	bad := "not base64!"
	args := map[string]any{
		"tag": []any{"aGk=", bad},
		"body": map[string]any{
			"extra":  bad,
			"node":   map[string]any{"sig": "aGk=", "next": map[string]any{"sig": bad}},
			"named":  map[string]any{"k": bad},
			"maybe":  bad,
			"either": bad,
			"plain":  bad,
		},
	}
	err := v.Validate(args, ValidationStrict)
	if err == nil {
		t.Fatal("accepted")
	}
	want := "invalid base64 in body.extra, body.maybe, body.named.k, body.node.next.sig, tag[1]"
	if err.Error() != want {
		t.Errorf("got  %s\nwant %s", err, want)
	}
	if strings.Contains(err.Error(), bad) {
		t.Error("error carries the value")
	}
	for _, mode := range []string{ValidationWarn, ValidationOff} {
		if err := v.Validate(args, mode); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	if err := v.Validate(args, ""); err == nil {
		t.Error(`"" is not strict`)
	}
	ok := map[string]any{"tag": []any{"aGk", "a-_a"}, "body": map[string]any{"extra": "", "maybe": nil}}
	if err := v.Validate(ok, ValidationStrict); err != nil {
		t.Errorf("valid args: %v", err)
	}
}

// A client picks map keys and list indexes, so warnings must not
// grow with them.
func TestOpenAPIValidator_WarningsBounded(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	v := NewOpenAPIValidator(validateOp(t, "/checked"), "checked")
	for i := 0; i < maxValidationWarnings*3; i++ {
		args := map[string]any{"body": map[string]any{"named": map[string]any{"k\n" + strconv.Itoa(i): "!"}}}
		if err := v.Validate(args, ValidationWarn); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(logged.String(), "\n"); got != maxValidationWarnings {
		t.Errorf("%d log lines:\n%s", got, logged.String())
	}
}

func TestOpenAPIValidator_NothingToCheck(t *testing.T) {
	if NewOpenAPIValidator(validateOp(t, "/form"), "form") != nil {
		t.Error("multipart body compiled a validator")
	}
	if NewOpenAPIValidator(nil, "nil") != nil {
		t.Error("nil op compiled a validator")
	}
	var v *OpenAPIValidator
	if err := v.Validate(map[string]any{"x": "!"}, ValidationStrict); err != nil {
		t.Error(err)
	}
}
