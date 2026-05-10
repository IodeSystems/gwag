package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRuntimeChain_OpenAPIInjectPathFires registers an InjectPath
// rule against an openapi-ingested op, drives a GraphQL query, and
// checks that the upstream backend received the resolver-injected
// value for the hidden arg. Closes the cross-format runtime gap
// noted in plan §1 followups: the proto-shape Middleware chain now
// fires for openapi dispatchers via boundary conversion in
// `wrapCanonicalDispatcherWithChain`.
func TestRuntimeChain_OpenAPIInjectPathFires(t *testing.T) {
	called := 0
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)

	// Hide(false) so the arg stays on the external schema and the
	// dispatcher's args still carry whatever the resolver returns.
	// Hide(true) would also exercise the path but would test schema
	// rewrites alongside runtime — the unit under test here is
	// runtime injection.
	gw.Use(InjectPath("test.getThing.id", func(_ context.Context, current any) (any, error) {
		called++
		// Confirm the proto-shape chain saw the canonical-args input
		// — current should mirror the gql arg the caller sent.
		if got, _ := current.(string); got != "incoming" {
			t.Errorf("resolver got current=%v, want \"incoming\"", current)
		}
		return "rewritten", nil
	}, Hide(false)))

	var lastPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(backend.Close)

	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	h := gw.Handler()

	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ test { getThing(id:\"incoming\") { id } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env struct {
		Errors []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if len(env.Errors) > 0 {
		t.Fatalf("graphql errors: %s", rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("resolver called %d times, want 1", called)
	}
	if lastPath != "/things/rewritten" {
		t.Fatalf("backend path=%q want /things/rewritten (resolver injection didn't propagate)", lastPath)
	}
}

// TestRuntimeChain_GraphQLInjectPathFires is the graphql analogue:
// an InjectPath rule against a stitched graphql op fires before
// dispatch, the upstream graphql request reflects the injected
// arg.
func TestRuntimeChain_GraphQLInjectPathFires(t *testing.T) {
	called := 0
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)

	gw.Use(InjectPath("pets.user.id", func(_ context.Context, current any) (any, error) {
		called++
		if got, _ := current.(string); got != "incoming" {
			t.Errorf("resolver got current=%v, want \"incoming\"", current)
		}
		return "rewritten", nil
	}, Hide(false)))

	var lastVars map[string]any
	rf := newRemoteFixture(t)
	rf.queryHandler = func(_ string, vars map[string]any) any {
		lastVars = vars
		return map[string]any{
			"user": map[string]any{"id": "x", "name": "alice", "role": "ADMIN"},
		}
	}
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	h := gw.Handler()

	req := httptest.NewRequest(http.MethodPost, "/graphql",
		strings.NewReader(`{"query":"{ pets { user(id:\"incoming\") { id name role } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env struct {
		Errors []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if len(env.Errors) > 0 {
		t.Fatalf("graphql errors: %s", rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("resolver called %d times, want 1", called)
	}
	if got, _ := lastVars["id"].(string); got != "rewritten" {
		t.Fatalf("upstream vars[id]=%q want rewritten (resolver injection didn't propagate); got vars=%v", got, lastVars)
	}
}

// TestRuntimeChain_NoMiddlewareSkipsWrap is a guard against the
// boundary conversion firing when no Runtime middleware is
// registered — without it, every openapi/graphql dispatch would
// pay an unnecessary argsToMessage / messageToMap roundtrip.
func TestRuntimeChain_NoMiddlewareSkipsWrap(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)

	if gw.hasRuntimeMiddleware() {
		t.Fatal("fresh gateway reports runtime middleware registered")
	}

	// A Schema-only Transform (no Runtime half) shouldn't trigger
	// the wrap either.
	gw.Use(Transform{Schema: []SchemaRewrite{HideTypeRewrite{Name: "auth.v1.Context"}}})
	if gw.hasRuntimeMiddleware() {
		t.Fatal("schema-only Transform falsely flagged as runtime-bearing")
	}

	// Adding a Runtime-bearing Transform flips the flag.
	gw.Use(Transform{Runtime: func(next Handler) Handler { return next }})
	if !gw.hasRuntimeMiddleware() {
		t.Fatal("runtime-bearing Transform didn't flip the flag")
	}
}
