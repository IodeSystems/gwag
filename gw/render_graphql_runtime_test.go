package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/graphql-go/graphql"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// TestRenderGraphQLRuntime_ProtoTopLevelQuery is the smallest happy
// path: one proto-origin service with one Query op that flattens a
// single arg, returns a typed Object via the IR Types registry, and
// resolves through the dispatcher registry.
func TestRenderGraphQLRuntime_ProtoTopLevelQuery(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "greeter",
		Version:    "v1",
		OriginKind: ir.KindProto,
		Types: map[string]*ir.Type{
			"greeter.HelloReply": {
				Name:     "greeter.HelloReply",
				TypeKind: ir.TypeObject,
				Fields: []*ir.Field{
					{Name: "message", Type: ir.TypeRef{Builtin: ir.ScalarString}},
				},
			},
		},
		Operations: []*ir.Operation{{
			Name:   "hello",
			Kind:   ir.OpQuery,
			Args:   []*ir.Arg{{Name: "name", Type: ir.TypeRef{Builtin: ir.ScalarString}}},
			Output: &ir.TypeRef{Named: "greeter.HelloReply"},
		}},
	}
	ir.PopulateSchemaIDs(svc)

	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		name, _ := args["name"].(string)
		return map[string]any{"message": "hello, " + name}, nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}

	res := graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `{ hello(name: "world") { message } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("execute: %v", res.Errors)
	}
	data, _ := res.Data.(map[string]any)
	hello, _ := data["hello"].(map[string]any)
	if got, want := hello["message"], "hello, world"; got != want {
		t.Errorf("hello.message = %v, want %v", got, want)
	}
}

// TestRenderGraphQLRuntime_OpenAPIQueryAndMutation covers the
// OpenAPI-origin naming policy (type names prefixed with `<ns>_`)
// and the OpQuery / OpMutation split into separate roots.
func TestRenderGraphQLRuntime_OpenAPIQueryAndMutation(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "pets",
		Version:    "v1",
		OriginKind: ir.KindOpenAPI,
		Types: map[string]*ir.Type{
			"Pet": {
				Name:     "Pet",
				TypeKind: ir.TypeObject,
				Fields: []*ir.Field{
					{Name: "id", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true},
					{Name: "name", Type: ir.TypeRef{Builtin: ir.ScalarString}},
				},
			},
		},
		Operations: []*ir.Operation{
			{
				Name:   "getPet",
				Kind:   ir.OpQuery,
				Args:   []*ir.Arg{{Name: "id", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true}},
				Output: &ir.TypeRef{Named: "Pet"},
			},
			{
				Name: "createPet",
				Kind: ir.OpMutation,
				Args: []*ir.Arg{
					{Name: "name", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true},
				},
				Output: &ir.TypeRef{Named: "Pet"},
			},
		},
	}
	ir.PopulateSchemaIDs(svc)

	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"id": args["id"], "name": "fido"}, nil
	}))
	registry.Set(svc.Operations[1].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"id": "new", "name": args["name"]}, nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}

	// Type name should be `pets_Pet` (OpenAPI naming policy).
	if got := schema.Type("pets_Pet"); got == nil {
		t.Errorf("expected type pets_Pet in schema; types: %v", schemaTypeNames(schema))
	}

	res := graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `{ getPet(id: "abc") { id name } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("query execute: %v", res.Errors)
	}
	data, _ := res.Data.(map[string]any)
	pet, _ := data["getPet"].(map[string]any)
	if pet["id"] != "abc" || pet["name"] != "fido" {
		t.Errorf("getPet = %v, want {id:abc, name:fido}", pet)
	}

	res = graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `mutation { createPet(name: "rex") { id name } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("mutation execute: %v", res.Errors)
	}
	data, _ = res.Data.(map[string]any)
	pet, _ = data["createPet"].(map[string]any)
	if pet["id"] != "new" || pet["name"] != "rex" {
		t.Errorf("createPet = %v, want {id:new, name:rex}", pet)
	}
}

// TestRenderGraphQLRuntime_GraphQLNestedGroups covers the
// graphql-origin nested-namespace pattern: an "admin" namespace
// hosting Query ops in one Group and Mutation ops in another. Each
// Group becomes a sibling field under its respective root with a
// synthesized container Object.
func TestRenderGraphQLRuntime_GraphQLNestedGroups(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "admin",
		Version:    "v1",
		OriginKind: ir.KindGraphQL,
		Types: map[string]*ir.Type{
			"Peer": {
				Name:     "Peer",
				TypeKind: ir.TypeObject,
				Fields: []*ir.Field{
					{Name: "id", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true},
				},
			},
		},
		Groups: []*ir.OperationGroup{
			{
				Name: "admin",
				Kind: ir.OpQuery,
				Operations: []*ir.Operation{{
					Name:   "listPeers",
					Kind:   ir.OpQuery,
					Output: &ir.TypeRef{Named: "Peer"}, OutputRepeated: true,
				}},
			},
			{
				Name: "admin",
				Kind: ir.OpMutation,
				Operations: []*ir.Operation{{
					Name:   "forgetPeer",
					Kind:   ir.OpMutation,
					Args:   []*ir.Arg{{Name: "id", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true}},
					Output: &ir.TypeRef{Named: "Peer"},
				}},
			},
		},
	}
	ir.PopulateSchemaIDs(svc)

	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Groups[0].Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return []any{map[string]any{"id": "p1"}}, nil
	}))
	registry.Set(svc.Groups[1].Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"id": args["id"]}, nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}

	// Nested-namespace container types should exist.
	for _, n := range []string{"AdminQueryNamespace", "AdminMutationNamespace"} {
		if schema.Type(n) == nil {
			t.Errorf("expected synthesized container type %s", n)
		}
	}

	res := graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `{ admin { listPeers { id } } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("query execute: %v", res.Errors)
	}
	data, _ := res.Data.(map[string]any)
	adminQ, _ := data["admin"].(map[string]any)
	peers, _ := adminQ["listPeers"].([]any)
	if len(peers) != 1 {
		t.Fatalf("listPeers = %v, want 1 entry", peers)
	}
	if got := peers[0].(map[string]any)["id"]; got != "p1" {
		t.Errorf("listPeers[0].id = %v, want p1", got)
	}

	res = graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `mutation { admin { forgetPeer(id: "p9") { id } } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("mutation execute: %v", res.Errors)
	}
	data, _ = res.Data.(map[string]any)
	adminM, _ := data["admin"].(map[string]any)
	forgot, _ := adminM["forgetPeer"].(map[string]any)
	if forgot["id"] != "p9" {
		t.Errorf("forgetPeer.id = %v, want p9", forgot["id"])
	}
}

// TestRenderGraphQLRuntime_NestedSubgroup verifies recursive Group
// descent: a Query group contains a sub-Group whose container type
// is named with the parent path joined.
func TestRenderGraphQLRuntime_NestedSubgroup(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "greeter",
		Version:    "v1",
		OriginKind: ir.KindGraphQL,
		Types:      map[string]*ir.Type{},
		Groups: []*ir.OperationGroup{{
			Name: "greeter",
			Kind: ir.OpQuery,
			Groups: []*ir.OperationGroup{{
				Name: "v1",
				Kind: ir.OpQuery,
				Operations: []*ir.Operation{{
					Name:   "ping",
					Kind:   ir.OpQuery,
					Output: &ir.TypeRef{Builtin: ir.ScalarString},
				}},
			}},
		}},
	}
	ir.PopulateSchemaIDs(svc)

	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Groups[0].Groups[0].Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return "pong", nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	if schema.Type("GreeterV1QueryNamespace") == nil {
		t.Errorf("expected GreeterV1QueryNamespace in schema; types: %v", schemaTypeNames(schema))
	}

	res := graphql.Do(graphql.Params{
		Schema:        *schema,
		RequestString: `{ greeter { v1 { ping } } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("execute: %v", res.Errors)
	}
	data, _ := res.Data.(map[string]any)
	g, _ := data["greeter"].(map[string]any)
	v, _ := g["v1"].(map[string]any)
	if v["ping"] != "pong" {
		t.Errorf("ping = %v, want pong", v["ping"])
	}
}

