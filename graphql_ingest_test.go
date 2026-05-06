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

// petsIntrospection is a minimal introspection JSON for a pets-svc:
//
//	type Query {
//	  users: [User!]!
//	  user(id: ID!): User
//	}
//	type User {
//	  id: ID!
//	  name: String
//	  role: Role!
//	}
//	enum Role { ADMIN MEMBER }
//
// Built by hand so the test stays independent of any GraphQL server.
const petsIntrospection = `{
  "data": {
    "__schema": {
      "queryType": {"name": "Query"},
      "mutationType": null,
      "subscriptionType": null,
      "types": [
        {
          "kind": "OBJECT", "name": "Query", "fields": [
            {
              "name": "users",
              "args": [],
              "type": {"kind": "NON_NULL", "ofType": {"kind": "LIST", "ofType": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "User"}}}}
            },
            {
              "name": "user",
              "args": [{"name": "id", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}],
              "type": {"kind": "OBJECT", "name": "User"}
            }
          ]
        },
        {
          "kind": "OBJECT", "name": "User", "fields": [
            {"name": "id", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}},
            {"name": "name", "args": [], "type": {"kind": "SCALAR", "name": "String"}},
            {"name": "role", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "ENUM", "name": "Role"}}}
          ]
        },
        {
          "kind": "ENUM", "name": "Role", "enumValues": [
            {"name": "ADMIN"},
            {"name": "MEMBER"}
          ]
        }
      ]
    }
  }
}`

// remoteFixture is a fake downstream GraphQL service. Records the
// last query body the gateway forwarded; respond shape is per-test.
type remoteFixture struct {
	t           *testing.T
	server      *httptest.Server
	lastQuery   atomic.Pointer[string]
	queryHandler func(query string, vars map[string]any) any
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	rf := &remoteFixture{t: t}
	rf.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Introspection short-circuit: any IntrospectionQuery returns
		// the canned petsIntrospection.
		if strings.Contains(req.Query, "IntrospectionQuery") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(petsIntrospection))
			return
		}
		rf.lastQuery.Store(&req.Query)
		var data any
		if rf.queryHandler != nil {
			data = rf.queryHandler(req.Query, req.Variables)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(rf.server.Close)
	return rf
}

func TestGraphQLIngest_SchemaPrefixesTypes(t *testing.T) {
	rf := newRemoteFixture(t)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	// Hit /schema/graphql via gw.SchemaHandler — the SDL must contain
	// the namespace-prefixed types.
	schemaSrv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(schemaSrv.Close)
	resp, err := http.Get(schemaSrv.URL)
	if err != nil {
		t.Fatalf("schema fetch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	sdl := string(body)
	for _, want := range []string{
		"pets_users",
		"pets_user",
		"pets_User",
		"pets_Role",
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}
	// Built-in scalars stay unprefixed.
	if strings.Contains(sdl, "pets_ID") || strings.Contains(sdl, "pets_String") {
		t.Errorf("SDL prefixed a built-in scalar:\n%s", sdl)
	}
}

func TestGraphQLIngest_ForwardingStripsPrefix(t *testing.T) {
	rf := newRemoteFixture(t)
	rf.queryHandler = func(query string, vars map[string]any) any {
		// Remote should see the un-prefixed field name.
		return map[string]any{
			"users": []map[string]any{
				{"id": "1", "name": "alice", "role": "ADMIN"},
				{"id": "2", "name": "bob", "role": "MEMBER"},
			},
		}
	}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ pets_users { id name role } }"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(body), "errors") {
		t.Fatalf("response had errors: %s", body)
	}
	if !strings.Contains(string(body), "alice") || !strings.Contains(string(body), "MEMBER") {
		t.Fatalf("unexpected response: %s", body)
	}

	// Inspect what the remote actually received. Field name must be
	// "users", not "pets_users".
	last := rf.lastQuery.Load()
	if last == nil {
		t.Fatal("remote never queried")
	}
	if !strings.Contains(*last, "users") {
		t.Fatalf("forwarded query missing 'users': %s", *last)
	}
	if strings.Contains(*last, "pets_users") {
		t.Fatalf("forwarded query still has 'pets_users' prefix: %s", *last)
	}
}

func TestGraphQLIngest_ArgumentsPassThrough(t *testing.T) {
	rf := newRemoteFixture(t)
	rf.queryHandler = func(query string, vars map[string]any) any {
		return map[string]any{"user": map[string]any{"id": "42", "name": "alice", "role": "ADMIN"}}
	}
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ pets_user(id:\"42\") { id name } }"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "errors") {
		t.Fatalf("response had errors: %s", body)
	}
	last := *rf.lastQuery.Load()
	// The arg must survive into the forwarded query.
	if !strings.Contains(last, "42") {
		t.Errorf("forwarded query missing arg: %s", last)
	}
	if !strings.Contains(last, "user(") {
		t.Errorf("forwarded query missing user(...): %s", last)
	}
}

// graphQLIngestDispatchMetrics tallies just RecordDispatch calls so
// the test can verify label parity with the proto / OpenAPI paths.
type graphQLIngestDispatchMetrics struct {
	noopMetrics
	mu    sync.Mutex
	calls []graphQLIngestDispatchCall
}

