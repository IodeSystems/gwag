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
