package gat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type fieldsItem struct {
	Name string `json:"name" minLength:"2"`
}

type fieldsInput struct {
	Limit int `query:"limit" minimum:"1"`
	Body  struct {
		Name  string       `json:"name" minLength:"3"`
		Code  string       `json:"code" pattern:"^[a-z]+$"`
		Inner *fieldsItem  `json:"inner,omitempty"`
		Items []fieldsItem `json:"items,omitempty"`
	}
}

// Pins: a failed huma input check reaches GraphQL as
// extensions.fields [{path, message}], path in GraphQL argument terms
// (the body arg is `body`; query/path/header params keep their bare
// name; list indexes are dot segments), alongside status/code.
func TestGraphQL_ValidationFields(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "makeThing", Method: http.MethodPost, Path: "/things"},
		func(ctx context.Context, in *fieldsInput) (*checkedOutput, error) {
			return &checkedOutput{}, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"body_field", `limit: 1, body: {name: "ab", code: "abc"}`, []string{"body.name"}},
		{"pattern", `limit: 1, body: {name: "abc", code: "BAD"}`, []string{"body.code"}},
		{"nested", `limit: 1, body: {name: "abc", code: "abc", inner: {name: "x"}}`, []string{"body.inner.name"}},
		{"list_item", `limit: 1, body: {name: "abc", code: "abc", items: [{name: "ok"}, {name: "x"}]}`, []string{"body.items.1.name"}},
		{"query_param", `limit: 0, body: {name: "abc", code: "abc"}`, []string{"limit"}},
		{"multiple", `limit: 0, body: {name: "a", code: "abc"}`, []string{"limit", "body.name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := graphQLAs(t, srv.URL+"/api/graphql", "", `mutation { Demo { makeThing(`+tc.args+`) { v } } }`)
			errs, _ := resp["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("want 1 error, got %v", resp)
			}
			ext := errs[0].(map[string]any)["extensions"].(map[string]any)
			if ext["status"] != float64(422) || ext["code"] == nil {
				t.Errorf("status/code changed: %v", ext)
			}
			raw, _ := ext["fields"].([]any)
			var paths []string
			for _, f := range raw {
				fm := f.(map[string]any)
				paths = append(paths, fm["path"].(string))
				if m, _ := fm["message"].(string); m == "" {
					t.Errorf("field %v has no message", fm)
				}
			}
			if !reflect.DeepEqual(paths, tc.want) {
				t.Errorf("fields paths = %v, want %v (ext %v)", paths, tc.want, ext)
			}
		})
	}

	t.Run("no_details_no_fields", func(t *testing.T) {
		mux := http.NewServeMux()
		api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
		g := mustNewGat(t)
		gat.Register(api, g, huma.Operation{OperationID: "nope", Method: http.MethodGet, Path: "/nope"},
			func(ctx context.Context, in *struct{}) (*checkedOutput, error) {
				return nil, huma.Error403Forbidden("no")
			})
		if err := gat.RegisterHuma(api, g, "/api"); err != nil {
			t.Fatal(err)
		}
		s := httptest.NewServer(mux)
		defer s.Close()
		resp := graphQLAs(t, s.URL+"/api/graphql", "", `{ Demo { nope { v } } }`)
		ext := resp["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
		if _, has := ext["fields"]; has {
			t.Errorf("unexpected fields key: %v", ext)
		}
	})
}
