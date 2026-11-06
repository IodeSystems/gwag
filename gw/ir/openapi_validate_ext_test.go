package ir

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const validateExtSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Blob": {"type": "string", "format": "byte"},
    "Doc": {"type": "object", "properties": {"data": {"$ref": "#/components/schemas/Blob"}}},
    "Note": {"type": "object", "properties": {"text": {"type": "string"}}}
  }},
  "paths": {
    "/form": {"post": {
      "operationId": "form",
      "requestBody": {"content": {"multipart/form-data": {"schema": {
        "type": "object", "properties": {
          "file": {"type": "string", "format": "byte"},
          "files": {"type": "array", "items": {"type": "string", "format": "byte"}},
          "enc": {"type": "string", "contentEncoding": "base64"},
          "maybe": {"oneOf": [{"type": "string", "format": "byte"}, {"type": "null"}]},
          "wrapped": {"allOf": [{"type": "string", "format": "byte"}]},
          "nested": {"type": "object", "properties": {"sig": {"type": "string", "format": "byte"}}},
          "plain": {"type": "string"}
        }
      }}}},
      "responses": {"200": {"description": "ok"}}
    }},
    "/union": {"post": {
      "operationId": "union",
      "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
        "bytesOrInt": {"oneOf": [{"$ref": "#/components/schemas/Blob"}, {"type": "integer"}]},
        "bytesOrString": {"anyOf": [{"$ref": "#/components/schemas/Blob"}, {"type": "string"}]},
        "docOrNote": {"oneOf": [{"$ref": "#/components/schemas/Doc"}, {"$ref": "#/components/schemas/Note"}]},
        "twoBlobs": {"oneOf": [{"$ref": "#/components/schemas/Blob"}, {"type": "string", "format": "byte", "maxLength": 8}]}
      }}}}},
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object", "properties": {"data": {"$ref": "#/components/schemas/Blob"}}
      }}}}}
    }},
    "/resp": {"get": {
      "operationId": "resp",
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object", "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Doc"}}}
      }}}}}
    }}
  }
}`

func validateExtOp(t *testing.T, path, method string) *openapi3.Operation {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(validateExtSpec))
	if err != nil {
		t.Fatal(err)
	}
	return doc.Paths.Value(path).Operations()[method]
}

const notBase64 = "not base64!"

// A multipart string part that is not a file upload is base64 text.
// A plain format:byte part (and an array of it) is a file: it carries
// a *Upload, which is never checked.
func TestOpenAPIValidator_Multipart(t *testing.T) {
	v := NewOpenAPIValidator(validateExtOp(t, "/form", "POST"), "form")
	if v == nil {
		t.Fatal("no validator")
	}
	bad := map[string]any{
		"file":    notBase64,
		"files":   []any{notBase64},
		"enc":     notBase64,
		"maybe":   notBase64,
		"wrapped": notBase64,
		"nested":  map[string]any{"sig": notBase64},
		"plain":   notBase64,
	}
	err := v.Validate(bad, ValidationStrict)
	if err == nil {
		t.Fatal("accepted")
	}
	if want := "invalid base64 in enc, maybe, nested.sig, wrapped"; err.Error() != want {
		t.Errorf("got  %s\nwant %s", err, want)
	}
	good := map[string]any{"enc": "aGk=", "maybe": nil, "wrapped": "aGk", "nested": map[string]any{"sig": "aGk="}, "file": notBase64}
	if err := v.Validate(good, ValidationStrict); err != nil {
		t.Errorf("valid args: %v", err)
	}
	if err := v.Validate(bad, ValidationWarn); err != nil {
		t.Errorf("warn: %v", err)
	}
}

func TestOpenAPIValidator_Unions(t *testing.T) {
	v := NewOpenAPIValidator(validateExtOp(t, "/union", "POST"), "union")
	if v == nil {
		t.Fatal("no validator")
	}
	body := func(k string, val any) map[string]any {
		return map[string]any{"body": map[string]any{k: val}}
	}
	for _, c := range []struct {
		name string
		args map[string]any
		bad  string // "" accepts
	}{
		// The value's type picks the branch.
		{"string picks bytes", body("bytesOrInt", notBase64), "body.bytesOrInt"},
		{"valid bytes", body("bytesOrInt", "aGk="), ""},
		{"integer picks the integer branch", body("bytesOrInt", 7.0), ""},
		// A string fits both branches: ambiguous, any accept passes.
		{"ambiguous accepts", body("bytesOrString", notBase64), ""},
		{"ambiguous valid", body("bytesOrString", "aGk="), ""},
		// Object branches: Doc rejects, Note declares no base64, so accepts.
		{"object, other branch accepts", body("docOrNote", map[string]any{"data": notBase64}), ""},
		{"object valid", body("docOrNote", map[string]any{"data": "aGk="}), ""},
		// Every branch rejects: reported once.
		{"all branches reject", body("twoBlobs", notBase64), "body.twoBlobs"},
		{"all branches accept", body("twoBlobs", "aGk="), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := v.Validate(c.args, ValidationStrict)
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case c.bad != "" && err == nil:
				t.Fatal("accepted")
			case c.bad != "" && err.Error() != "invalid base64 in "+c.bad:
				t.Fatalf("got %s, want path %s", err, c.bad)
			}
		})
	}
}

// Responses are checked in warn mode only, and only logged.
func TestOpenAPIValidator_Response(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	v := NewOpenAPIValidator(validateExtOp(t, "/resp", "GET"), "resp")
	if v == nil {
		t.Fatal("response-only op compiled no validator")
	}
	// No arg declarations: Validate stays a no-op.
	if err := v.Validate(map[string]any{"x": notBase64}, ValidationStrict); err != nil {
		t.Errorf("args: %v", err)
	}
	resp := map[string]any{"items": []any{map[string]any{"data": "aGk="}, map[string]any{"data": notBase64}}}

	for _, mode := range []string{ValidationStrict, "", ValidationOff} {
		v.ValidateResponse(resp, mode)
		if logged.Len() != 0 {
			t.Fatalf("%q mode logged: %s", mode, logged.String())
		}
	}
	v.ValidateResponse(resp, ValidationWarn)
	out := logged.String()
	if !strings.Contains(out, "items[1].data") || strings.Contains(out, "items[0]") {
		t.Errorf("log = %q", out)
	}
	if strings.Contains(out, notBase64) {
		t.Error("log carries the value")
	}
	logged.Reset()
	v.ValidateResponse(map[string]any{"items": []any{map[string]any{"data": "aGk="}}}, ValidationWarn)
	if logged.Len() != 0 {
		t.Errorf("valid response logged: %s", logged.String())
	}
	var nilV *OpenAPIValidator
	nilV.ValidateResponse(resp, ValidationWarn)

	// An op with a request declaration validates the request too.
	u := NewOpenAPIValidator(validateExtOp(t, "/union", "POST"), "union")
	logged.Reset()
	u.ValidateResponse(map[string]any{"data": notBase64}, ValidationWarn)
	if !strings.Contains(logged.String(), "data") {
		t.Errorf("log = %q", logged.String())
	}
}
