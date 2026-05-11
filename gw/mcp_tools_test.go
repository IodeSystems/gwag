package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// newMCPToolsFixture stands up a gateway with the minimal OpenAPI
// spec ingested under namespace `things`, so the schema tools have
// real (and predictable) IR to walk: ops getThing (Query) +
// createThing (Mutation).
func newMCPToolsFixture(t *testing.T) *Gateway {
	t.Helper()
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("things")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	// Force initial schema build so MCPQuery sees a non-nil schema;
	// the bench fixtures use the same pattern.
	gw.mu.Lock()
	if err := gw.assembleLocked(); err != nil {
		gw.mu.Unlock()
		t.Fatalf("assembleLocked: %v", err)
	}
	gw.mu.Unlock()
	return gw
}

func TestMCPSchemaList_DefaultDenyEmpty(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if rows := gw.MCPSchemaList(); len(rows) != 0 {
		t.Fatalf("default-deny: got %d rows, want 0: %+v", len(rows), rows)
	}
}

func TestMCPSchemaList_IncludeExposesOps(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"things.*"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	rows := gw.MCPSchemaList()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// Sorted alphabetically by path.
	if rows[0].Path != "things.createThing" || rows[0].Kind != SchemaOpMutation {
		t.Errorf("rows[0]=%+v", rows[0])
	}
	if rows[1].Path != "things.getThing" || rows[1].Kind != SchemaOpQuery {
		t.Errorf("rows[1]=%+v", rows[1])
	}
	if rows[0].Namespace != "things" {
		t.Errorf("namespace=%q want things", rows[0].Namespace)
	}
}

