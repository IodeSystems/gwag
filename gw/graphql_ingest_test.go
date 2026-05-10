package gateway

import (
	"bytes"
	"context"
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
	t            *testing.T
	server       *httptest.Server
	lastQuery    atomic.Pointer[string]
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
	// Field names are nested under a `pets: PetsQueryNamespace!`
	// container in the runtime renderer; type names keep their `pets_`
	// prefix.
	for _, want := range []string{
		"PetsQueryNamespace",
		"users",
		"user(",
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
		strings.NewReader(`{"query":"{ pets { users { id name role } } }"}`))
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
	// "users", with no namespace wrapping.
	last := rf.lastQuery.Load()
	if last == nil {
		t.Fatal("remote never queried")
	}
	if !strings.Contains(*last, "users") {
		t.Fatalf("forwarded query missing 'users': %s", *last)
	}
	if strings.Contains(*last, "pets {") || strings.Contains(*last, "pets_users") {
		t.Fatalf("forwarded query leaked local namespace wrapper: %s", *last)
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
		strings.NewReader(`{"query":"{ pets { user(id:\"42\") { id name } } }"}`))
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

func (m *graphQLIngestDispatchMetrics) RecordDispatch(_ context.Context, ns, ver, method string, _ time.Duration, err error) {
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
		strings.NewReader(`{"query":"{ pets { users { id } } }"}`))
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
				strings.NewReader(`{"query":"{ pets { users { id } } }"}`))
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

	q := `{"query":"{ pets { users { id } } }"}`
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

func TestGraphQLIngest_DuplicateNamespaceIdempotent(t *testing.T) {
	// Re-registering the same namespace with the same introspection
	// is a no-op (matches the OpenAPI source path). A different hash
	// → error is covered by TestDynamicGraphQL_HashMismatchRejected.
	rf := newRemoteFixture(t)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("first AddGraphQL: %v", err)
	}
	if err := gw.AddGraphQL(rf.server.URL, As("pets")); err != nil {
		t.Fatalf("second AddGraphQL same hash: %v", err)
	}
}