type graphQLIngestDispatchCall struct {
	Namespace string
	Version   string
	Method    string
	Err       error
}

func (m *graphQLIngestDispatchMetrics) RecordDispatch(ns, ver, method string, _ time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, graphQLIngestDispatchCall{ns, ver, method, err})
}

func TestGraphQLIngest_RecordDispatchFires(t *testing.T) {
	rf := newRemoteFixture(t)
	rf.queryHandler = func(_ string, _ map[string]any) any {
		return map[string]any{"users": []map[string]any{{"id": "1", "name": "a", "role": "ADMIN"}}}
	}
	cm := &graphQLIngestDispatchMetrics{}
	gw := New(WithMetrics(cm), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ pets_users { id } }"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	cm.mu.Lock()
	calls := append([]graphQLIngestDispatchCall(nil), cm.calls...)
	cm.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("expected 1 RecordDispatch call, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.Namespace != "pets" || c.Version != "v1" || c.Method != "query users" {
		t.Errorf("labels = (%q, %q, %q), want (pets, v1, query users)",
			c.Namespace, c.Version, c.Method)
	}
	if c.Err != nil {
		t.Errorf("err = %v, want nil (happy path)", c.Err)
	}
}

func TestGraphQLIngest_ErrorClassification(t *testing.T) {
	// HTTP statuses + remote GraphQL errors must propagate as Reject
	// codes so go_api_gateway_dispatch_duration_seconds slices by
	// outcome the way the OpenAPI path does.
	cases := []struct {
		name     string
		respond  func(w http.ResponseWriter)
		wantCode string
	}{
		{
			name: "http-401",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("nope"))
			},
			wantCode: "UNAUTHENTICATED",
		},
		{
			name: "http-404",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("missing"))
			},
			wantCode: "NOT_FOUND",
		},
		{
			name: "http-500",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			},
			wantCode: "INTERNAL",
		},
		{
			name: "remote-graphql-error",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"errors":[{"message":"bad"}]}`))
			},
			wantCode: "INTERNAL",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Custom backend: respond to the introspection probe with
			// petsIntrospection so AddGraphQL succeeds, then apply the
			// per-case responder for actual queries.
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal(body, &req)
				if strings.Contains(req.Query, "IntrospectionQuery") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(petsIntrospection))
					return
				}
				tc.respond(w)
			}))
			t.Cleanup(backend.Close)

			gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
			t.Cleanup(gw.Close)
			if err := gw.AddGraphQL(backend.URL, As("pets")); err != nil {
				t.Fatalf("AddGraphQL: %v", err)
			}
			srv := httptest.NewServer(gw.Handler())
			t.Cleanup(srv.Close)

			resp, err := http.Post(srv.URL+"/graphql", "application/json",
				strings.NewReader(`{"query":"{ pets_users { id } }"}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tc.wantCode) {
				t.Errorf("expected %s in response, got %s", tc.wantCode, body)
			}
		})
	}
}

// graphQLIngestBackpressureMetrics tallies dwell + backoff calls so
// the backpressure test can confirm the per-source semaphore actually
// fired. Mirrors openAPIBackpressureMetrics.
type graphQLIngestBackpressureMetrics struct {
	noopMetrics
	mu       sync.Mutex
	backoff  int
	dwellHit int
}

func (m *graphQLIngestBackpressureMetrics) RecordDwell(_, _, _, kind string, _ time.Duration) {
	if kind != "unary" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dwellHit++
}

func (m *graphQLIngestBackpressureMetrics) RecordBackoff(_, _, _, _, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backoff++
}

func (m *graphQLIngestBackpressureMetrics) SetQueueDepth(_, _, _ string, _ int) {}

func TestGraphQLIngest_BackpressureTimesOutAndRejects(t *testing.T) {
	// One backend slot held by a long-running request; with MaxInflight=1
	// and MaxWaitTime=50ms a concurrent dispatch should reject with
	// RESOURCE_EXHAUSTED rather than queueing forever. Same shape as
	// TestOpenAPIE2E_BackpressureTimesOutAndRejects.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	requestArrived := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		// Always respond fast for the introspection probe; only the
		// actual query holds the slot.
		if strings.Contains(req.Query, "IntrospectionQuery") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(petsIntrospection))
			return
		}
		select {
		case requestArrived <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"users":[]}}`))
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(closeRelease)

	cm := &graphQLIngestBackpressureMetrics{}
	gw := New(
		WithMetrics(cm),
		WithBackpressure(BackpressureOptions{MaxInflight: 1, MaxWaitTime: 50 * time.Millisecond}),
		WithAdminToken([]byte("test-token")),
	)
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(backend.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	postQuery := func(q string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/graphql", strings.NewReader(q))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	q := `{"query":"{ pets_users { id } }"}`
	holder := make(chan string, 1)
	go func() {
		_, body := postQuery(q)
		holder <- body
	}()

	// Wait until the first request reached the backend (and is
	// holding the slot) before firing the second.
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

func TestGraphQLIngest_DuplicateNamespaceRejected(t *testing.T) {
	rf := newRemoteFixture(t)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("first AddGraphQL: %v", err)
	}
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err == nil {
		t.Fatal("expected error on duplicate namespace")
	}
}
