package gat_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMaxOperations_RefusesTheWholeDocument(t *testing.T) {
	for _, parallel := range []int{0, 4} {
		srv, calls := newLimitsGat(t, func(g *gat.Gateway) { g.MaxOperations(3).ParallelQueries(parallel) })
		resp := mustGraphQL(t, srv.URL+"/api/graphql",
			`query { Demo { a: opA(id: "1") { id } b: opA(id: "2") { id } c: opA(id: "3") { id } d: opA(id: "4") { id } e: opA(id: "5") { id } } }`)
		if n := calls.Load(); n != 0 {
			t.Errorf("parallel=%d: handler ran %d times, want 0", parallel, n)
		}
		errs, _ := resp["errors"].([]any)
		if len(errs) != 1 || resp["data"] != nil {
			t.Fatalf("parallel=%d: want one error and no data: %v", parallel, resp)
		}
		first, _ := errs[0].(map[string]any)
		if !strings.Contains(first["message"].(string), "selects 5 operations, over the limit of 3") {
			t.Errorf("message = %v", first["message"])
		}
		resp = mustGraphQL(t, srv.URL+"/api/graphql",
			`query { Demo { a: opA(id: "1") { id } b: opA(id: "2") { id } c: opA(id: "3") { id } } }`)
		if resp["errors"] != nil || calls.Load() != 3 {
			t.Errorf("parallel=%d: at the cap: calls=%d resp=%v", parallel, calls.Load(), resp)
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

func TestMutationOverGETIsRefused(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	var ran atomic.Int32
	gat.Register(api, g, huma.Operation{OperationID: "setA", Method: http.MethodPost, Path: "/a/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			ran.Add(1)
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "getA", Method: http.MethodGet, Path: "/a/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, resp := getJSON(t, srv.URL+"/api/graphql?query="+url.QueryEscape(`mutation { Demo { setA(id: "1") { id } } }`))
	if n := ran.Load(); n != 0 {
		t.Fatalf("mutation ran %d times over GET", n)
	}
	if status != http.StatusMethodNotAllowed {
		t.Errorf("GET mutation: status %d, want 405: %v", status, resp)
	}

	if r := mustGraphQL(t, srv.URL+"/api/graphql", `mutation { Demo { setA(id: "1") { id } } }`); r["errors"] != nil || ran.Load() != 1 {
		t.Errorf("POST mutation: ran=%d resp=%v", ran.Load(), r)
	}
	status, resp = getJSON(t, srv.URL+"/api/graphql?query="+url.QueryEscape(`query { Demo { getA(id: "1") { id } } }`))
	if status != http.StatusOK || digPath(resp, "data", "Demo", "getA", "id") != "1" {
		t.Errorf("GET query broke: %v", resp)
	}
}

func TestMaxOperations_DefaultIs64(t *testing.T) {
	srv, calls := newLimitsGat(t, func(*gat.Gateway) {})
	var q strings.Builder
	q.WriteString("query { Demo {")
	for i := range 65 {
		fmt.Fprintf(&q, ` a%d: opA(id: "%d") { id }`, i, i)
	}
	q.WriteString(" } }")
	resp := mustGraphQL(t, srv.URL+"/api/graphql", q.String())
	if n := calls.Load(); n != 0 {
		t.Errorf("handler ran %d times, want 0", n)
	}
	if errs, _ := resp["errors"].([]any); len(errs) != 1 {
		t.Errorf("%d errors, want 1", len(errs))
	}
}

func TestMaxDepth(t *testing.T) {
	srv, calls := newLimitsGat(t, func(g *gat.Gateway) { g.MaxDepth(2) })
	gql := srv.URL + "/api/graphql"
	refused := func(name, q string) {
		t.Helper()
		resp := mustGraphQL(t, gql, q)
		errs, _ := resp["errors"].([]any)
		if resp["data"] != nil || len(errs) == 0 {
			t.Errorf("%s: want refused, got %v", name, resp)
			return
		}
		if msg, _ := errs[0].(map[string]any)["message"].(string); !strings.Contains(msg, "exceeds the limit of 2") {
			t.Errorf("%s: message %q", name, msg)
		}
	}
	refused("direct", `query { Demo { opA(id: "1") { id } } }`)
	refused("fragment", `query { Demo { ...F } } fragment F on DemoQueryNamespace { opA(id: "1") { ... on Demo_Project { id } } }`)
	if n := calls.Load(); n != 0 {
		t.Errorf("handler ran %d times for refused queries", n)
	}
	if resp := mustGraphQL(t, gql, `query { Demo { __typename } }`); resp["errors"] != nil {
		t.Errorf("depth 2 refused: %v", resp)
	}
	// A fragment cycle must not hang the depth walk (the spec's
	// NoFragmentCycles rule reports it).
	resp := mustGraphQL(t, gql, `query { Demo { ...A } } fragment A on DemoQueryNamespace { ...B } fragment B on DemoQueryNamespace { ...A }`)
	if resp["errors"] == nil {
		t.Errorf("fragment cycle accepted: %v", resp)
	}
}

func TestDisableIntrospection(t *testing.T) {
	srv, _ := newLimitsGat(t, func(g *gat.Gateway) { g.DisableIntrospection() })
	gql := srv.URL + "/api/graphql"
	for _, q := range []string{`{ __schema { queryType { name } } }`, `{ __type(name: "Query") { name } }`} {
		resp := mustGraphQL(t, gql, q)
		if resp["data"] != nil || resp["errors"] == nil {
			t.Errorf("%s: want refused, got %v", q, resp)
		}
	}
	if resp := mustGraphQL(t, gql, `query { Demo { __typename opA(id: "1") { id } } }`); resp["errors"] != nil {
		t.Errorf("__typename refused: %v", resp)
	}
	if status, _ := getStatus(t, srv.URL+"/api/schema/graphql"); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Errorf("/schema/graphql still served: %d", status)
	}
}

func getStatus(t *testing.T, url string) (int, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