func TestMCPSchemaList_AutoIncludeWithExclude(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		AutoInclude: true,
		Exclude:     []string{"*.create*"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	rows := gw.MCPSchemaList()
	if len(rows) != 1 || rows[0].Path != "things.getThing" {
		t.Fatalf("AutoInclude + Exclude(*.create*): %+v want only things.getThing", rows)
	}
}

func TestMCPSchemaSearch_PathGlobFilter(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"**"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	got, err := gw.MCPSchemaSearch(SchemaSearchInput{PathGlob: "things.get*"})
	if err != nil {
		t.Fatalf("MCPSchemaSearch: %v", err)
	}
	if len(got) != 1 || got[0].Path != "things.getThing" {
		t.Fatalf("got %+v want one getThing", got)
	}
	if got[0].Kind != SchemaOpQuery {
		t.Errorf("kind=%q want Query", got[0].Kind)
	}
	if len(got[0].Args) == 0 {
		t.Errorf("expected getThing to have at least one arg (id), got %+v", got[0].Args)
	}
}

func TestMCPSchemaSearch_RegexAcrossNameArgsDoc(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"**"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	// `id` is an arg name on getThing but not createThing.
	got, err := gw.MCPSchemaSearch(SchemaSearchInput{Regex: "^id$"})
	if err != nil {
		t.Fatalf("MCPSchemaSearch: %v", err)
	}
	if len(got) != 1 || got[0].Path != "things.getThing" {
		t.Fatalf("regex ^id$ matched: %+v want only getThing", got)
	}
}

func TestMCPSchemaSearch_RejectsInvalidRegex(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	_, err := gw.MCPSchemaSearch(SchemaSearchInput{Regex: "([invalid"})
	if err == nil {
		t.Fatal("expected error on invalid regex")
	}
}

func TestMCPSchemaSearch_EmptyInputReturnsAllAllowed(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"things.getThing"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	got, err := gw.MCPSchemaSearch(SchemaSearchInput{})
	if err != nil {
		t.Fatalf("MCPSchemaSearch: %v", err)
	}
	if len(got) != 1 || got[0].Path != "things.getThing" {
		t.Fatalf("empty input + Include(things.getThing): %+v", got)
	}
}

func TestMCPSchemaSearch_ArgShapeRendersTypes(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	got, err := gw.MCPSchemaSearch(SchemaSearchInput{PathGlob: "things.getThing"})
	if err != nil {
		t.Fatalf("MCPSchemaSearch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	// getThing has one required string arg `id` (OpenAPI path param).
	var idArg *SchemaSearchArg
	for i := range got[0].Args {
		if got[0].Args[i].Name == "id" {
			idArg = &got[0].Args[i]
			break
		}
	}
	if idArg == nil {
		t.Fatalf("id arg missing: %+v", got[0].Args)
	}
	if !idArg.Required {
		t.Errorf("id should be Required: %+v", idArg)
	}
	if !strings.HasPrefix(idArg.Type, "String") {
		t.Errorf("id type=%q want String-prefixed", idArg.Type)
	}
}

// TestMCPSchemaList_InternalNSHidden — registering under an internal
// namespace (AsInternal) keeps it out of the MCP surface regardless of
// the allowlist (the _* / Internal filter runs before the matcher).
func TestMCPSchemaList_InternalNSHidden(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(be.Close)
	// AsInternal stamps Service.Internal=true; walker skips it.
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(be.URL), As("hidden"), AsInternal()); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	// AutoInclude on, no exclusions — still shouldn't surface.
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{AutoInclude: true}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	rows := gw.MCPSchemaList()
	for _, r := range rows {
		if r.Namespace == "hidden" {
			t.Errorf("AsInternal namespace `hidden` leaked into MCP surface: %+v", r)
		}
	}
}

func TestMCPSchemaExpand_OpReturnsClosure(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	res, err := gw.MCPSchemaExpand("things.getThing")
	if err != nil {
		t.Fatalf("MCPSchemaExpand: %v", err)
	}
	if res.Op == nil {
		t.Fatal("expected Op populated")
	}
	if res.Op.Path != "things.getThing" || res.Op.Kind != SchemaOpQuery {
		t.Errorf("op=%+v", res.Op)
	}
	if res.Op.OutputType == "" {
		t.Errorf("OutputType empty")
	}
	// The op returns an inline anonymous object — IR registers it as
	// a named Type, so the closure should carry at least one Type.
	if len(res.Types) == 0 {
		t.Errorf("expected non-empty Types closure: %+v", res.Types)
	}
}

func TestMCPSchemaExpand_DeniesByAllowlist(t *testing.T) {
	gw := newMCPToolsFixture(t)
	// No SetMCPConfig — default-deny.
	_, err := gw.MCPSchemaExpand("things.getThing")
	if err == nil {
		t.Fatal("default-deny should reject expand")
	}
	if !strings.Contains(err.Error(), "allowlist denies") {
		t.Errorf("err=%v want 'allowlist denies' phrasing", err)
	}
}

func TestMCPSchemaExpand_UnknownTargetErrors(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	_, err := gw.MCPSchemaExpand("things.doesNotExist")
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

func TestMCPSchemaExpand_TypeName(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	// First find a named type via expand-on-op so the test doesn't
	// hardcode IR-private naming.
	res, err := gw.MCPSchemaExpand("things.getThing")
	if err != nil {
		t.Fatalf("seed expand: %v", err)
	}
	if len(res.Types) == 0 {
		t.Skipf("op has no named types (closure empty); skipping type-expand round-trip")
	}
	target := res.Types[0].Name
	tres, err := gw.MCPSchemaExpand(target)
	if err != nil {
		t.Fatalf("expand by type %q: %v", target, err)
	}
	if tres.Type == nil || tres.Type.Name != target {
		t.Fatalf("type expand result=%+v want type %q", tres, target)
	}
}

func TestMCPQuery_NoSchemaErrors(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	_, err := gw.MCPQuery(context.Background(), MCPQueryInput{Query: "{ __typename }"})
	if err == nil {
		t.Fatal("expected error when no schema is loaded")
	}
}

func TestMCPQuery_EmptyInputRejected(t *testing.T) {
	gw := newMCPToolsFixture(t)
	_, err := gw.MCPQuery(context.Background(), MCPQueryInput{})
	if err == nil {
		t.Fatal("expected error when query is empty")
	}
}

func TestMCPQuery_WrapsResponseInEvents(t *testing.T) {
	gw := newMCPToolsFixture(t)
	res, err := gw.MCPQuery(context.Background(), MCPQueryInput{
		Query: "{ __typename }",
	})
	if err != nil {
		t.Fatalf("MCPQuery: %v", err)
	}
	if res.Response == nil {
		t.Fatal("Response must be populated")
	}
	if res.Events.Level != "none" {
		t.Errorf("Events.Level=%q, want none in v1", res.Events.Level)
	}
	if res.Events.Channels == nil || len(res.Events.Channels) != 0 {
		t.Errorf("Events.Channels=%+v, want non-nil empty slice", res.Events.Channels)
	}
}

func TestMCPQuery_VariablesPassThrough(t *testing.T) {
	gw := newMCPToolsFixture(t)
	// Use the introspection schema query — it has no required vars,
	// but graphql.Do will accept and ignore extras. The point is the
	// VariableValues map round-trips into the executor without
	// crashing.
	res, err := gw.MCPQuery(context.Background(), MCPQueryInput{
		Query:     "query Q { __typename }",
		Variables: map[string]any{"ignored": 42},
	})
	if err != nil {
		t.Fatalf("MCPQuery: %v", err)
	}
	if res.Response == nil {
		t.Fatal("Response must be populated")
	}
}

func TestMCPQuery_OperationNameDispatch(t *testing.T) {
	gw := newMCPToolsFixture(t)
	q := `
query First { __typename }
query Second { __typename }
`
	// Without OperationName, graphql.Do errors on multi-operation docs.
	// With OperationName, the named op runs.
	res, err := gw.MCPQuery(context.Background(), MCPQueryInput{
		Query:         q,
		OperationName: "Second",
	})
	if err != nil {
		t.Fatalf("MCPQuery: %v", err)
	}
	if res.Response == nil {
		t.Fatal("Response must be populated")
	}
}

// TestMCPSchemaList_ProtoOpNamesLowerCamel pins that proto-ingested
// services surface their ops with the same lowerCamel field names the
// GraphQL renderer emits, not the PascalCase IR op.Name. Without this
// the agent's `schema_list → query` chain would build queries against
// a field that doesn't exist in the schema.
func TestMCPSchemaList_ProtoOpNamesLowerCamel(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
		To(lis.Addr().String()),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{AutoInclude: true}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	rows := gw.MCPSchemaList()
	found := map[string]SchemaOpKind{}
	for _, r := range rows {
		if r.Namespace == "greeter" {
			found[r.Path] = r.Kind
		}
	}
	if _, ok := found["greeter.hello"]; !ok {
		t.Errorf("expected path greeter.hello in: %+v", found)
	}
	if _, ok := found["greeter.Hello"]; ok {
		t.Errorf("PascalCase IR name leaked into MCP surface: %+v", found)
	}
	// Same path threads through expand and the allowlist matcher.
	if res, err := gw.MCPSchemaExpand("greeter.hello"); err != nil {
		t.Errorf("expand greeter.hello: %v", err)
	} else if res.Op == nil || res.Op.Path != "greeter.hello" {
		t.Errorf("expand op=%+v", res.Op)
	}
}

// Sanity: the order produced by MCPSchemaList is deterministic across
// runs (sorted by path). Skip the assert if there's only one row.
func TestMCPSchemaList_DeterministicOrder(t *testing.T) {
	gw := newMCPToolsFixture(t)
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{Include: []string{"**"}}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	var first []string
	for run := 0; run < 3; run++ {
		var paths []string
		for _, r := range gw.MCPSchemaList() {
			paths = append(paths, r.Path)
		}
		if run == 0 {
			first = paths
			continue
		}
		if !sort.StringsAreSorted(paths) {
			t.Errorf("run %d: not sorted: %v", run, paths)
		}
		if len(paths) != len(first) {
			t.Errorf("run %d: length drift %d vs %d", run, len(paths), len(first))
		}
	}
}
