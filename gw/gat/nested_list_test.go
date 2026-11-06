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

type nestedRegion struct {
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"`
}

type nestedRegionIn struct{ Body nestedRegion }
type nestedRegionOut struct{ Body nestedRegion }

// A Go [][2]float64 is an OpenAPI array of array. It must render as a
// nested GraphQL list and round-trip through a mutation; it used to
// flatten to [Float]!, which refused [[1,2]] at validation.
func TestNestedListRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	api := huma.NewAPI(huma.DefaultConfig("Demo", "1.0.0"), humago.NewAdapter(mux, ""))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "putRegion", Method: http.MethodPost, Path: "/regions"},
		func(ctx context.Context, in *nestedRegionIn) (*nestedRegionOut, error) {
			return &nestedRegionOut{Body: in.Body}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	// Output: required lists coerce nil → []; huma marks the slices
	// nullable (["array","null"]), so the input side stays nullable.
	for _, want := range []string{"points: [[Float!]]!", "points: [[Float!]]"} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}
	resp := mustGraphQL(t, srv.URL+"/api/graphql",
		`mutation { Demo { putRegion(body: {name: "a", points: [[1, 2], [3.5, 4]]}) { name points } } }`)
	got := digPath(resp, "data", "Demo", "putRegion", "points")
	rows, ok := got.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("points = %v: %v", got, resp)
	}
	if r, ok := rows[1].([]any); !ok || len(r) != 2 || r[0] != 3.5 || r[1] != 4.0 {
		t.Errorf("points[1] = %v", rows[1])
	}
}
