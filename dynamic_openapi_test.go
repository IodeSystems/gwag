package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

const dynamicOpenAPISpec = `{
  "openapi": "3.0.0",
  "info": {"title": "billing", "version": "1.0.0"},
  "paths": {
    "/invoices/{id}": {
      "get": {
        "operationId": "getInvoice",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}
        ],
        "responses": {
          "200": {
            "description": "ok",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id":     {"type": "string"},
                    "amount": {"type": "number"}
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// TestDynamicOpenAPI_Standalone verifies the standalone-mode
// (no-cluster) Register path: control-plane sees an openapi_spec
// binding, addOpenAPISourceLocked creates the source, the schema
// rebuilds, and a query through gw.Handler() dispatches HTTP to the
// registered base URL.
func TestDynamicOpenAPI_Standalone(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invoices/INV-1" {
			http.Error(w, "wrong path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"INV-1","amount":42.5}`))
	}))
	t.Cleanup(backend.Close)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)

	cp := gw.ControlPlane()
	resp, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr:       backend.URL,
		InstanceId: "billing@1",
		Services: []*cpv1.ServiceBinding{{
			Namespace:   "billing",
			OpenapiSpec: []byte(dynamicOpenAPISpec),
		}},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.GetRegistrationId() == "" {
		t.Fatal("empty registration id")
	}

	// Force schema assembly + query via Handler.
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	got := postGraphQLForDynamic(t, srv.URL,
		`{"query":"{ billing_getInvoice(id:\"INV-1\") { id amount } }"}`)
	if !strings.Contains(got, `INV-1`) || !strings.Contains(got, `42.5`) || strings.Contains(got, `errors`) {
		t.Fatalf("unexpected response: %s", got)
	}

	// Deregister and confirm the source disappears.
	if _, err := cp.Deregister(context.Background(), &cpv1.DeregisterRequest{
		RegistrationId: resp.GetRegistrationId(),
	}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	got = postGraphQLForDynamic(t, srv.URL,
		`{"query":"{ billing_getInvoice(id:\"INV-1\") { id } }"}`)
	if !strings.Contains(got, "errors") || !strings.Contains(got, "billing_getInvoice") {
		t.Fatalf("expected schema to lose billing_getInvoice after deregister, got: %s", got)
	}
}

func TestDynamicOpenAPI_HashMismatchRejected(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()

	if _, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "http://a.example/api",
		Services: []*cpv1.ServiceBinding{{
			Namespace:   "billing",
			OpenapiSpec: []byte(dynamicOpenAPISpec),
		}},
	}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Mutate the spec slightly so the hash differs.
	mutated := strings.Replace(dynamicOpenAPISpec, "1.0.0", "1.0.1", 1)
	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "http://b.example/api",
		Services: []*cpv1.ServiceBinding{{
			Namespace:   "billing",
			OpenapiSpec: []byte(mutated),
		}},
	})
	if err == nil {
		t.Fatal("expected hash-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "different spec hash") {
		t.Fatalf("error: %v (want 'different spec hash')", err)
	}
}

func TestDynamicOpenAPI_BothSpecAndDescriptorRejected(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()

	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "http://x/",
		Services: []*cpv1.ServiceBinding{{
			Namespace:         "weird",
			FileDescriptorSet: []byte("anything"),
			OpenapiSpec:       []byte(dynamicOpenAPISpec),
		}},
	})
	if err == nil {
		t.Fatal("expected error when both descriptor and spec set")
	}
	if !strings.Contains(err.Error(), "cannot set both") {
		t.Fatalf("error: %v", err)
	}
}

func TestDynamicOpenAPI_NeitherSpecNorDescriptorRejected(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	cp := gw.ControlPlane()

	_, err := cp.Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "http://x/",
		Services: []*cpv1.ServiceBinding{{
			Namespace: "weird",
		}},
	})
	if err == nil {
		t.Fatal("expected error when neither set")
	}
	if !strings.Contains(err.Error(), "must set") {
		t.Fatalf("error: %v", err)
	}
}

// TestDynamicOpenAPI_CrossGatewayDispatch verifies an OpenAPI spec
// registered on gateway A becomes a GraphQL field on gateway B via
// the registry KV reconciler — the cluster equivalent of the proto
// cross-gateway test.
func TestDynamicOpenAPI_CrossGatewayDispatch(t *testing.T) {
	a, b := startTwoNodeCluster(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"INV-1","amount":99}`))
	}))
	t.Cleanup(backend.Close)

	// Register on A.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := a.gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
			Addr: backend.URL,
			Services: []*cpv1.ServiceBinding{{
				Namespace:   "billing",
				OpenapiSpec: []byte(dynamicOpenAPISpec),
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

	// Wait for B's reconciler to install the openAPISource locally.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b.gw.mu.Lock()
		_, ok := b.gw.openAPISources["billing"]
		b.gw.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.gw.mu.Lock()
	_, ok := b.gw.openAPISources["billing"]
	b.gw.mu.Unlock()
	if !ok {
		t.Fatal("billing openAPISource never appeared on gateway B")
	}

	// Query through B → dispatches HTTP to backend.
	got := postGraphQLForDynamic(t, b.httpSrv.URL,
		`{"query":"{ billing_getInvoice(id:\"INV-1\") { id amount } }"}`)
	if strings.Contains(got, "errors") || !strings.Contains(got, "INV-1") {
		t.Fatalf("response via B: %s", got)
	}
}

// postGraphQLForDynamic posts a GraphQL request to srv.URL+/graphql
// and returns the body as a string. Helper deduped from existing
// fixtures to keep this file self-contained.
func postGraphQLForDynamic(t *testing.T, srvURL, query string) string {
	t.Helper()
	resp, err := http.Post(srvURL+"/graphql", "application/json", strings.NewReader(query))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body)
}

// silence
var _ = json.Marshal
var _ = time.Second
