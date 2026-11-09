package gat_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// TestSingleVersionGolden pins the schema views of a one-API gateway
// (gat.RegisterHuma) byte for byte. Multi-version support must leave
// them unchanged. GAT_UPDATE_GOLDEN=1 rewrites the files. (The
// introspection JSON is left out: its type order is not stable.)
func TestSingleVersionGolden(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "listProjects", Method: http.MethodGet, Path: "/projects"},
		func(context.Context, *listProjectsInput) (*listProjectsOutput, error) { return &listProjectsOutput{}, nil })
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(_ context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "renameProject", Method: http.MethodPost, Path: "/projects/{id}/name"},
		func(_ context.Context, in *goldenRenameInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID, Name: in.Body.Name}}, nil
		})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "projectEvents", Method: http.MethodGet, Path: "/projects/{id}/events"},
		map[string]any{"project": project{}},
		func(context.Context, *getProjectInput, sse.Sender) {})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, c := range []struct{ file, path string }{
		{"schema.graphql", "/api/schema/graphql"},
		{"schema.fds", "/api/schema/proto"},
		{"openapi.json", "/api/schema/openapi"},
		{"openapi30.json", "/api/schema/openapi?openapi=3.0"},
	} {
		got := mustGET(t, srv.URL+c.path)
		checkGolden(t, filepath.Join("testdata", "single_version", c.file), got)
	}
	checkGolden(t, filepath.Join("testdata", "single_version", "sdl.graphql"), []byte(g.GraphQLSDL()))
}

type goldenRenameInput struct {
	ID   string `path:"id"`
	Body struct {
		Name string `json:"name"`
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("GAT_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (GAT_UPDATE_GOLDEN=1 writes it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: output differs from golden (%d bytes, want %d)\ngot:\n%s", path, len(got), len(want), got)
	}
}
