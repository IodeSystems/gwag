package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	return newOpenAPIE2EFixtureWithGatewayClient(t, nil, opts...)
}

// newOpenAPIE2EFixtureWithGatewayClient mirrors newOpenAPIE2EFixture
// but lets the test pin the gateway-wide *http.Client used by every
// OpenAPI dispatch (via WithOpenAPIClient). Per-source overrides
// from `opts` still beat the gateway-wide default.
func newOpenAPIE2EFixtureWithGatewayClient(t *testing.T, client *http.Client, opts ...ServiceOption) *openAPIE2EFixture {
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

	gwOpts := []Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token"))}
	if client != nil {
		gwOpts = append(gwOpts, WithOpenAPIClient(client))
	}
	f.gw = New(gwOpts...)
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

// markingTransport wraps an inner RoundTripper and stamps a header
// onto every outbound request so tests can verify which transport
// actually carried the dispatch.
type markingTransport struct {
	inner       http.RoundTripper
	markHeader  string
	markValue   string
	roundTrips  *atomic.Int32
	lastSubject *atomic.Pointer[capturedRequest]
}

func (m *markingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.roundTrips != nil {
		m.roundTrips.Add(1)
	}
	if m.markHeader != "" {
		req.Header.Set(m.markHeader, m.markValue)
	}
	inner := m.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(req)
}

func TestOpenAPIE2E_HTTPClient_GatewayWideDefault(t *testing.T) {
	// WithOpenAPIClient sets the default client every OpenAPI source
	// uses unless it overrides per-source. The marking transport
	// stamps "X-Via: gw-default", which the test backend echoes back
	// via lastReq.Headers.
	f := newOpenAPIE2EFixtureWithGatewayClient(t, &http.Client{
		Transport: &markingTransport{markHeader: "X-Via", markValue: "gw-default"},
	})

	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	_, _ = f.postGraphQL(t, q, nil)
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if got := rec.Headers.Get("X-Via"); got != "gw-default" {
		t.Errorf("X-Via = %q, want gw-default (gateway-wide client should have run)", got)
	}
}

func TestOpenAPIE2E_HTTPClient_PerSourceBeatsGatewayWide(t *testing.T) {
	// Per-source OpenAPIClient overrides the gateway-wide default —
	// confirm the stamp comes from the per-source client.
	f := newOpenAPIE2EFixtureWithGatewayClient(t,
		&http.Client{Transport: &markingTransport{markHeader: "X-Via", markValue: "gw-default"}},
		OpenAPIClient(&http.Client{Transport: &markingTransport{markHeader: "X-Via", markValue: "per-source"}}),
	)

	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	_, _ = f.postGraphQL(t, q, nil)
	rec := f.lastReq.Load()
	if rec == nil {
		t.Fatal("backend not called")
	}
	if got := rec.Headers.Get("X-Via"); got != "per-source" {
		t.Errorf("X-Via = %q, want per-source", got)
	}
}

func TestOpenAPIE2E_HTTPClient_NilFallsBackToDefault(t *testing.T) {
	// No WithOpenAPIClient + no OpenAPIClient → http.DefaultClient.
	// We can't easily probe the default client, but we can confirm
	// the request reached the backend (no transport rewriting / drops).
	f := newOpenAPIE2EFixture(t)
	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	status, body := f.postGraphQL(t, q, nil)
	if status != http.StatusOK || strings.Contains(body, "errors") {
		t.Fatalf("default client path failed: status=%d body=%s", status, body)
	}
	if f.lastReq.Load() == nil {
		t.Fatal("backend not called via http.DefaultClient")
	}
}

// openAPIBackpressureMetrics tallies dwell + backoff calls so the
// backpressure test can confirm the per-source semaphore actually
// fired.
type openAPIBackpressureMetrics struct {
	noopMetrics
	mu          sync.Mutex
	backoff     int
	dwellHit    int
	dispatch    int
	dispatchErr int
}

func (m *openAPIBackpressureMetrics) RecordDwell(_, _, _, kind string, _ time.Duration) {
	if kind != "unary" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dwellHit++
}

func (m *openAPIBackpressureMetrics) RecordBackoff(_, _, _, _, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backoff++
}

func (m *openAPIBackpressureMetrics) RecordDispatch(_, _, _ string, _ time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatch++
	if err != nil {
		m.dispatchErr++
	}
}

func (m *openAPIBackpressureMetrics) SetQueueDepth(_, _, _ string, _ int) {}

func TestOpenAPIE2E_BackpressureTimesOutAndRejects(t *testing.T) {
	// One backend slot held by a long-running request; with MaxInflight=1
	// and MaxWaitTime=50ms a concurrent dispatch should reject with
	// RESOURCE_EXHAUSTED rather than queueing forever.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	requestArrived := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case requestArrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	// Release before backend.Close so the still-blocked first request
	// returns and httptest.Server.Close doesn't stall on the active
	// connection.
	t.Cleanup(backend.Close)
	t.Cleanup(closeRelease)

	cm := &openAPIBackpressureMetrics{}
	gw := New(
		WithMetrics(cm),
		WithBackpressure(BackpressureOptions{MaxInflight: 1, MaxWaitTime: 50 * time.Millisecond}),
		WithAdminToken([]byte("test-token")),
	)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	h := gw.Handler()

	postQuery := func(q string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(q))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	q := `{"query":"{ test_getThing(id:\"1\") { id } }"}`
	holder := make(chan string, 1)
	go func() {
		_, body := postQuery(q)
		holder <- body
	}()

	// Wait until the first request reached the backend (and is holding
	// the slot) before firing the second.
	select {
	case <-requestArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached backend")
	}

	status, body := postQuery(q)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, "RESOURCE_EXHAUSTED") {
		t.Errorf("expected RESOURCE_EXHAUSTED, got %s", body)
	}

	cm.mu.Lock()
	backoff := cm.backoff
	dwell := cm.dwellHit
	cm.mu.Unlock()
	if backoff < 1 {
		t.Errorf("backoff metric not recorded (got %d)", backoff)
	}
	if dwell < 1 {
		t.Errorf("dwell metric not recorded (got %d)", dwell)
	}

	// Drain the held first request so cleanup is fast.
	closeRelease()
	select {
	case <-holder:
	case <-time.After(time.Second):
	}
}

