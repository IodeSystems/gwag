package ir

import (
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// IngestOpenAPI converts a kin-openapi document into a single
// ir.Service. OpenAPI doesn't carry a (namespace, version)
// coordinate natively — Info.Version is doc-level not service-
// level, and there's no "namespace" field — so the caller fills
// Service.Namespace/Version after the call (typically from the
// gateway's openAPISource key).
//
// Operations land in declaration order per path (sorted) and per
// HTTP method (GET→POST→PUT→PATCH→DELETE) so the output is
// reproducible across runs.
//
// Components.Schemas → Service.Types. Each schema becomes a Type
// (Object/Enum); $ref'd schemas resolve to TypeRef.Named pointing
// at the schema's components key. Inline (anonymous) schemas keep
// their structure but don't get a Types entry — TypeRef.Map
// handles map-shaped inlines, otherwise they fall through to
// Builtin scalars when possible.
func IngestOpenAPI(doc *openapi3.T) *Service {
	desc := ""
	if doc.Info != nil {
		desc = doc.Info.Description
	}
	svc := &Service{
		Description: desc,
		Operations:  []*Operation{},
		Types:       map[string]*Type{},
		OriginKind:  KindOpenAPI,
		Origin:      doc,
	}

	// Components first so $ref TypeRefs land in the registry
	// before operations that reference them.
	if doc.Components != nil {
		ingestOpenAPISchemas(svc, doc.Components.Schemas)
	}

	// Operations: sort path keys + per-path methods for stable output.
	if doc.Paths != nil {
		paths := doc.Paths.Map()
		keys := make([]string, 0, len(paths))
		for k := range paths {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, p := range keys {
			ingestOpenAPIPath(svc, p, paths[p])
		}
	}
	return svc
}

func ingestOpenAPISchemas(svc *Service, schemas openapi3.Schemas) {
	keys := make([]string, 0, len(schemas))
	for k := range schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ref := schemas[k]
		if ref == nil || ref.Value == nil {
			continue
		}
		t := openapiSchemaToType(k, ref)
		svc.Types[k] = t
	}
}

func openapiSchemaToType(name string, ref *openapi3.SchemaRef) *Type {
	s := ref.Value
	t := &Type{
		Name:        name,
		Description: s.Description,
		OriginKind:  KindOpenAPI,
		Origin:      ref,
	}
	// oneOf / anyOf at the schema root → TypeUnion with named
	// variants. Inline (non-$ref) variants are skipped since the IR
	// has no native synthesized-name story for them — operators
	// hitting that case should declare a named component schema.
	// Discriminator metadata stays accessible via Origin (the
	// *openapi3.SchemaRef) when the renderer needs it.
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		variants := s.OneOf
		if len(variants) == 0 {
			variants = s.AnyOf
		}
		t.TypeKind = TypeUnion
		for _, v := range variants {
			if v == nil || v.Ref == "" {
				continue
			}
			parts := strings.Split(v.Ref, "/")
			t.Variants = append(t.Variants, parts[len(parts)-1])
		}
		return t
	}
	if pt := primaryOpenAPIType(s); pt == "string" && len(s.Enum) > 0 {
		t.TypeKind = TypeEnum
		for _, v := range s.Enum {
			if str, ok := v.(string); ok {
				t.Enum = append(t.Enum, EnumValue{Name: str})
			}
		}
		return t
	}
	t.TypeKind = TypeObject
	props := s.Properties
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	for _, k := range keys {
		t.Fields = append(t.Fields, openapiPropToField(k, props[k], required[k]))
	}
	return t
}

func openapiPropToField(name string, ref *openapi3.SchemaRef, required bool) *Field {
	f := &Field{
		Name:     name,
		JSONName: name,
		Required: required,
	}
	if ref == nil || ref.Value == nil {
		f.Type = TypeRef{Builtin: ScalarString}
		return f
	}
	if ref.Ref != "" {
		// "#/components/schemas/Foo" → "Foo"
		parts := strings.Split(ref.Ref, "/")
		f.Type = TypeRef{Named: parts[len(parts)-1]}
		return f
	}
	s := ref.Value
	f.Description = s.Description
	f.Format = s.Format
	f.Pattern = s.Pattern
	if pt := primaryOpenAPIType(s); pt != "" {
		switch pt {
		case "string":
			f.Type = TypeRef{Builtin: ScalarString}
		case "boolean":
			f.Type = TypeRef{Builtin: ScalarBool}
		case "integer":
			switch s.Format {
			case "int64", "uint64":
				f.Type = TypeRef{Builtin: ScalarInt64}
			default:
				f.Type = TypeRef{Builtin: ScalarInt32}
			}
		case "number":
			switch s.Format {
			case "float":
				f.Type = TypeRef{Builtin: ScalarFloat}
			default:
				f.Type = TypeRef{Builtin: ScalarDouble}
			}
		case "array":
			f.Repeated = true
			if s.Items != nil && s.Items.Ref != "" {
				parts := strings.Split(s.Items.Ref, "/")
				f.Type = TypeRef{Named: parts[len(parts)-1]}
			} else {
				f.Type = TypeRef{Builtin: ScalarString}
			}
		case "object":
			if s.AdditionalProperties.Schema != nil {
				val := openapiPropToField("v", s.AdditionalProperties.Schema, false)
				f.Type = TypeRef{Map: &MapType{
					KeyType:   TypeRef{Builtin: ScalarString},
					ValueType: val.Type,
				}}
			} else {
				f.Type = TypeRef{Builtin: ScalarString}
			}
		}
	}
	return f
}

