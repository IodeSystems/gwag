package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	gateway "github.com/iodesystems/gwag/v2/gw"
)

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
	if ns := regs[0].Service.Namespace; ns != "Pets" {
		t.Errorf("namespace: got %q, want %q (case preserved from spec title)", ns, "Pets")
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
		{}, // no source
		{"--openapi", "spec.yaml", "--proto", "x"}, // both sources
		{"--openapi", "spec.yaml"},                 // no --to
		{"--proto", "x.proto"},                     // no --to
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

type connectGreeter struct {
	greeterv1.UnimplementedGreeterServiceServer
}

func (connectGreeter) Hello(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	return &greeterv1.HelloResponse{Greeting: "hello " + req.GetName()}, nil
}

// The full-gateway serve mux serves registered proto services over
// Connect at /api/connect/.
func TestFullGatewayMux_ConnectCall(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	be := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(be, connectGreeter{})
	go func() { _ = be.Serve(lis) }()
	t.Cleanup(be.Stop)

	gw := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(gw.Close)
	if err := gw.AddProto("../../../examples/multi/protos/greeter.proto",
		gateway.To(lis.Addr().String()), gateway.As("greeter")); err != nil {
		t.Fatalf("AddProto: %v", err)
	}

	srv := httptest.NewServer(fullGatewayMux(gw, false))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/connect/greeter.v1.GreeterService/Hello",
		"application/json", strings.NewReader(`{"name":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct{ Greeting string }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if out.Greeting != "hello world" {
		t.Fatalf("greeting %q (body %s)", out.Greeting, body)
	}
}
