package ir

import (
	"encoding/json"
	"fmt"
	"sort"
)

// MCPToolOrigin is stamped on Operation.Origin for MCP-ingested
// operations. It carries the upstream tool's wire name — Operation.Name
// is the GraphQL-sanitised form, which the dispatcher can't pass back
// to `tools/call`.
//
// Stability: stable
type MCPToolOrigin struct {
	ToolName string
}

// mcpToolsWire is the `tools/list` JSON-RPC result shape.
type mcpToolsWire struct {
	Tools []mcpToolWire `json:"tools"`
}

type mcpToolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// jsonSchemaWire is the subset of JSON Schema gwag reads from an MCP
// tool's inputSchema. `type` is decoded as `any` because JSON Schema
// permits either a string or an array of strings; mcpSchemaType
// normalises it.
type jsonSchemaWire struct {
	Type        any                        `json:"type"`
	Description string                     `json:"description"`
	Properties  map[string]json.RawMessage `json:"properties"`
	Required    []string                   `json:"required"`
	Items       json.RawMessage            `json:"items"`
	Enum        []any                      `json:"enum"`
}

// IngestMCP parses an MCP `tools/list` response into an IR Service.
// Each tool becomes an OpMutation Operation — MCP tool calls are
// side-effecting by convention, so they land on the GraphQL Mutation
// root. The tool's inputSchema (JSON Schema) flattens into Args;
// nested object / enum schemas synthesise named Types. Tool results
// are semi-structured (content blocks, optional structuredContent),
// so every operation returns the JSON-shaped Map type rather than a
// per-tool output type.
//
// Takes raw JSON so gw/ir carries no mcp-go dependency — mirrors
// IngestGraphQL.
//
// Stability: stable
func IngestMCP(data json.RawMessage) (*Service, error) {
	var wire mcpToolsWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode mcp tools/list: %w", err)
	}
	svc := &Service{
		Operations: []*Operation{},
		Types:      map[string]*Type{},
		OriginKind: KindMCP,
	}
	sort.Slice(wire.Tools, func(i, j int) bool { return wire.Tools[i].Name < wire.Tools[j].Name })
	seen := map[string]bool{}
	for _, t := range wire.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("mcp tools/list: tool with empty name")
		}
		opName := mcpOpName(t.Name)
		if seen[opName] {
			return nil, fmt.Errorf("mcp tools/list: tool name %q collides with another after sanitisation (%q)", t.Name, opName)
		}
		seen[opName] = true
		op := &Operation{
			Name:        opName,
			Kind:        OpMutation,
			Description: t.Description,
			OriginKind:  KindMCP,
			Origin:      MCPToolOrigin{ToolName: t.Name},
			// MCP tool results carry content blocks + optional
			// structuredContent — opaque to the schema. JSON-shaped
			// Map renders as the `JSON` scalar in SDL and resolves to
			// the gateway's JSON scalar at runtime.
			Output: &TypeRef{Map: &MapType{
				KeyType:   TypeRef{Builtin: ScalarString},
				ValueType: TypeRef{Builtin: ScalarUnknown},
			}},
			OutputRequired: true,
		}
		op.Args = mcpArgsFromInputSchema(svc, opName, t.InputSchema)
		svc.Operations = append(svc.Operations, op)
	}
	return svc, nil
}

// mcpOpName projects an MCP tool name into a GraphQL-valid identifier:
// non-identifier runes are dropped, and a leading digit (or an empty
// result) is prefixed with `_` so the name satisfies
// /[_A-Za-z][_0-9A-Za-z]*/.
func mcpOpName(s string) string {
	clean := sanitizeIdent(s)
	if clean == "" {
		return "_tool"
	}
	if r := clean[0]; r >= '0' && r <= '9' {
		return "_" + clean
	}
	return clean
}

// mcpArgsFromInputSchema flattens a tool's inputSchema object into a
// list of Args. A nil / non-object schema yields no args. Properties
// are visited in sorted order for deterministic output.
func mcpArgsFromInputSchema(svc *Service, opHint string, raw json.RawMessage) []*Arg {
	if len(raw) == 0 {
		return nil
	}
	var s jsonSchemaWire
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if mcpSchemaType(s.Type) != "object" || len(s.Properties) == 0 {
		return nil
	}
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]*Arg, 0, len(keys))
	for _, k := range keys {
		ref, repeated, desc := mcpTypeRef(svc, opHint+pascalIdent(k), s.Properties[k])
		args = append(args, &Arg{
			Name:        k,
			Type:        ref,
			Repeated:    repeated,
			Required:    required[k],
			Description: desc,
		})
	}
	return args
}

