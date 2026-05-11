package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gatewayWithMinimalOpenAPI returns a gateway with one OpenAPI source
// already registered, so g.Handler() can render a populated schema.
// Callers add Options up front via opts.
func gatewayWithMinimalOpenAPI(t *testing.T, opts ...Option) (*Gateway, http.Handler) {
	t.Helper()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(be.Close)

	all := append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored"))}, opts...)
	gw := New(all...)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	return gw, gw.Handler()
}

// TestGraphiQLHandlerCached confirms the cached graphiql handler is
// reused (not rebuilt per request) and serves HTML to browser-shaped
// requests.
func TestGraphiQLHandlerCached(t *testing.T) {
	gw, h := gatewayWithMinimalOpenAPI(t)

	first := gw.graphiqlHandler.Load()
	if first == nil {
		t.Fatal("graphiqlHandler was nil after assemble")
	}

	// Two browser-shaped requests → both should render the UI without
	// constructing a new handler.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
		req.Header.Set("Accept", "text/html")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "graphiql") && !strings.Contains(rr.Body.String(), "GraphiQL") {
			t.Fatalf("iter %d: response did not look like GraphiQL HTML; body=%s", i, rr.Body.String())
		}
	}

	if got := gw.graphiqlHandler.Load(); got != first {
		t.Fatalf("graphiqlHandler pointer changed across requests: %p → %p", first, got)
	}
}

// TestWithoutGraphiQL_FallsThroughToJSON confirms the option drops the
// cached handler and routes browser-shaped requests through the JSON
// path (response is JSON, not HTML).
func TestWithoutGraphiQL_FallsThroughToJSON(t *testing.T) {
	gw, h := gatewayWithMinimalOpenAPI(t, WithoutGraphiQL())

	if got := gw.graphiqlHandler.Load(); got != nil {
		t.Fatalf("graphiqlHandler should be nil with WithoutGraphiQL(); got %p", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type=%q, want application/json (UI should be off)", ct)
	}
	if strings.Contains(rr.Body.String(), "<html") || strings.Contains(rr.Body.String(), "GraphiQL") {
		t.Fatalf("response looked like GraphiQL HTML even with WithoutGraphiQL(); body=%s", rr.Body.String())
	}
}

// TestGraphQLResponseCompact confirms the hot path (plan branch) emits
// compact JSON bytes directly via ExecutePlanAppend — no encoder, no
// SetIndent, no trailing newline. The append-mode swap dropped the
// previous pretty-JSON default; operators wanting indented bodies can
// pipe through `jq` etc.
func TestGraphQLResponseCompact(t *testing.T) {
	_, h := gatewayWithMinimalOpenAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { getThing(id:\"1\") { id } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "\n\t") || strings.Contains(body, "\n ") {
		t.Fatalf("plan-branch response should be compact (no indentation):\n%s", body)
	}
}

// TestGraphiQLHandlerRebuiltOnSchemaRebuild confirms the cached handler
// pointer is replaced when the schema is re-assembled (e.g. a new
// service registers).
func TestGraphiQLHandlerRebuiltOnSchemaRebuild(t *testing.T) {
	gw, _ := gatewayWithMinimalOpenAPI(t)

	first := gw.graphiqlHandler.Load()
	if first == nil {
		t.Fatal("graphiqlHandler was nil after first assemble")
	}

	// Trigger a second assembleLocked by registering another OpenAPI
	// source; that path calls assembleLocked on success.
	be2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","name":"y"}`))
	}))
	t.Cleanup(be2.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be2.URL), As("test2")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	second := gw.graphiqlHandler.Load()
	if second == nil {
		t.Fatal("graphiqlHandler was nil after rebuild")
	}
	if second == first {
		t.Fatalf("graphiqlHandler pointer did not advance across rebuild: %p", first)
	}
}
