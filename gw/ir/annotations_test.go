package ir

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestParseGqlDirective(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantArgs []AnnotationArg
		wantOK   bool
	}{
		{"pii", "pii", nil, true},
		{"hasRole(role: \"ADMIN\")", "hasRole", []AnnotationArg{{"role", AnnString, "ADMIN"}}, true},
		{"hasRole(role: ADMIN)", "hasRole", []AnnotationArg{{"role", AnnIdent, "ADMIN"}}, true},
		{"cost(weight: 5)", "cost", []AnnotationArg{{"weight", AnnNumber, "5"}}, true},
		{"flag(on: true)", "flag", []AnnotationArg{{"on", AnnBool, "true"}}, true},
		{"two(a: 1, b: \"x, y\")", "two", []AnnotationArg{{"a", AnnNumber, "1"}, {"b", AnnString, "x, y"}}, true},
		{"", "", nil, false},
		{"bad(", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			an, ok := parseGqlDirective(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if an.Name != c.wantName {
				t.Errorf("name = %q, want %q", an.Name, c.wantName)
			}
			if len(an.Args) != len(c.wantArgs) {
				t.Fatalf("args = %+v, want %+v", an.Args, c.wantArgs)
			}
			for i, a := range an.Args {
				if a != c.wantArgs[i] {
					t.Errorf("arg[%d] = %+v, want %+v", i, a, c.wantArgs[i])
				}
			}
		})
	}
}

func TestSplitGqlAnnotations(t *testing.T) {
	comment := "Greet a caller.\n\n@gql hasRole(role: \"ADMIN\")\n@gql audited"
	clean, anns := splitGqlAnnotations(comment)
	if clean != "Greet a caller." {
		t.Errorf("clean = %q", clean)
	}
	if len(anns) != 2 || anns[0].Name != "hasRole" || anns[1].Name != "audited" {
		t.Fatalf("anns = %+v", anns)
	}
}

const openapiSpecWithAnnotations = `{
  "openapi": "3.0.0",
  "info": {"title": "t", "version": "1.0.0"},
  "paths": {
    "/projects": {
      "get": {
        "operationId": "listProjects",
        "x-gwag-annotations": [
          {"name": "hasRole", "args": {"role": "ADMIN"}},
          {"name": "audited"}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

// TestOpenAPIAnnotations_ToGraphQLSDL: an OpenAPI x-gwag-annotations entry
// becomes a real directive on the served GraphQL SDL, with a synthesized
// declaration so the document stays valid.
func TestOpenAPIAnnotations_ToGraphQLSDL(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiSpecWithAnnotations))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Namespace = "proj"
	svc.Version = "v1"
	svc.ServiceName = "ProjService"

	var op *Operation
	for _, o := range svc.Operations {
		if o.Name == "listProjects" {
			op = o
		}
	}
	if op == nil {
		t.Fatal("listProjects missing")
	}
	if len(op.Annotations) != 2 || op.Annotations[0].Name != "hasRole" {
		t.Fatalf("op.Annotations = %+v", op.Annotations)
	}

	idx := NewAnnotationIndex()
	schema, err := RenderGraphQLRuntime([]*Service{svc}, NewDispatchRegistry(), RuntimeOptions{AnnotationSink: idx})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	sdl := PrintSchemaSDL(schema, idx)

	for _, want := range []string{
		// Directives render sorted by name for determinism.
		`listProjects: String @audited @hasRole(role: "ADMIN")`,
		"directive @hasRole(role: String) on FIELD_DEFINITION",
		"directive @audited on FIELD_DEFINITION",
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q\n--- SDL ---\n%s", want, sdl)
		}
	}
	// Without the index, no directives leak into the SDL.
	if plain := PrintSchemaSDL(schema); strings.Contains(plain, "@hasRole") {
		t.Errorf("annotations leaked into un-indexed SDL\n%s", plain)
	}
}

// TestProtoAnnotations_RoundTrip: a proto `@gql` comment is ingested into
// IR, re-emitted into the proto SDL view, and surfaces as a GraphQL
// directive.
func TestProtoAnnotations_RoundTrip(t *testing.T) {
	// Drive through the OpenAPI→proto render path for SourceCodeInfo,
	// reusing the proto egress; ingest is covered by the gat test. Here
	// assert the IR→proto comment emission directly.
	svc := &Service{
		Namespace:   "svc",
		Version:     "v1",
		ServiceName: "S",
		OriginKind:  KindOpenAPI,
		Operations: []*Operation{{
			Name:        "doThing",
			Kind:        OpMutation,
			Annotations: []Annotation{{Name: "hasRole", Args: []AnnotationArg{{Name: "role", Kind: AnnIdent, Value: "ADMIN"}}}},
		}},
	}
	fds, err := RenderProtoFiles([]*Service{svc})
	if err != nil {
		t.Fatalf("RenderProtoFiles: %v", err)
	}
	sci := fds.File[0].GetSourceCodeInfo()
	if sci == nil {
		t.Fatal("no SourceCodeInfo")
	}
	found := false
	for _, loc := range sci.GetLocation() {
		if strings.Contains(loc.GetLeadingComments(), "@gql hasRole(role: ADMIN)") {
			found = true
		}
	}
	if !found {
		t.Errorf("proto comment missing @gql: %+v", sci.GetLocation())
	}
}

// TestAnnotations_OpenAPIEgress: cross-kind OpenAPI render carries
// annotations back out as the x-gwag-annotations extension.
func TestAnnotations_OpenAPIEgress(t *testing.T) {
	svc := &Service{
		Namespace: "svc", Version: "v1", ServiceName: "S", OriginKind: KindProto,
		Operations: []*Operation{{
			Name:        "doThing",
			Kind:        OpQuery,
			Annotations: []Annotation{{Name: "pii"}},
			OriginKind:  KindProto,
		}},
	}
	doc, err := RenderOpenAPI(svc)
	if err != nil {
		t.Fatalf("RenderOpenAPI: %v", err)
	}
	var op *openapi3.Operation
	for _, p := range doc.Paths.Map() {
		for _, o := range p.Operations() {
			if o.OperationID == "doThing" {
				op = o
			}
		}
	}
	if op == nil {
		t.Fatal("doThing op missing from rendered OpenAPI")
	}
	if _, ok := op.Extensions[xAnnotationsExtension]; !ok {
		t.Errorf("x-gwag-annotations missing; extensions = %+v", op.Extensions)
	}
}