// mcpTypeRef maps one JSON Schema property to an IR TypeRef. Nested
// object / string-enum schemas synthesise named Types in svc.Types
// under `hint`. Returns the ref, whether it's a repeated (array)
// slot, and the property description.
func mcpTypeRef(svc *Service, hint string, raw json.RawMessage) (ref TypeRef, repeated bool, desc string) {
	if len(raw) == 0 {
		return TypeRef{Builtin: ScalarString}, false, ""
	}
	var s jsonSchemaWire
	if err := json.Unmarshal(raw, &s); err != nil {
		return TypeRef{Builtin: ScalarString}, false, ""
	}
	desc = s.Description
	switch mcpSchemaType(s.Type) {
	case "string":
		if len(s.Enum) > 0 {
			return TypeRef{Named: mcpSynthEnum(svc, hint, s)}, false, desc
		}
		return TypeRef{Builtin: ScalarString}, false, desc
	case "boolean":
		return TypeRef{Builtin: ScalarBool}, false, desc
	case "integer":
		return TypeRef{Builtin: ScalarInt64}, false, desc
	case "number":
		return TypeRef{Builtin: ScalarDouble}, false, desc
	case "array":
		itemRef, _, _ := mcpTypeRef(svc, hint+"Item", s.Items)
		return itemRef, true, desc
	case "object":
		if len(s.Properties) > 0 {
			return TypeRef{Named: mcpSynthObject(svc, hint, s)}, false, desc
		}
		// Free-form object → JSON-shaped Map.
		return TypeRef{Map: &MapType{
			KeyType:   TypeRef{Builtin: ScalarString},
			ValueType: TypeRef{Builtin: ScalarUnknown},
		}}, false, desc
	default:
		// Unknown / absent type → JSON-shaped Map.
		return TypeRef{Map: &MapType{
			KeyType:   TypeRef{Builtin: ScalarString},
			ValueType: TypeRef{Builtin: ScalarUnknown},
		}}, false, desc
	}
}

// mcpSynthObject registers a TypeObject for an inline object schema
// under `name` and returns that name. Pre-registers the placeholder
// before recursing so a (rare) cyclic property shape terminates.
func mcpSynthObject(svc *Service, name string, s jsonSchemaWire) string {
	if name == "" {
		name = "McpObject"
	}
	if existing, ok := svc.Types[name]; ok && existing.TypeKind == TypeObject {
		return name
	}
	t := &Type{
		Name:        name,
		TypeKind:    TypeObject,
		Description: s.Description,
		OriginKind:  KindMCP,
	}
	svc.Types[name] = t
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ref, repeated, desc := mcpTypeRef(svc, name+pascalIdent(k), s.Properties[k])
		t.Fields = append(t.Fields, &Field{
			Name:        k,
			JSONName:    k,
			Type:        ref,
			Repeated:    repeated,
			Required:    required[k],
			Description: desc,
			OneofIndex:  -1,
		})
	}
	return name
}

// mcpSynthEnum registers a TypeEnum for an inline string-with-enum
// schema and returns the chosen name. Non-string enum values are
// skipped — GraphQL enum values are bare identifiers.
func mcpSynthEnum(svc *Service, name string, s jsonSchemaWire) string {
	if name == "" {
		name = "McpEnum"
	}
	if existing, ok := svc.Types[name]; ok && existing.TypeKind == TypeEnum {
		return name
	}
	t := &Type{
		Name:        name,
		TypeKind:    TypeEnum,
		Description: s.Description,
		OriginKind:  KindMCP,
	}
	for _, v := range s.Enum {
		if str, ok := v.(string); ok {
			t.Enum = append(t.Enum, EnumValue{Name: str})
		}
	}
	svc.Types[name] = t
	return name
}

// mcpSchemaType normalises JSON Schema's `type` keyword. JSON Schema
// allows either a string or an array of strings (e.g. ["string",
// "null"] for a nullable field); the array form returns the first
// non-"null" entry.
func mcpSchemaType(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}