// openAPIDispatchMetrics tallies just RecordDispatch calls, capturing
// the labels so a single test can verify happy-path + error path
// label parity with the proto pool path.
type openAPIDispatchMetrics struct {
	noopMetrics
	mu      sync.Mutex
	calls   []openAPIDispatchCall
}

type openAPIDispatchCall struct {
	Namespace string
	Version   string
	Method    string
	Err       error
}

func (m *openAPIDispatchMetrics) RecordDispatch(ns, ver, method string, _ time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, openAPIDispatchCall{ns, ver, method, err})
}

func TestOpenAPIE2E_RecordDispatchFires(t *testing.T) {
	// Happy path + backend-error path: RecordDispatch must fire once
	// per dispatch with (namespace, "v1", "<METHOD> <pathTemplate>"),
	// matching the proto pool path's contract.
	cm := &openAPIDispatchMetrics{}
	gw := New(WithMetrics(cm), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)

	var fail atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
	}))
	t.Cleanup(backend.Close)

	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	h := gw.Handler()

	post := func(q string) {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(q))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	post(`{"query":"{ test_getThing(id:\"1\") { id } }"}`)
	fail.Store(true)
	post(`{"query":"{ test_getThing(id:\"2\") { id } }"}`)

	cm.mu.Lock()
	calls := append([]openAPIDispatchCall(nil), cm.calls...)
	cm.mu.Unlock()

	if len(calls) != 2 {
		t.Fatalf("expected 2 RecordDispatch calls, got %d: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Namespace != "test" || c.Version != "v1" || c.Method != "GET /things/{id}" {
			t.Errorf("call[%d] labels = (%q, %q, %q), want (test, v1, GET /things/{id})",
				i, c.Namespace, c.Version, c.Method)
		}
	}
	if calls[0].Err != nil {
		t.Errorf("call[0] err = %v, want nil (happy path)", calls[0].Err)
	}
	if calls[1].Err == nil {
		t.Errorf("call[1] err = nil, want non-nil (backend 500)")
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
