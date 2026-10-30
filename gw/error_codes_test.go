package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type gqlErrorBody struct {
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func postGraphQLQuery(t *testing.T, h http.Handler, query string) gqlErrorBody {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body gqlErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("no errors in %s", rr.Body.String())
	}
	return body
}

// An upstream error status reaches GraphQL as Reject's upper-case Code
// by default, and as gat's {status, code} shape (lower-case Connect
// code, the one the gRPC and REST ingresses use) under
// WithConnectErrorCodes.
func TestConnectErrorCodes_UpstreamStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var code int
		_, _ = fmt.Sscanf(r.URL.Path, "/things/%d", &code)
		w.WriteHeader(code)
	}))
	defer backend.Close()
	for _, tc := range []struct {
		name     string
		opts     []Option
		upstream int
		want     map[string]any
	}{
		{"default 404", nil, 404, map[string]any{"code": "NOT_FOUND"}},
		{"default 409", nil, 409, map[string]any{"code": "INVALID_ARGUMENT"}},
		{"connect 404", []Option{WithConnectErrorCodes()}, 404, map[string]any{"code": "not_found", "status": float64(404)}},
		{"connect 409", []Option{WithConnectErrorCodes()}, 409, map[string]any{"code": "already_exists", "status": float64(409)}},
		{"connect 503", []Option{WithConnectErrorCodes()}, 503, map[string]any{"code": "unavailable", "status": float64(503)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test"))}, tc.opts...)...)
			defer gw.Close()
			if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
				t.Fatal(err)
			}
			// Both resolver paths: id + name (append) and __typename.
			for _, q := range []string{
				fmt.Sprintf(`{ test { getThing(id:"%d") { id name } } }`, tc.upstream),
				fmt.Sprintf(`{ test { getThing(id:"%d") { __typename id } } }`, tc.upstream),
			} {
				got := postGraphQLQuery(t, gw.Handler(), q).Errors[0].Extensions
				if fmt.Sprint(got) != fmt.Sprint(tc.want) {
					t.Errorf("%s: extensions %v, want %v", q, got, tc.want)
				}
			}
		})
	}
}