// TestRenderGraphQLRuntime_SubscriptionGroupFlattens verifies
// graphql-go's "no nested objects under Subscription" limitation:
// a subscription-rooted Group's operations land at the root with
// `<group>_<op>` names.
func TestRenderGraphQLRuntime_SubscriptionGroupFlattens(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "events",
		Version:    "v1",
		OriginKind: ir.KindGraphQL,
		Types:      map[string]*ir.Type{},
		Groups: []*ir.OperationGroup{{
			Name: "events",
			Kind: ir.OpSubscription,
			Operations: []*ir.Operation{{
				Name:   "tick",
				Kind:   ir.OpSubscription,
				Output: &ir.TypeRef{Builtin: ir.ScalarString},
			}},
		}},
	}
	ir.PopulateSchemaIDs(svc)

	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Groups[0].Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return "tick", nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	subRoot := schema.SubscriptionType()
	if subRoot == nil {
		t.Fatal("expected Subscription root type")
	}
	if _, ok := subRoot.Fields()["events_tick"]; !ok {
		var names []string
		for k := range subRoot.Fields() {
			names = append(names, k)
		}
		t.Errorf("expected events_tick on Subscription, got %v", names)
	}
}

// TestRenderGraphQLRuntime_DispatchAtCallTime verifies the
// resolver looks up Dispatchers by SchemaID *at call time*, not at
// schema-build time. Replacing a dispatcher between build and
// execute should be observable.
func TestRenderGraphQLRuntime_DispatchAtCallTime(t *testing.T) {
	svc := &ir.Service{
		Namespace:  "x",
		Version:    "v1",
		OriginKind: ir.KindProto,
		Types:      map[string]*ir.Type{},
		Operations: []*ir.Operation{{
			Name:   "ping",
			Kind:   ir.OpQuery,
			Output: &ir.TypeRef{Builtin: ir.ScalarString},
		}},
	}
	ir.PopulateSchemaIDs(svc)
	registry := ir.NewDispatchRegistry()
	registry.Set(svc.Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return "v1", nil
	}))

	schema, err := RenderGraphQLRuntime([]*ir.Service{svc}, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}

	// Swap the dispatcher post-build.
	registry.Set(svc.Operations[0].SchemaID, ir.DispatcherFunc(func(ctx context.Context, args map[string]any) (any, error) {
		return "v2", nil
	}))

	res := graphql.Do(graphql.Params{Schema: *schema, RequestString: `{ ping }`})
	if len(res.Errors) > 0 {
		t.Fatalf("execute: %v", res.Errors)
	}
	if v := res.Data.(map[string]any)["ping"]; v != "v2" {
		t.Errorf("ping = %v, want v2 (post-swap)", v)
	}

	// Deleting the dispatcher must surface a CodeInternal error
	// rather than a panic.
	registry.Delete(svc.Operations[0].SchemaID)
	res = graphql.Do(graphql.Params{Schema: *schema, RequestString: `{ ping }`})
	if len(res.Errors) == 0 {
		t.Fatalf("expected error after dispatcher delete; got data %v", res.Data)
	}
	if !strings.Contains(res.Errors[0].Error(), "no dispatcher") {
		t.Errorf("error message = %q, want to contain 'no dispatcher'", res.Errors[0].Error())
	}
}

