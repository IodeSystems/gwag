package gat_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

func TestStaged_ExportFeedsASiblingInOneDocument(t *testing.T) {
	for _, parallel := range []int{0, 8} {
		srv, calls := newShopGat(t, func(g *gat.Gateway) { g.ParallelQueries(parallel) })
		resp := mustGraphQL(t, srv.URL+"/api/graphql", `query ($ids: [String!]) {
			Shop {
				listOrders { id customerId @export(as: "ids") }
				getCustomersById(ids: $ids) { id name }
			}
		}`)
		if orders, _ := digPath(resp, "data", "Shop", "listOrders").([]any); len(orders) != 4 {
			t.Errorf("parallel=%d: orders %v", parallel, resp)
		}
		if custs, _ := digPath(resp, "data", "Shop", "getCustomersById").([]any); len(custs) != 3 {
			t.Errorf("parallel=%d: customers %v", parallel, resp)
		}
		if got := calls(); len(got) != 1 || strings.Join(got[0], ",") != "c1,c2,c3" {
			t.Errorf("parallel=%d: lookups %v, want one with [c1 c2 c3]", parallel, got)
		}
	}
}

func TestStaged_ChainsAndBranches(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	// Branch 1: orders → customers → customers again (a 3-stage chain).
	// Branch 2: an independent lookup in stage 1.
	resp := mustGraphQL(t, srv.URL+"/api/graphql", `query ($ids: [String!], $again: [String!], $fixed: [String!] = ["cZ"]) {
		Shop {
			listOrders { customerId @export(as: "ids") }
			first: getCustomersById(ids: $ids) { id @export(as: "again") }
			second: getCustomersById(ids: $again) { id name }
			other: getCustomersById(ids: $fixed) { id }
		}
	}`)
	for _, k := range []string{"listOrders", "first", "second", "other"} {
		if digPath(resp, "data", "Shop", k) == nil {
			t.Errorf("missing %s: %v", k, resp)
		}
	}
	if resp["errors"] != nil {
		t.Errorf("errors: %v", resp["errors"])
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("lookups %v, want 3 (other, first, second)", got)
	}
	if strings.Join(got[len(got)-1], ",") != "c1,c2,c3" {
		t.Errorf("third stage got %v, want the ids the second stage exported", got[len(got)-1])
	}
}

func TestStaged_FragmentsAndClientVariableWins(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	mustGraphQL(t, srv.URL+"/api/graphql", `query ($ids: [String!]) {
		Shop { ...Orders getCustomersById(ids: $ids) { id } }
	}
	fragment Orders on ShopQueryNamespace { listOrders { customerId @export(as: "ids") } }`)
	if got := calls(); len(got) != 1 || len(got[0]) != 3 {
		t.Errorf("through a fragment: lookups %v", got)
	}

	srv2, calls2 := newShopGat(t, func(*gat.Gateway) {})
	b := postGraphQLVars(t, srv2.URL+"/api/graphql", `query ($ids: [String!]) {
		Shop { listOrders { customerId @export(as: "ids") } getCustomersById(ids: $ids) { id } }
	}`, map[string]any{"ids": []string{"cX"}})
	if got := calls2(); len(got) != 1 || strings.Join(got[0], ",") != "cX" {
		t.Errorf("client variable should win: lookups %v (%v)", got, b)
	}
}

func TestStaged_CycleIsRefused(t *testing.T) {
	srv, calls := newShopGat(t, func(*gat.Gateway) {})
	resp := mustGraphQL(t, srv.URL+"/api/graphql", `query ($a: [String!], $b: [String!]) {
		Shop {
			x: getCustomersById(ids: $a) { id @export(as: "b") }
			y: getCustomersById(ids: $b) { id @export(as: "a") }
		}
	}`)
	if errorCode(resp) != "EXPORT_DEPENDENCY" {
		t.Errorf("cycle: %v", resp)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("%d lookups ran for a refused document", n)
	}
}

func TestStaged_UnreadExportRunsInOnePass(t *testing.T) {
	srv, _ := newShopGat(t, func(*gat.Gateway) {})
	resp := mustGraphQL(t, srv.URL+"/api/graphql", `{ Shop { listOrders { customerId @export(as: "ids") } } }`)
	if orders, _ := digPath(resp, "data", "Shop", "listOrders").([]any); len(orders) != 4 || resp["errors"] != nil {
		t.Errorf("single-stage document: %v", resp)
	}
}

func postGraphQLVars(t *testing.T, url, query string, vars map[string]any) map[string]any {
	t.Helper()
	_, resp := postJSON(t, url, map[string]any{"query": query, "variables": vars})
	return resp
}

func TestStaged_ResponseKeepsQueryFieldOrder(t *testing.T) {
	srv, _ := newShopGat(t, func(*gat.Gateway) {})
	_, raw := postRaw(t, srv.URL+"/api/graphql", `query ($ids: [String!]) {
		Shop {
			getCustomersById(ids: $ids) { name id }
			listOrders { customerId @export(as: "ids") id }
		}
	}`)
	want := `{"data":{"Shop":{"getCustomersById":[{"name":"Customer c1","id":"c1"}`
	if !strings.HasPrefix(raw, want) {
		t.Errorf("response %s\nwant prefix %s", raw, want)
	}
	if i, j := strings.Index(raw, `"customerId":"c1","id":"o1"`), strings.Index(raw, `"listOrders"`); i < 0 || j < 0 {
		t.Errorf("orders not in query field order: %s", raw)
	}
}

func postRaw(t *testing.T, url, query string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"query": query})
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