// primaryOpenAPIType pulls the single non-null type out of an
// OpenAPI 3.1 multi-type list (Schema.Type is now []string-shaped).
// Mirrors gw/openapi.go's primaryType but local to the ir package.
func primaryOpenAPIType(s *openapi3.Schema) string {
	if s == nil || s.Type == nil {
		return ""
	}
	var primaries []string
	for _, t := range *s.Type {
		if t != "null" {
			primaries = append(primaries, t)
		}
	}
	if len(primaries) == 1 {
		return primaries[0]
	}
	return ""
}

func ingestOpenAPIPath(svc *Service, path string, item *openapi3.PathItem) {
	verbs := []struct {
		method string
		op     *openapi3.Operation
	}{
		{"GET", item.Get},
		{"POST", item.Post},
		{"PUT", item.Put},
		{"PATCH", item.Patch},
		{"DELETE", item.Delete},
	}
	for _, v := range verbs {
		if v.op == nil {
			continue
		}
		svc.Operations = append(svc.Operations, ingestOpenAPIOp(v.method, path, v.op))
	}
}

func ingestOpenAPIOp(method, path string, op *openapi3.Operation) *Operation {
	out := &Operation{
		Name:        op.OperationID,
		Description: op.Description,
		HTTPMethod:  method,
		HTTPPath:    path,
		Tags:        op.Tags,
		OriginKind:  KindOpenAPI,
		Origin:      op,
	}
	if out.Name == "" {
		out.Name = method + path
	}
	switch strings.ToUpper(method) {
	case "GET", "HEAD":
		out.Kind = OpQuery
	default:
		out.Kind = OpMutation
	}

	// Path/query/header/cookie params → Args.
	for _, paramRef := range op.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		arg := &Arg{
			Name:            p.Name,
			Required:        p.Required,
			Description:     p.Description,
			OpenAPILocation: p.In,
		}
		if p.Schema != nil && p.Schema.Value != nil {
			arg.Type = openapiPropToField(p.Name, p.Schema, p.Required).Type
		} else {
			arg.Type = TypeRef{Builtin: ScalarString}
		}
		out.Args = append(out.Args, arg)
	}

	// Body → "body" arg, located OpenAPILocation="body".
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		body := op.RequestBody.Value
		if mt, ok := body.Content["application/json"]; ok && mt.Schema != nil {
			out.Args = append(out.Args, &Arg{
				Name:            "body",
				Type:            openapiPropToField("body", mt.Schema, body.Required).Type,
				Required:        body.Required,
				Description:     body.Description,
				OpenAPILocation: "body",
			})
		}
	}

	// Response: prefer 200, then 201, then default. One TypeRef.
	if op.Responses != nil {
		out.Output = openapiResponseTypeRef(op.Responses)
	}
	return out
}

func openapiResponseTypeRef(r *openapi3.Responses) *TypeRef {
	for _, code := range []string{"200", "201"} {
		resp := r.Status(parseStatusCode(code))
		if resp != nil && resp.Value != nil {
			if mt, ok := resp.Value.Content["application/json"]; ok && mt.Schema != nil {
				ref := openapiPropToField("response", mt.Schema, false).Type
				return &ref
			}
		}
	}
	if def := r.Default(); def != nil && def.Value != nil {
		if mt, ok := def.Value.Content["application/json"]; ok && mt.Schema != nil {
			ref := openapiPropToField("response", mt.Schema, false).Type
			return &ref
		}
	}
	return nil
}

// parseStatusCode is a tiny helper to avoid importing strconv just
// for parsing fixed strings the OpenAPI ingester knows up front.
// Returns -1 on bad input so the response lookup misses cleanly.
func parseStatusCode(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