// TestRenderGraphQLRuntime_FieldCollision verifies field-name
// collisions across services share the root namespace and fail the
// build — matches the existing buildSchemaLocked posture.
func TestRenderGraphQLRuntime_FieldCollision(t *testing.T) {
	mk := func(ns string) *ir.Service {
		svc := &ir.Service{
			Namespace:  ns,
			Version:    "v1",
			OriginKind: ir.KindProto,
			Types:      map[string]*ir.Type{},
			Operations: []*ir.Operation{{
				Name:   "ping",
				Kind:   ir.OpQuery,
				Output: &ir.TypeRef{Builtin: ir.ScalarString},
			}},
		}
		ir.PopulateSchemaIDs(svc)
		return svc
	}
	registry := ir.NewDispatchRegistry()
	_, err := RenderGraphQLRuntime([]*ir.Service{mk("a"), mk("b")}, registry, RuntimeOptions{})
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error %q does not mention collision", err.Error())
	}
}

// TestRenderGraphQLRuntime_EmptyServices yields a stub schema with
// a `_status` field — same posture as buildSchemaLocked when no
// services are registered.
func TestRenderGraphQLRuntime_EmptyServices(t *testing.T) {
	registry := ir.NewDispatchRegistry()
	schema, err := RenderGraphQLRuntime(nil, registry, RuntimeOptions{})
	if err != nil {
		t.Fatalf("RenderGraphQLRuntime: %v", err)
	}
	res := graphql.Do(graphql.Params{Schema: *schema, RequestString: `{ _status }`})
	if len(res.Errors) > 0 {
		t.Fatalf("execute: %v", res.Errors)
	}
	if v := res.Data.(map[string]any)["_status"]; v != "no services registered" {
		t.Errorf("_status = %v, want 'no services registered'", v)
	}
}

func schemaTypeNames(s *graphql.Schema) []string {
	names := []string{}
	for n := range s.TypeMap() {
		names = append(names, n)
	}
	return names
}
