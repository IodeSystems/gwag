package gateway

import (
	"context"
	"io"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// TestBuildOpenAPIRequest_NullParams: a null path or query argument
// sends no parameter (never "<nil>"), a null list item is skipped, and
// numbers have no exponent form.
func TestBuildOpenAPIRequest_NullParams(t *testing.T) {
	param := func(name, in string) *openapi3.ParameterRef {
		return &openapi3.ParameterRef{Value: &openapi3.Parameter{Name: name, In: in}}
	}
	op := &openapi3.Operation{Parameters: openapi3.Parameters{
		param("id", "path"), param("s", "query"), param("tags", "query"), param("f", "query"),
	}}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"null", map[string]any{"id": nil, "s": nil, "tags": nil, "f": nil}, "http://u/p/{id}"},
		{"absent", map[string]any{}, "http://u/p/{id}"},
		{"list-null-item", map[string]any{"id": "x", "tags": []any{"a", nil, "b"}}, "http://u/p/x?tags=a&tags=b"},
		{"number", map[string]any{"id": float64(3e9), "f": 2.5}, "http://u/p/3000000000?f=2.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, full, ups, err := buildOpenAPIRequest(context.Background(), "GET", "http://u", "/p/{id}", op, c.args, nil, nil, nil, "application/json")
			if err != nil {
				t.Fatal(err)
			}
			closeUploads(ups)
			if full != c.want {
				t.Fatalf("url = %s, want %s", full, c.want)
			}
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				if len(b) != 0 {
					t.Fatalf("body = %s, want none", b)
				}
			}
		})
	}
}
