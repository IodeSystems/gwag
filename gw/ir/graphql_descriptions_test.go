package ir

import (
	"strings"
	"testing"
)

const petsIntrospectionWithDocs = `{
  "data": {
    "__schema": {
      "queryType": {"name": "Query"},
      "mutationType": null,
      "subscriptionType": null,
      "types": [
        {
          "kind": "OBJECT", "name": "Query", "fields": [
            {
              "name": "user",
              "description": "Look up a user by id.",
              "args": [
                {"name": "id", "description": "Stable user identifier.", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}
              ],
              "type": {"kind": "OBJECT", "name": "User"}
            }
          ]
        },
        {
          "kind": "OBJECT", "name": "User", "description": "An authenticated principal.",
          "fields": [
            {"name": "id", "description": "Stable identifier.", "args": [], "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}}
          ]
        }
      ]
    }
  }
}`

// TestGraphQLIngest_DescriptionsLand pins that introspection
// `description` fields land in IR Description for ops, args, types,
// and fields. MCP search corpus depends on this for downstream-
// GraphQL services.
func TestGraphQLIngest_DescriptionsLand(t *testing.T) {
	dataStart := strings.Index(petsIntrospectionWithDocs, `"data":`)
	dataEnd := strings.LastIndex(petsIntrospectionWithDocs, "}")
	data := []byte(petsIntrospectionWithDocs[dataStart+len(`"data":`) : dataEnd])
	svc, err := IngestGraphQL(data)
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}

	var userOp *Operation
	for _, op := range svc.Operations {
		if op.Name == "user" {
			userOp = op
			break
		}
	}
	if userOp == nil {
		t.Fatal("user op missing")
	}
	if !strings.Contains(userOp.Description, "Look up a user") {
		t.Errorf("user.Description = %q", userOp.Description)
	}
	var idArg *Arg
	for _, a := range userOp.Args {
		if a.Name == "id" {
			idArg = a
			break
		}
	}
	if idArg == nil {
		t.Fatal("user.id arg missing")
	}
	if !strings.Contains(idArg.Description, "Stable user identifier") {
		t.Errorf("user.id.Description = %q", idArg.Description)
	}

	user, ok := svc.Types["User"]
	if !ok {
		t.Fatal("User type missing")
	}
	if !strings.Contains(user.Description, "authenticated principal") {
		t.Errorf("User.Description = %q", user.Description)
	}
	for _, f := range user.Fields {
		if f.Name == "id" {
			if !strings.Contains(f.Description, "Stable identifier") {
				t.Errorf("User.id.Description = %q", f.Description)
			}
			return
		}
	}
	t.Fatal("User.id field missing")
}
