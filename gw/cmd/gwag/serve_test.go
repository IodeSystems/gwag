package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeOpenAPINamespace(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"Pets", "pets"},
		{"Pet Store", "petstore"},
		{"hello-world!", "helloworld"},
		{"123abc", "_123abc"},
		{"v1_api", "v1_api"},
	}
	for _, tc := range cases {
		got := sanitizeOpenAPINamespace(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeOpenAPINamespace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const petsSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Pets", "version": "1.0.0"},
  "paths": {
    "/pets/{id}": {
      "get": {
        "operationId": "getPet",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}
        ],
        "responses": {
          "200": {
            "description": "ok",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "properties": {
          "id":   {"type": "string"},
          "name": {"type": "string"}
        }
      }
    }
  }
}`

func TestLoadOpenAPIRegistration_E2E(t *testing.T) {
	// Upstream that echoes the path id back as a Pet.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /pets/{id} — pluck the id.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 || parts[0] != "pets" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": parts[1], "name": "Pet " + parts[1],
		})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	specPath := filepath.Join(dir, "pets.json")
	if err := os.WriteFile(specPath, []byte(petsSpec), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	regs, err := loadOpenAPIRegistration(specPath, upstream.URL)
	if err != nil {
		t.Fatalf("loadOpenAPIRegistration: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected 1 reg, got %d", len(regs))
	}
	if ns := regs[0].Service.Namespace; ns != "pets" {
		t.Errorf("namespace: got %q, want %q", ns, "pets")
	}
	if ver := regs[0].Service.Version; ver != "v1" {
		t.Errorf("version: got %q, want %q", ver, "v1")
	}
	if regs[0].BaseURL != upstream.URL {
		t.Errorf("baseURL: got %q, want %q", regs[0].BaseURL, upstream.URL)
	}
}

func TestLoadOpenAPIRegistration_NamespaceFromFilenameWhenNoTitle(t *testing.T) {
	specNoTitle := strings.Replace(petsSpec, `"title": "Pets"`, `"title": ""`, 1)
	dir := t.TempDir()
	specPath := filepath.Join(dir, "my_cool_api.json")
	if err := os.WriteFile(specPath, []byte(specNoTitle), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	regs, err := loadOpenAPIRegistration(specPath, "http://example.invalid")
	if err != nil {
		t.Fatalf("loadOpenAPIRegistration: %v", err)
	}
	if ns := regs[0].Service.Namespace; ns != "my_cool_api" {
		t.Errorf("namespace: got %q, want %q (fallback to filename stem)", ns, "my_cool_api")
	}
}

func TestServeCmd_RejectsMissingArgs(t *testing.T) {
	cases := [][]string{
		{},                                          // no source
		{"--openapi", "spec.yaml", "--proto", "x"},  // both sources
		{"--openapi", "spec.yaml"},                  // no --to
		{"--proto", "x.proto"},                      // no --to
	}
	// Redirect stderr so the usage messages don't pollute test output.
	origStderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	if devnull != nil {
		os.Stderr = devnull
		defer func() { os.Stderr = origStderr; devnull.Close() }()
	}
	for _, args := range cases {
		if code := serveCmd(args); code == 0 {
			t.Errorf("serveCmd(%v) = 0, want non-zero", args)
		}
	}
}

// silence unused
var _ = bytes.Buffer{}
var _ = io.Discard
