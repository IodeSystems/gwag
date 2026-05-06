package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// openAPIE2EFixture spins a test backend + a Gateway that ingests a
// minimal OpenAPI spec pointing at it. Returns the gateway HTTP
// handler (post /graphql to it) and a function for reading what the
// backend last saw.
type openAPIE2EFixture struct {
	gw           *Gateway
	graphql      http.Handler
	backend      *httptest.Server
	lastReq      atomic.Pointer[capturedRequest]
	backendReply func(http.ResponseWriter, *http.Request)
}

type capturedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

const minimalOpenAPISpec = `{
  "openapi": "3.0.0",
  "info": {"title": "test", "version": "1.0.0"},
  "paths": {
    "/things/{id}": {
      "get": {
        "operationId": "getThing",
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
                    "id":   {"type": "string"},
                    "name": {"type": "string"}
                  }
                }
              }
            }
          }
        }
      }
    },
    "/things": {
      "post": {
        "operationId": "createThing",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "name": {"type": "string"}
                }
              }
            }
          }
        },
        "responses": {
          "200": {
            "description": "ok",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id":   {"type": "string"},
                    "name": {"type": "string"}
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

func newOpenAPIE2EFixture(t *testing.T, opts ...ServiceOption) *openAPIE2EFixture {
	t.Helper()
	f := &openAPIE2EFixture{}
	f.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.lastReq.Store(&capturedRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Headers: r.Header.Clone(),
			Body:    body,
		})
		if f.backendReply != nil {
			f.backendReply(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(f.backend.Close)

	f.gw = New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(f.gw.Close)

	allOpts := append([]ServiceOption{To(f.backend.URL), As("test")}, opts...)
	if err := f.gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), allOpts...); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	f.graphql = f.gw.Handler()
	return f
}

func (f *openAPIE2EFixture) postGraphQL(t *testing.T, query string, headers map[string]string) (status int, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(query))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	f.graphql.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestOpenAPIE2E_Get(t *testing.T) {
	f := newOpenAPIE2EFixture(t)
	status, body := f.postGraphQL(t, `{"query":"{ test_getThing(id:\"42\") { id name } }"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}

	var out struct {
		Data struct {
			TestGetThing map[string]any `json:"test_getThing"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %v", out.Errors)
	}
	if got := out.Data.TestGetThing["id"]; got != "abc" {
		t.Fatalf("id=%v want abc", got)
	}

	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Method != http.MethodGet || rec.Path != "/things/42" {
		t.Fatalf("backend got %s %s, want GET /things/42", rec.Method, rec.Path)
	}
}

func TestOpenAPIE2E_PostBody(t *testing.T) {
	f := newOpenAPIE2EFixture(t)
	q := `{"query":"mutation { test_createThing(body:{name:\"widget\"}) { id name } }"}`
	status, body := f.postGraphQL(t, q, nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if rec.Method != http.MethodPost || rec.Path != "/things" {
		t.Fatalf("backend got %s %s, want POST /things", rec.Method, rec.Path)
	}
	var bodyJSON map[string]any
	if err := json.Unmarshal(rec.Body, &bodyJSON); err != nil {
		t.Fatalf("backend body not json: %v: %s", err, rec.Body)
	}
	if bodyJSON["name"] != "widget" {
		t.Fatalf("backend body name=%v want widget", bodyJSON["name"])
	}
}

func TestOpenAPIE2E_AuthorizationForwarded(t *testing.T) {
	f := newOpenAPIE2EFixture(t)
	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	_, _ = f.postGraphQL(t, q, map[string]string{
		"Authorization": "Bearer hunter2",
		"X-Other":       "leaked?",
	})
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if got := rec.Headers.Get("Authorization"); got != "Bearer hunter2" {
		t.Errorf("Authorization not forwarded, got %q", got)
	}
	if got := rec.Headers.Get("X-Other"); got != "" {
		t.Errorf("X-Other should not leak by default, got %q", got)
	}
}

func TestOpenAPIE2E_ForwardHeadersAllowlist(t *testing.T) {
	f := newOpenAPIE2EFixture(t, ForwardHeaders("X-Api-Key"))
	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	_, _ = f.postGraphQL(t, q, map[string]string{
		"Authorization": "Bearer hunter2",
		"X-Api-Key":     "k1",
	})
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if got := rec.Headers.Get("X-Api-Key"); got != "k1" {
		t.Errorf("X-Api-Key not forwarded, got %q", got)
	}
	if got := rec.Headers.Get("Authorization"); got != "" {
		t.Errorf("custom allowlist must drop Authorization, got %q", got)
	}
}

func TestOpenAPIE2E_BackendErrorSurfaces(t *testing.T) {
	f := newOpenAPIE2EFixture(t)
	f.backendReply = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}
	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	status, body := f.postGraphQL(t, q, nil)
	if status != http.StatusOK {
		t.Fatalf("graphql transport status = %d, want 200", status)
	}
	if !strings.Contains(body, "errors") || !strings.Contains(body, "500") {
		t.Fatalf("expected backend 500 surfaced as graphql error, got %s", body)
	}
}
