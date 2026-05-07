package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

// dynamicGraphQLBackend stands up a fake downstream GraphQL service
// that answers introspection with petsIntrospection (defined in
// graphql_ingest_test.go) and per-test query handler. Returns the
// httptest.Server and a counter of non-introspection queries.
func dynamicGraphQLBackend(t *testing.T, queryHandler func(query string, vars map[string]any) any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var queries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "IntrospectionQuery") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(petsIntrospection))
			return
		}
		queries.Add(1)
		var data any
		if queryHandler != nil {
			data = queryHandler(req.Query, req.Variables)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

// TestDynamicGraphQL_Standalone is the standalone-mode (no-cluster)
// happy path: control-plane sees a graphql_endpoint binding, the
// gateway introspects it at Register time, the schema rebuilds with
// the namespace-prefixed types, and a query through gw.Handler()
// forwards to the upstream.
func TestDynamicGraphQL_Standalone(t *testing.T) {
	backend, queries := dynamicGraphQLBackend(t, func(_ string, _ map[string]any) any {
		return map[string]any{
			"users": []map[string]any{{"id": "1", "name": "alice", "role": "ADMIN"}},
		}
	})

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)

	cp := gw.ControlPlane()
	resp, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr:       backend.URL, // ignored for graphql but valid
		InstanceId: "pets@1",
		Services: []*cpv1.ServiceBinding{{
			Namespace:       "pets",
			GraphqlEndpoint: backend.URL,
		}},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.GetRegistrationId() == "" {
		t.Fatal("empty registration id")
	}

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	got := postGraphQLForDynamic(t, srv.URL, `{"query":"{ pets_users { id name role } }"}`)
	if strings.Contains(got, "errors") || !strings.Contains(got, "alice") {
		t.Fatalf("unexpected response: %s", got)
	}
	if queries.Load() < 1 {
		t.Errorf("backend never received the query (count=%d)", queries.Load())
	}

	// Deregister → source removed → schema loses pets_users.
	if _, err := cp.Deregister(context.Background(), &cpv1.DeregisterRequest{
		RegistrationId: resp.GetRegistrationId(),
	}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	got = postGraphQLForDynamic(t, srv.URL, `{"query":"{ pets_users { id } }"}`)
	if !strings.Contains(got, "errors") || !strings.Contains(got, "pets_users") {
		t.Fatalf("expected pets_users to disappear, got: %s", got)
	}
}

func TestDynamicGraphQL_HashMismatchRejected(t *testing.T) {
	// First Register loads petsIntrospection. Second Register hits a
	// different backend that returns a tweaked introspection — the
	// hash differs, so the gateway must reject.
	backendA, _ := dynamicGraphQLBackend(t, nil)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()

	if _, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: backendA.URL,
		Services: []*cpv1.ServiceBinding{{
			Namespace:       "pets",
			GraphqlEndpoint: backendA.URL,
		}},
	}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Different backend that returns a slightly different
	// introspection (extra type) — different bytes → different hash.
	mutated := strings.Replace(petsIntrospection, `"MEMBER"`, `"VIEWER"`, 1)
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mutated))
	}))
	t.Cleanup(backendB.Close)

	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: backendB.URL,
		Services: []*cpv1.ServiceBinding{{
			Namespace:       "pets",
			GraphqlEndpoint: backendB.URL,
		}},
	})
	if err == nil {
		t.Fatal("expected hash-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "different schema hash") {
		t.Fatalf("error: %v (want 'different schema hash')", err)
	}
}

func TestDynamicGraphQL_AllThreeFormsRejected(t *testing.T) {
	// Setting graphql_endpoint together with file_descriptor_set OR
	// openapi_spec must fail.
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()
	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "http://x",
		Services: []*cpv1.ServiceBinding{{
			Namespace:         "weird",
			FileDescriptorSet: []byte("anything"),
			GraphqlEndpoint:   "http://x/graphql",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "may set only one of") {
		t.Fatalf("expected 'may set only one of', got %v", err)
	}
}

func TestDynamicGraphQL_NamespaceRequired(t *testing.T) {
	// graphql_endpoint with empty namespace is rejected — unlike the
	// proto and OpenAPI paths, there's no fallback (proto stems are
	// derived from the .proto filename; OpenAPI from Info.Title;
	// GraphQL has neither).
	backend, _ := dynamicGraphQLBackend(t, nil)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()
	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: backend.URL,
		Services: []*cpv1.ServiceBinding{{
			GraphqlEndpoint: backend.URL,
		}},
	})
	if err == nil {
		t.Fatal("expected error when namespace is empty")
	}
	if !strings.Contains(err.Error(), "graphql_endpoint binding requires explicit namespace") {
		t.Fatalf("error: %v", err)
	}
}

// TestDynamicGraphQL_CrossGatewayDispatch is the cluster equivalent
// of the standalone test: register on A, expect B's reconciler to
// pick up the introspection bytes from KV and create the source so a
// query through B forwards to the upstream.
func TestDynamicGraphQL_CrossGatewayDispatch(t *testing.T) {
	a, b := startTwoNodeCluster(t)

	backend, _ := dynamicGraphQLBackend(t, func(_ string, _ map[string]any) any {
		return map[string]any{
			"users": []map[string]any{{"id": "42", "name": "alice", "role": "ADMIN"}},
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := a.gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
			Addr: backend.URL,
			Services: []*cpv1.ServiceBinding{{
				Namespace:       "pets",
				GraphqlEndpoint: backend.URL,
			}},
		})
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("Register: %v", lastErr)
	}

	// Wait for B's reconciler to install the source.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b.gw.mu.Lock()
		_, ok := b.gw.graphQLSources["pets"]
		b.gw.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.gw.mu.Lock()
	_, ok := b.gw.graphQLSources["pets"]
	b.gw.mu.Unlock()
	if !ok {
		t.Fatal("pets graphQLSource never appeared on gateway B")
	}

	// Query through B forwards to backend.
	got := postGraphQLForDynamic(t, b.httpSrv.URL,
		`{"query":"{ pets_users { id name role } }"}`)
	if strings.Contains(got, "errors") || !strings.Contains(got, "alice") {
		t.Fatalf("response via B: %s", got)
	}
}
