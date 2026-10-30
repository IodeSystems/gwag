package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type order struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
}

type listOrdersInput struct {
	Limit int `query:"limit"`
}

type listOrdersOutput struct {
	Body []order
}

type customersInput struct {
	IDs []string `query:"ids"`
}

type customer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type customersOutput struct {
	Body []customer
}

// newShopGat serves listOrders (four orders, two sharing a customer)
// and getCustomersById, which records every ids list it is called with.
func newShopGat(t *testing.T, configure func(*gat.Gateway)) (*httptest.Server, func() [][]string) {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Shop", "1.0.0"))
	g := mustNewGat(t)
	configure(g)
	var (
		mu    sync.Mutex
		calls [][]string
	)
	gat.Register(api, g, huma.Operation{OperationID: "listOrders", Method: http.MethodGet, Path: "/orders"},
		func(ctx context.Context, in *listOrdersInput) (*listOrdersOutput, error) {
			return &listOrdersOutput{Body: []order{
				{ID: "o1", CustomerID: "c1"}, {ID: "o2", CustomerID: "c2"},
				{ID: "o3", CustomerID: "c1"}, {ID: "o4", CustomerID: "c3"},
			}}, nil
		})
	gat.Register(api, g, huma.Operation{OperationID: "getCustomersById", Method: http.MethodGet, Path: "/customers"},
		func(ctx context.Context, in *customersInput) (*customersOutput, error) {
			mu.Lock()
			calls = append(calls, in.IDs)
			mu.Unlock()
			out := &customersOutput{}
			for _, id := range in.IDs {
				out.Body = append(out.Body, customer{ID: id, Name: "Customer " + id})
			}
			return out, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func postBatch(t *testing.T, url string, batch []map[string]any) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(batch)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch status %d", resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBatch_ExportFeedsTheNextOperation(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	out := postBatch(t, srv.URL+"/api/graphql", []map[string]any{
		{"query": `{ Shop { listOrders { id customerId @export(as: "ids") } } }`},
		{"query": `query($ids: [String!]) { Shop { getCustomersById(ids: $ids) { id name } } }`},
	})
	if len(out) != 2 {
		t.Fatalf("results = %d, want 2: %v", len(out), out)
	}
	if orders, _ := digPath(out[0], "data", "Shop", "listOrders").([]any); len(orders) != 4 {
		t.Errorf("first result: %v", out[0])
	}
	custs, _ := digPath(out[1], "data", "Shop", "getCustomersById").([]any)
	if len(custs) != 3 {
		t.Fatalf("second result: %v", out[1])
	}
	got := calls()
	if len(got) != 1 || strings.Join(got[0], ",") != "c1,c2,c3" {
		t.Errorf("getCustomersById called with %v, want one call with [c1 c2 c3] (deduplicated, in order)", got)
	}
}

func TestBatch_ExportThroughFragmentAndAlias(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	postBatch(t, srv.URL+"/api/graphql", []map[string]any{
		{"query": `{ Shop { os: listOrders { ...O } } } fragment O on Shop_Order { cid: customerId @export(as: "ids") }`},
		{"query": `query($ids: [String!]) { Shop { getCustomersById(ids: $ids) { id } } }`},
	})
	if got := calls(); len(got) != 1 || len(got[0]) != 3 {
		t.Errorf("calls = %v", got)
	}
}

func TestBatch_ClientVariableWinsOverExport(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	postBatch(t, srv.URL+"/api/graphql", []map[string]any{
		{"query": `{ Shop { listOrders { customerId @export(as: "ids") } } }`},
		{"query": `query($ids: [String!]) { Shop { getCustomersById(ids: $ids) { id } } }`, "variables": map[string]any{"ids": []string{"cX"}}},
	})
	if got := calls(); len(got) != 1 || strings.Join(got[0], ",") != "cX" {
		t.Errorf("calls = %v, want [[cX]]", got)
	}
}

func TestBatch_RefusedEntryDoesNotFailTheOthers(t *testing.T) {
	srv, _ := newShopGat(t, func(g *gat.Gateway) {
		g.TrustedDocuments(map[string]string{"orders": `{ Shop { listOrders { id } } }`}, true)
	})
	out := postBatch(t, srv.URL+"/api/graphql", []map[string]any{
		{"documentId": "orders"},
		{"query": `{ Shop { listOrders { id } } }`},
	})
	if len(out) != 2 || digPath(out[0], "data", "Shop", "listOrders") == nil {
		t.Fatalf("trusted entry: %v", out)
	}
	if errorCode(out[1]) != "TRUSTED_DOCUMENT_REQUIRED" {
		t.Errorf("query-text entry under enforce: %v", out[1])
	}
}

func TestBatch_OperationCap(t *testing.T) {
	srv, _ := newShopGat(t, func(g *gat.Gateway) { g.MaxOperations(2) })
	out := postBatch(t, srv.URL+"/api/graphql", []map[string]any{
		{"query": `{ Shop { a: listOrders { id } b: listOrders { id } } }`},
		{"query": `{ Shop { a: listOrders { id } b: listOrders { id } c: listOrders { id } } }`},
	})
	if out[0]["errors"] != nil {
		t.Errorf("document at the cap of 2: %v", out[0])
	}
	if errs, _ := out[1]["errors"].([]any); len(errs) != 1 || out[1]["data"] != nil {
		t.Errorf("document over the cap of 2 should be refused whole: %v", out[1])
	}
	b, _ := json.Marshal([]map[string]any{{"query": "{ __typename }"}, {"query": "{ __typename }"}, {"query": "{ __typename }"}})
	resp, err := http.Post(srv.URL+"/api/graphql", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("batch longer than MaxOperations: status %d, want 400", resp.StatusCode)
	}
}

func TestExportDirectiveInSDL(t *testing.T) {
	srv, _ := newShopGat(t, func(*gat.Gateway) {})
	if sdl := string(mustGET(t, srv.URL+"/api/schema/graphql")); !strings.Contains(sdl, "directive @export(as: String!) on FIELD") {
		t.Errorf("SDL lacks the @export declaration:\n%s", sdl)
	}
}
