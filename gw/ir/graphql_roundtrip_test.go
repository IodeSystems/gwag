package ir

import (
	"strings"
	"testing"
)

// petsIntrospection is the same canned introspection used in the
// gateway's downstream-GraphQL ingest tests.
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

func TestGraphQLIngest(t *testing.T) {
	// IngestGraphQL takes the "data" envelope content. Pull the
	// inner schema out before passing.
	dataStart := strings.Index(petsIntrospection, `"data":`)
	dataEnd := strings.LastIndex(petsIntrospection, "}")
	data := []byte(petsIntrospection[dataStart+len(`"data":`):dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}

	// Query.users + Query.user → Operations of Kind=OpQuery.
	if got := len(svc.Operations); got != 2 {
		t.Fatalf("Operations = %d, want 2", got)
	}
	for _, op := range svc.Operations {
		if op.Kind != OpQuery {
			t.Errorf("op %s Kind = %v, want OpQuery", op.Name, op.Kind)
		}
	}

	// User object + Role enum land in Types.
	user, ok := svc.Types["User"]
	if !ok {
		t.Fatal("User missing from Types")
	}
	if user.TypeKind != TypeObject {
		t.Errorf("User TypeKind = %v, want TypeObject", user.TypeKind)
	}
	if got := len(user.Fields); got != 3 {
		t.Errorf("User has %d fields, want 3", got)
	}
	role, ok := svc.Types["Role"]
	if !ok {
		t.Fatal("Role missing from Types")
	}
	if role.TypeKind != TypeEnum {
		t.Errorf("Role TypeKind = %v, want TypeEnum", role.TypeKind)
	}
	if got := len(role.Enum); got != 2 {
		t.Errorf("Role has %d values, want 2", got)
	}
}

// nestedIntrospection mimics a self-emitted nested-namespace
// gateway schema: Query.greeter holds operations directly + a v1
// sub-namespace, all reachable through introspection.
const nestedIntrospection = `{
  "data": {
    "__schema": {
      "queryType": {"name": "Query"},
      "mutationType": null,
      "subscriptionType": null,
      "types": [
        {
          "kind": "OBJECT", "name": "Query", "fields": [
            {"name": "greeter", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "GreeterNamespace"}}}
          ]
        },
        {
          "kind": "OBJECT", "name": "GreeterNamespace", "fields": [
            {
              "name": "hello",
              "args": [{"name": "name", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}}],
              "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}
            },
            {"name": "v1", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "GreeterV1Namespace"}}}
          ]
        },
        {
          "kind": "OBJECT", "name": "GreeterV1Namespace", "fields": [
            {
              "name": "hello",
              "args": [{"name": "name", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}}],
              "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}
            }
          ]
        }
      ]
    }
  }
}`

func TestGraphQLIngest_NestedNamespaces(t *testing.T) {
	dataStart := strings.Index(nestedIntrospection, `"data":`)
	dataEnd := strings.LastIndex(nestedIntrospection, "}")
	data := []byte(nestedIntrospection[dataStart+len(`"data":`):dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}

	if len(svc.Operations) != 0 {
		t.Errorf("top-level Operations = %d, want 0 (greeter is a Group, not a flat op)", len(svc.Operations))
	}
	if len(svc.Groups) != 1 {
		t.Fatalf("Groups = %d, want 1", len(svc.Groups))
	}
	g := svc.Groups[0]
	if g.Name != "greeter" {
		t.Errorf("group Name = %q, want greeter", g.Name)
	}
	if g.Kind != OpQuery {
		t.Errorf("group Kind = %v, want OpQuery", g.Kind)
	}
	if len(g.Operations) != 1 || g.Operations[0].Name != "hello" {
		t.Errorf("greeter.Operations not [hello]: %v", g.Operations)
	}
	if len(g.Operations[0].Args) != 1 || g.Operations[0].Args[0].Name != "name" {
		t.Errorf("greeter.hello args wrong: %v", g.Operations[0].Args)
	}
	if len(g.Groups) != 1 || g.Groups[0].Name != "v1" {
		t.Fatalf("greeter.Groups not [v1]: %v", g.Groups)
	}
	v1 := g.Groups[0]
	if v1.Kind != OpQuery {
		t.Errorf("v1 Kind = %v, want OpQuery", v1.Kind)
	}
	if len(v1.Operations) != 1 || v1.Operations[0].Name != "hello" {
		t.Errorf("v1.Operations not [hello]: %v", v1.Operations)
	}

	// Synthesized container types should be pruned from svc.Types
	// — they're regenerated by the renderer from Groups.
	if _, ok := svc.Types["GreeterNamespace"]; ok {
		t.Errorf("GreeterNamespace leaked into svc.Types")
	}
	if _, ok := svc.Types["GreeterV1Namespace"]; ok {
		t.Errorf("GreeterV1Namespace leaked into svc.Types")
	}
}

func TestGraphQLRender_NestedNamespaces(t *testing.T) {
	dataStart := strings.Index(nestedIntrospection, `"data":`)
	dataEnd := strings.LastIndex(nestedIntrospection, "}")
	data := []byte(nestedIntrospection[dataStart+len(`"data":`):dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	sdl := RenderGraphQL([]*Service{svc})
	for _, want := range []string{
		"type Query {",
		"greeter: GreeterQueryNamespace!",
		"type GreeterQueryNamespace {",
		"hello(name: String!): String!",
		"v1: GreeterQueryV1Namespace!",
		"type GreeterQueryV1Namespace {",
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}
}

func TestGraphQLRender_FlatProtoFlattensGroupsToProto(t *testing.T) {
	// A nested-namespace ingest, rendered to proto, should flatten
	// group paths into method names with `_` joins.
	dataStart := strings.Index(nestedIntrospection, `"data":`)
	dataEnd := strings.LastIndex(nestedIntrospection, "}")
	data := []byte(nestedIntrospection[dataStart+len(`"data":`):dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Namespace = "greeter"
	svc.Version = "v1"
	svc.ServiceName = "GreeterService"
	flat := svc.FlatOperations()
	names := map[string]bool{}
	for _, op := range flat {
		names[op.Name] = true
	}
	for _, want := range []string{"greeter_hello", "greeter_v1_hello"} {
		if !names[want] {
			t.Errorf("FlatOperations missing %q; got %v", want, names)
		}
	}
}

func TestGraphQLRender(t *testing.T) {
	dataStart := strings.Index(petsIntrospection, `"data":`)
	dataEnd := strings.LastIndex(petsIntrospection, "}")
	data := []byte(petsIntrospection[dataStart+len(`"data":`):dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	sdl := RenderGraphQL([]*Service{svc})
	for _, want := range []string{
		"type User {",
		"id: ID!",
		"name: String",
		"role: Role!",
		"enum Role {",
		"ADMIN",
		"MEMBER",
		"type Query {",
		"users: [User!]!",
		"user(id: ID!): User",
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}
}
