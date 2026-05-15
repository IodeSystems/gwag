package ir

import (
	"encoding/json"
	"testing"
)

// TestIngestMCP_ToolsBecomeMutations pins the core mapping: each tool
// is an OpMutation, inputSchema properties flatten to Args, and a
// string-enum property synthesises an enum Type.
func TestIngestMCP_ToolsBecomeMutations(t *testing.T) {
	data := json.RawMessage(`{"tools":[
	  {"name":"get_weather","description":"Weather","inputSchema":{"type":"object",
	    "properties":{"location":{"type":"string"},"units":{"type":"string","enum":["c","f"]},"days":{"type":"integer"}},
	    "required":["location"]}}
	]}`)
	svc, err := IngestMCP(data)
	if err != nil {
		t.Fatalf("IngestMCP: %v", err)
	}
	if svc.OriginKind != KindMCP {
		t.Errorf("OriginKind = %v; want KindMCP", svc.OriginKind)
	}
	if len(svc.Operations) != 1 {
		t.Fatalf("got %d operations; want 1", len(svc.Operations))
	}
	op := svc.Operations[0]
	if op.Kind != OpMutation {
		t.Errorf("op kind = %v; want OpMutation", op.Kind)
	}
	if op.Name != "get_weather" {
		t.Errorf("op name = %q; want get_weather", op.Name)
	}
	origin, ok := op.Origin.(MCPToolOrigin)
	if !ok || origin.ToolName != "get_weather" {
		t.Errorf("op origin = %#v; want MCPToolOrigin{get_weather}", op.Origin)
	}
	byName := map[string]*Arg{}
	for _, a := range op.Args {
		byName[a.Name] = a
	}
	if a := byName["location"]; a == nil || !a.Required || a.Type.Builtin != ScalarString {
		t.Errorf("location arg wrong: %#v", a)
	}
	if a := byName["days"]; a == nil || a.Type.Builtin != ScalarInt64 {
		t.Errorf("days arg wrong: %#v", a)
	}
	units := byName["units"]
	if units == nil || units.Type.Named == "" {
		t.Fatalf("units arg should reference a synthesised enum: %#v", units)
	}
	enum := svc.Types[units.Type.Named]
	if enum == nil || enum.TypeKind != TypeEnum || len(enum.Enum) != 2 {
		t.Errorf("synthesised enum wrong: %#v", enum)
	}
}

// TestIngestMCP_NameSanitisation pins that tool names with characters
// invalid in GraphQL identifiers are projected to valid ones, and a
// leading digit is guarded with an underscore.
func TestIngestMCP_NameSanitisation(t *testing.T) {
	data := json.RawMessage(`{"tools":[
	  {"name":"get-weather","inputSchema":{"type":"object"}},
	  {"name":"2fa.verify","inputSchema":{"type":"object"}}
	]}`)
	svc, err := IngestMCP(data)
	if err != nil {
		t.Fatalf("IngestMCP: %v", err)
	}
	got := map[string]string{}
	for _, op := range svc.Operations {
		origin, _ := op.Origin.(MCPToolOrigin)
		got[origin.ToolName] = op.Name
	}
	if got["get-weather"] != "getweather" {
		t.Errorf("get-weather → %q; want getweather", got["get-weather"])
	}
	if got["2fa.verify"] != "_2faverify" {
		t.Errorf("2fa.verify → %q; want _2faverify", got["2fa.verify"])
	}
}

// TestIngestMCP_CollisionRejected pins that two tools sanitising to
// the same identifier are a hard error rather than a silent drop.
func TestIngestMCP_CollisionRejected(t *testing.T) {
	data := json.RawMessage(`{"tools":[
	  {"name":"do-thing","inputSchema":{"type":"object"}},
	  {"name":"do.thing","inputSchema":{"type":"object"}}
	]}`)
	if _, err := IngestMCP(data); err == nil {
		t.Fatal("expected a collision error for do-thing / do.thing")
	}
}