// TestGraphQLIngest_TwoVersions registers v1 and v2 of the same
// namespace. Latest (v2) is exposed nested as "<ns>.users" with type
// "pets_User"; older (v1) sits under "<ns>.v1.users" with deprecation
// + type "pets_v1_User" so the two versions don't collide on type
// identity even when the introspection JSON is identical.
func TestGraphQLIngest_TwoVersions(t *testing.T) {
	v1 := newRemoteFixture(t)
	v2 := newRemoteFixture(t)
	v1.queryHandler = func(query string, vars map[string]any) any {
		return map[string]any{"users": []map[string]any{{"id": "v1", "name": "old", "role": "ADMIN"}}}
	}
	v2.queryHandler = func(query string, vars map[string]any) any {
		return map[string]any{"users": []map[string]any{{"id": "v2", "name": "new", "role": "ADMIN"}}}
	}

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(v1.server.URL, As("pets"), Version("v1")); err != nil {
		t.Fatalf("AddGraphQL v1: %v", err)
	}
	if err := gw.AddGraphQL(v2.server.URL, As("pets"), Version("v2")); err != nil {
		t.Fatalf("AddGraphQL v2: %v", err)
	}

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	post := func(query string) string {
		body, _ := json.Marshal(map[string]any{"query": query})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return string(out)
	}
	// pickIDAt walks data.<segments...>.users[0].id.
	pickIDAt := func(body string, path ...string) string {
		var out struct {
			Data   map[string]any `json:"data"`
			Errors []any          `json:"errors"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v: %s", err, body)
		}
		if len(out.Errors) > 0 {
			t.Fatalf("graphql errors: %v", out.Errors)
		}
		var cur any = out.Data
		for _, seg := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = m[seg]
		}
		users, _ := cur.([]any)
		if len(users) == 0 {
			return ""
		}
		u, _ := users[0].(map[string]any)
		id, _ := u["id"].(string)
		return id
	}

	if got := pickIDAt(post(`{ pets { users { id } } }`), "pets", "users"); got != "v2" {
		t.Errorf("pets.users id=%q want v2 (latest)", got)
	}
	if got := pickIDAt(post(`{ pets { v1 { users { id } } } }`), "pets", "v1", "users"); got != "v1" {
		t.Errorf("pets.v1.users id=%q want v1", got)
	}

	// SDL: nested namespace containers split latest vs older; type
	// names follow the same `pets_` / `pets_v1_` split.
	schemaSrv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(schemaSrv.Close)
	resp, err := http.Get(schemaSrv.URL)
	if err != nil {
		t.Fatalf("schema fetch: %v", err)
	}
	defer resp.Body.Close()
	sdlBytes, _ := io.ReadAll(resp.Body)
	sdl := string(sdlBytes)
	for _, want := range []string{
		"PetsQueryNamespace",
		"PetsV1QueryNamespace",
		"pets_User",    // latest type
		"pets_v1_User", // older type — distinct from latest
		`@deprecated(reason: "v2 is current")`,
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}
}

// petsUnionIntrospection extends petsIntrospection with a UNION type
// `Animal = Cat | Dog` and a Query.findAnimal field returning it.
// Used by TestGraphQLIngest_UnionTypedMirror to exercise the
// possibleTypes → graphql.NewUnion path.
const petsUnionIntrospection = `{
  "data": {
    "__schema": {
      "queryType": {"name": "Query"},
      "mutationType": null,
      "subscriptionType": null,
      "types": [
        {
          "kind": "OBJECT", "name": "Query", "fields": [
            {
              "name": "findAnimal",
              "args": [{"name": "id", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}],
              "type": {"kind": "UNION", "name": "Animal"}
            }
          ]
        },
        {
          "kind": "UNION", "name": "Animal",
          "possibleTypes": [
            {"kind": "OBJECT", "name": "Cat"},
            {"kind": "OBJECT", "name": "Dog"}
          ]
        },
        {
          "kind": "OBJECT", "name": "Cat", "fields": [
            {"name": "name", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}},
            {"name": "claws", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}}
          ]
        },
        {
          "kind": "OBJECT", "name": "Dog", "fields": [
            {"name": "name", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}},
            {"name": "barksPerMinute", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "Int"}}}
          ]
        }
      ]
    }
  }
}`

// TestGraphQLIngest_UnionTypedMirror covers the introspection →
// graphql.NewUnion path: SDL contains "union pets_Animal = pets_Cat
// | pets_Dog"; clients dispatch with inline fragments per variant;
// the gateway un-prefixes type-conditions on the wire and
// ResolveType picks the local Object via __typename.
func TestGraphQLIngest_UnionTypedMirror(t *testing.T) {
	var lastQuery atomic.Pointer[string]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "IntrospectionQuery") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(petsUnionIntrospection))
			return
		}
		lastQuery.Store(&req.Query)
		// Reply with a Cat-shaped value carrying __typename so
		// ResolveType picks pets_Cat.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"findAnimal": map[string]any{
					"__typename": "Cat",
					"name":       "whiskers",
					"claws":      9,
				},
			},
		})
	}))
	t.Cleanup(upstream.Close)

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	t.Cleanup(gw.Close)
	if err := gw.AddGraphQL(upstream.URL, As("pets")); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}

	// SDL: union with namespace-prefixed variants.
	schemaSrv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(schemaSrv.Close)
	sdlResp, err := http.Get(schemaSrv.URL)
	if err != nil {
		t.Fatalf("schema fetch: %v", err)
	}
	defer sdlResp.Body.Close()
	sdlBytes, _ := io.ReadAll(sdlResp.Body)
	sdl := string(sdlBytes)
	for _, want := range []string{
		"union pets_Animal",
		"pets_Cat",
		"pets_Dog",
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}

	// Dispatch with an inline fragment under the union. The local
	// query uses pets_Cat / pets_Dog; the gateway must un-prefix to
	// `Cat` / `Dog` on the wire so the upstream sees its own names.
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	gqlBody, _ := json.Marshal(map[string]any{
		"query": `{
			pets {
				findAnimal(id: "1") {
					__typename
					... on pets_Cat { name claws }
					... on pets_Dog { name barksPerMinute }
				}
			}
		}`,
	})
	resp, err := http.Post(srv.URL+"/graphql", "application/json", bytes.NewReader(gqlBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Pets struct {
				FindAnimal map[string]any `json:"findAnimal"`
			} `json:"pets"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rawBody)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %v\nbody=%s", out.Errors, rawBody)
	}
	if got := out.Data.Pets.FindAnimal["__typename"]; got != "pets_Cat" {
		t.Errorf("__typename=%v want pets_Cat", got)
	}
	if got := out.Data.Pets.FindAnimal["name"]; got != "whiskers" {
		t.Errorf("name=%v want whiskers", got)
	}
	if _, ok := out.Data.Pets.FindAnimal["claws"]; !ok {
		t.Errorf("missing claws (Cat-specific) field; body=%s", rawBody)
	}

	// Forwarded query: type-conditions un-prefixed (`Cat`, `Dog`),
	// not the local `pets_Cat` / `pets_Dog`.
	last := lastQuery.Load()
	if last == nil {
		t.Fatal("upstream never received a query")
	}
	if !strings.Contains(*last, "on Cat") || !strings.Contains(*last, "on Dog") {
		t.Errorf("forwarded query missing un-prefixed inline fragments: %s", *last)
	}
	if strings.Contains(*last, "on pets_Cat") || strings.Contains(*last, "on pets_Dog") {
		t.Errorf("forwarded query still has prefixed type-conditions: %s", *last)
	}
}

// TestGraphQLIngest_HTTPIngressRouteSynthesized verifies the cross-
// kind ingress completeness pass end-to-end: a stitched-graphql
// backend gets HTTP routes synthesized via ir.RenderOpenAPI(svc), the
// canonical-args dispatcher synthesizes a default selection set from
// op.Output through introspection, and the response decodes back as
// JSON. Selection set is `{ __typename id name role }` (every leaf
// scalar/enum field on User), which the upstream sees verbatim.
func TestGraphQLIngest_HTTPIngressRouteSynthesized(t *testing.T) {
	rf := newRemoteFixture(t)
	rf.queryHandler = func(query string, vars map[string]any) any {
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
	srv := httptest.NewServer(gw.IngressHandler())
	t.Cleanup(srv.Close)

	// IR→OpenAPI synthesis paths a graphql-origin service at
	// /<ns>.<ver>.Service/<op> (svc.ServiceName falls back to
	// "Service" for graphql-ingest), method GET for OpQuery.
	resp, err := http.Get(srv.URL + "/pets.v1.Service/users")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "alice") || !strings.Contains(string(body), "MEMBER") {
		t.Fatalf("response missing expected fields: %s", body)
	}
	// Confirm the upstream saw a synthesized canonical query, not
	// the introspection short-circuit. The selection should include
	// every leaf scalar/enum on User plus __typename.
	last := rf.lastQuery.Load()
	if last == nil {
		t.Fatal("upstream never received a non-introspection query")
	}
	for _, want := range []string{"users", "__typename", "id", "name", "role"} {
		if !strings.Contains(*last, want) {
			t.Errorf("synthesized query missing %q: %s", want, *last)
		}
	}
}
