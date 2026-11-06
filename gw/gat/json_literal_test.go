package gat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// An inline object literal for a JSON-scalar argument reaches the
// handler as plain values. It used to reach the binder as GraphQL AST
// nodes and fail to unmarshal. A variable nested inside the literal
// cannot be resolved, so it is refused at validation.
func TestJSONScalarInlineLiteral(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), humago.NewAdapter(mux, ""))
	g := mustNewGat(t)
	var seen map[string]map[string]int
	gat.Register(api, g, huma.Operation{OperationID: "putGrid", Method: http.MethodPost, Path: "/grids"},
		func(ctx context.Context, in *nestedGridIn) (*nestedGridOut, error) {
			seen = in.Body.Cells
			return &nestedGridOut{Body: in.Body}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := mustGraphQL(t, srv.URL+"/api/graphql",
		`mutation { Demo { putGrid(body: {name: "q", cells: {a: {x: 1, y: 9007199254740993}}}) { cells } } }`)
	if want := map[string]map[string]int{"a": {"x": 1, "y": 9007199254740993}}; !reflect.DeepEqual(seen, want) {
		t.Errorf("handler saw %v, want %v (%v)", seen, want, resp)
	}

	payload, _ := json.Marshal(map[string]any{
		"query":     `mutation($y: Int) { Demo { putGrid(body: {name: "q", cells: {a: {y: $y}}}) { cells } } }`,
		"variables": map[string]any{"y": 2},
	})
	hr, err := http.Post(srv.URL+"/api/graphql", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(hr.Body)
	hr.Body.Close()
	if !strings.Contains(string(rb), "invalid value") {
		t.Errorf("nested variable: %s, want a validation error", rb)
	}
}
