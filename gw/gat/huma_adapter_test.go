package gat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// opaqueAdapter is humago with the net/http request hidden: each
// context is wrapped in a type exposing only huma.Context, the way a
// gin / echo / fiber context looks to gat. It keeps the
// request-rebuild fallback in serveAdapter under test.
type opaqueAdapter struct{ huma.Adapter }

// humaCtx names the embedded field so it doesn't shadow Context().
type humaCtx = huma.Context

type opaqueCtx struct{ humaCtx }

func (a opaqueAdapter) Handle(op *huma.Operation, h func(huma.Context)) {
	a.Adapter.Handle(op, func(ctx huma.Context) { h(opaqueCtx{ctx}) })
}

func TestAdapterWithoutUnwrap(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), opaqueAdapter{humago.NewAdapter(mux, "")})
	g := mustNewGat(t).MaxRequestBytes(256)
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID, Name: "Project " + in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := mustGraphQL(t, srv.URL+"/api/graphql", `query { Demo { getProject(id: "p1") { name } } }`)
	if got := digPath(resp, "data", "Demo", "getProject", "name"); got != "Project p1" {
		t.Errorf("name = %v: %v", got, resp)
	}
	if body := string(mustGET(t, srv.URL+"/api/schema/graphql")); !strings.Contains(body, "getProject") {
		t.Errorf("SDL missing getProject: %s", body)
	}
	big := `{"query":"` + strings.Repeat("x", 300) + `"}`
	r, err := http.Post(srv.URL+"/api/graphql", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d, want 413 (Content-Length carried through the rebuild)", r.StatusCode)
	}
}
