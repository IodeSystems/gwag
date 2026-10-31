package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSchemaOpenAPIHandler_Version(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []Option
		query string
		want  string
		code  int
	}{
		{"default", nil, "", `"openapi": "3.0.0"`, http.StatusOK},
		{"query31", nil, "?openapi=3.1", `"openapi": "3.1.0"`, http.StatusOK},
		{"option", []Option{WithOpenAPI31()}, "", `"openapi": "3.1.0"`, http.StatusOK},
		{"option_query30", []Option{WithOpenAPI31()}, "?openapi=3.0", `"openapi": "3.0.0"`, http.StatusOK},
		{"bad", nil, "?openapi=2.0", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored"))}, tc.opts...)
			gw := New(opts...)
			t.Cleanup(gw.Close)
			if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To("http://127.0.0.1:1"), As("test")); err != nil {
				t.Fatalf("AddOpenAPIBytes: %v", err)
			}
			rr := httptest.NewRecorder()
			gw.SchemaOpenAPIHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/schema/openapi"+tc.query, nil))
			if rr.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rr.Code, tc.code, rr.Body.String())
			}
			if tc.want != "" && !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("want %s, got %.300s", tc.want, rr.Body.String())
			}
		})
	}
}
