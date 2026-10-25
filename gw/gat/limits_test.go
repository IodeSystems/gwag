package gat_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

func newLimitsGat(t *testing.T, configure func(*gat.Gateway)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	var calls atomic.Int32
	g := mustNewGat(t)
	configure(g)
	gat.Register(api, g, huma.Operation{OperationID: "opA", Method: http.MethodGet, Path: "/a/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			calls.Add(1)
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestMaxOperations_ExcessAliasesAreNotDispatched(t *testing.T) {
	for _, parallel := range []int{0, 4} {
		srv, calls := newLimitsGat(t, func(g *gat.Gateway) { g.MaxOperations(3).ParallelQueries(parallel) })
		resp := mustGraphQL(t, srv.URL+"/api/graphql",
			`query { Demo { a: opA(id: "1") { id } b: opA(id: "2") { id } c: opA(id: "3") { id } d: opA(id: "4") { id } e: opA(id: "5") { id } } }`)
		if n := calls.Load(); n != 3 {
			t.Errorf("parallel=%d: handler ran %d times, want 3", parallel, n)
		}
		errs, _ := resp["errors"].([]any)
		if len(errs) != 2 {
			t.Fatalf("parallel=%d: %d errors, want 2: %v", parallel, len(errs), resp)
		}
		first, _ := errs[0].(map[string]any)
		if !strings.Contains(first["message"].(string), "exceeds 3 operations") {
			t.Errorf("message = %v", first["message"])
		}
		if st := digPath(first, "extensions", "status"); st != float64(http.StatusBadRequest) {
			t.Errorf("extensions.status = %v, want 400", st)
		}
	}
}

func TestMaxOperations_CountsPerRequest(t *testing.T) {
	srv, calls := newLimitsGat(t, func(g *gat.Gateway) { g.MaxOperations(1) })
	for range 3 {
		resp := mustGraphQL(t, srv.URL+"/api/graphql", `query { Demo { opA(id: "1") { id } } }`)
		if resp["errors"] != nil {
			t.Fatalf("single op over the cap: %v", resp)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("calls = %d, want 3 (the cap is per request)", n)
	}
}

func TestMaxRequestBytes(t *testing.T) {
	srv, _ := newLimitsGat(t, func(g *gat.Gateway) { g.MaxRequestBytes(256) })
	big := `{"query":"query { Demo { opA(id: \"` + strings.Repeat("x", 300) + `\") { id } } }"}`
	post := func(body io.Reader, contentLength int64) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/graphql", body)
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = contentLength
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(strings.NewReader(big), int64(len(big))); code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared length over the cap: status %d, want 413", code)
	}
	// io.MultiReader hides the length, so the client sends it chunked.
	if code := post(io.MultiReader(strings.NewReader(big)), -1); code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked body over the cap: status %d, want 413", code)
	}
	small := `{"query":"query { Demo { opA(id: \"1\") { id } } }"}`
	if code := post(strings.NewReader(small), int64(len(small))); code != http.StatusOK {
		t.Errorf("small body: status %d, want 200", code)
	}
}
