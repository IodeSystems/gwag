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
