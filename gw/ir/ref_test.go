package ir

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestSplitRef(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantClean string
		wantRef   string
	}{
		{"none", "Just a description.", "Just a description.", ""},
		{"trailing", "List pets.\n\n@ref server/main.go:listPets", "List pets.", "server/main.go:listPets"},
		{"only", "@ref a/b.go:Sym", "", "a/b.go:Sym"},
		{"leading-space", "  @ref a/b.go:Sym\nText.", "Text.", "a/b.go:Sym"},
		{"path-only", "@ref a/b.go", "", "a/b.go"},
		{"not-a-marker", "@reference manual", "@reference manual", ""},
		{"bare-marker", "@ref", "@ref", ""},
		{"first-only", "@ref one\n@ref two", "@ref two", "one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean, ref := splitRef(c.in)
			if clean != c.wantClean || ref != c.wantRef {
				t.Errorf("splitRef(%q) = (%q, %q), want (%q, %q)", c.in, clean, ref, c.wantClean, c.wantRef)
			}
		})
	}
}

func TestWithRef(t *testing.T) {
	if got := withRef("Desc.", ""); got != "Desc." {
		t.Errorf("empty ref: %q", got)
	}
	if got := withRef("Desc.", "a/b.go:S"); got != "Desc.\n\n@ref a/b.go:S" {
		t.Errorf("with desc: %q", got)
	}
	if got := withRef("", "a/b.go:S"); got != "@ref a/b.go:S" {
		t.Errorf("empty desc: %q", got)
	}
}

func TestExtString(t *testing.T) {
	ext := map[string]any{"x-ref": "a/b.go:S", "x-other": 42}
	if got := extString(ext, "x-ref"); got != "a/b.go:S" {
		t.Errorf("string ext = %q", got)
	}
	if got := extString(ext, "x-missing"); got != "" {
		t.Errorf("missing ext = %q", got)
	}
	if got := extString(ext, "x-other"); got != "" {
		t.Errorf("non-string ext = %q", got)
	}
}

const openapiSpecWithRef = `{
  "openapi": "3.0.0",
  "info": {"title": "t", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "operationId": "listPets",
        "x-ref": "server/main.go:listPets",
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

// TestOpenAPIRefCarriage: an OpenAPI x-ref extension is captured into
// IR.Ref and re-emitted both as a GraphQL @ref doc line and as a proto
// method leading comment (SourceCodeInfo).
func TestOpenAPIRefCarriage(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiSpecWithRef))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Namespace = "pets"
	svc.Version = "v1"
	svc.ServiceName = "PetsService"

	var op *Operation
	for _, o := range svc.Operations {
		if o.Name == "listPets" {
			op = o
		}
	}
	if op == nil {
		t.Fatal("listPets op missing")
	}
	if op.Ref != "server/main.go:listPets" {
		t.Fatalf("op.Ref = %q", op.Ref)
	}

	// GraphQL emit: the served SDL is the runtime schema printed via
	// PrintSchemaSDL.
	sdl := runtimeSDL(t, svc)
	if !strings.Contains(sdl, "@ref server/main.go:listPets") {
		t.Errorf("SDL missing @ref marker\n--- SDL ---\n%s", sdl)
	}

	// Proto emit: leading comment on the synthesized method.
	fds, err := RenderProtoFiles([]*Service{svc})
	if err != nil {
		t.Fatalf("RenderProtoFiles: %v", err)
	}
	sci := fds.File[0].GetSourceCodeInfo()
	if sci == nil {
		t.Fatal("no SourceCodeInfo on rendered proto")
	}
	found := false
	for _, loc := range sci.GetLocation() {
		if strings.Contains(loc.GetLeadingComments(), "@ref server/main.go:listPets") {
			found = true
		}
	}
	if !found {
		t.Errorf("proto SourceCodeInfo missing @ref leading comment: %+v", sci.GetLocation())
	}
}

const graphqlIntrospectionWithRef = `{
  "__schema": {
    "queryType": {"name": "Query"},
    "mutationType": null,
    "subscriptionType": null,
    "types": [
      {
        "kind": "OBJECT", "name": "Query", "fields": [
          {
            "name": "pet",
            "description": "Look up a pet.\n\n@ref server/main.go:pet",
            "args": [],
            "type": {"kind": "OBJECT", "name": "Pet"}
          }
        ]
      },
      {
        "kind": "OBJECT", "name": "Pet", "description": "A pet.\n\n@ref server/types.go:Pet",
        "fields": [
          {"name": "id", "description": "Id.", "args": [], "type": {"kind": "SCALAR", "name": "String"}}
        ]
      }
    ]
  }
}`

// TestGraphQLRefCarriage: a GraphQL description @ref line is moved into
// IR.Ref (and stripped from the description), then re-emitted once into
// the rendered SDL — idempotent, no duplication.
func TestGraphQLRefCarriage(t *testing.T) {
	svc, err := IngestGraphQL([]byte(graphqlIntrospectionWithRef))
	if err != nil {
		t.Fatalf("IngestGraphQL: %v", err)
	}
	svc.Namespace = "shop"
	svc.Version = "v1"
	svc.ServiceName = "ShopService"
	var petOp *Operation
	for _, o := range svc.Operations {
		if o.Name == "pet" {
			petOp = o
		}
	}
	if petOp == nil {
		t.Fatal("pet op missing")
	}
	if petOp.Ref != "server/main.go:pet" {
		t.Errorf("op.Ref = %q", petOp.Ref)
	}
	if strings.Contains(petOp.Description, "@ref") {
		t.Errorf("op.Description still carries marker: %q", petOp.Description)
	}
	pet := svc.Types["Pet"]
	if pet == nil || pet.Ref != "server/types.go:Pet" {
		t.Errorf("Pet.Ref = %q", refOf(pet))
	}

	sdl := runtimeSDL(t, svc)
	// The op renders once per namespace tier (latest alias + versioned),
	// so >=1 — what matters is each rendered description carries exactly
	// one marker (no ingest-strip/re-emit doubling). The type is emitted
	// once in the type map.
	if !strings.Contains(sdl, "@ref server/main.go:pet") {
		t.Errorf("op @ref missing from SDL\n--- SDL ---\n%s", sdl)
	}
	if strings.Contains(sdl, "@ref server/main.go:pet @ref") || strings.Contains(sdl, "@ref @ref") {
		t.Errorf("op @ref doubled within a description\n--- SDL ---\n%s", sdl)
	}
	if n := strings.Count(sdl, "@ref server/types.go:Pet"); n != 1 {
		t.Errorf("type @ref appears %d times in SDL, want 1\n--- SDL ---\n%s", n, sdl)
	}
}

func refOf(t *Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.Ref
}

// runtimeSDL builds the runtime graphql.Schema for svcs and prints it as
// SDL — the artifact the gateway serves at /schema/graphql, and the only
// GraphQL SDL producer in the package.
func runtimeSDL(t *testing.T, svcs ...*Service) string {
	t.Helper()
	schema, err := RenderGraphQLRuntime(svcs, NewDispatchRegistry(), RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	return PrintSchemaSDL(schema)
}
