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
//
// Stability: stable
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
		t := openapiSchemaToType(svc, k, ref)
		svc.Types[k] = t
	}
}

func openapiSchemaToType(svc *Service, name string, ref *openapi3.SchemaRef) *Type {
	s := ref.Value
	t := &Type{
		Name:        name,
		Description: s.Description,
		OriginKind:  KindOpenAPI,
		Origin:      ref,
	}
	// Pre-register so recursive openapiPropToField calls that synthesize
	// inline objects naming themselves "<name>_<field>" don't see the
	// containing type as missing and re-enter ingest.
	svc.Types[name] = t
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
		if s.Discriminator != nil {
			t.DiscriminatorProperty = s.Discriminator.PropertyName
			if len(s.Discriminator.Mapping) > 0 {
				t.DiscriminatorMapping = map[string]string{}
				for k, ref := range s.Discriminator.Mapping {
					// Mapping values come in as "#/components/schemas/X"
					// or bare schema names. Strip to the leaf so the
					// canonical field stores variant-Name strings only.
					// MappingRef wraps SchemaRef — the .Ref string is
					// the JSON-serialised form ("foo" or
					// "#/components/schemas/Foo").
					parts := strings.Split(ref.Ref, "/")
					t.DiscriminatorMapping[k] = parts[len(parts)-1]
				}
			}
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
		childHint := name + pascalIdent(k)
		t.Fields = append(t.Fields, openapiPropToField(svc, childHint, k, props[k], required[k]))
	}
	return t
}

// openapiPropToField projects a schema slot (property, parameter,
// body, response) into a Field. pathHint is the deterministic name
// the synthesizer uses if the slot is an anonymous inline object —
// callers compute it from the surrounding context (operationId+
// "Body", parent type name + property name, etc.) so synthesized
// names are stable across runs and human-debuggable.
func openapiPropToField(svc *Service, pathHint, name string, ref *openapi3.SchemaRef, required bool) *Field {
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
	// Inline oneOf / anyOf with $ref'd variants: synthesise a
	// deterministic union Type ("AOrB") in svc.Types and point the
	// field at it. Anonymous (non-$ref) variants fall through to the
	// scalar fallback below — IR has no synthesised-name story for
	// them yet, so the renderer-side projection is JSON-shaped.
	if name := synthesizeInlineUnion(svc, s); name != "" {
		f.Type = TypeRef{Named: name}
		return f
	}
	if pt := primaryOpenAPIType(s); pt != "" {
		switch pt {
		case "string":
			switch {
			case s.Format == "binary" || s.Format == "byte":
				// OpenAPI's spec-blessed file-upload encoding: in a
				// multipart/form-data schema, type:string + format:binary
				// is "this property is a file". format:byte (base64 in
				// JSON contexts) is treated identically — both map to the
				// Upload scalar; the dispatcher chooses the wire encoding.
				f.Type = TypeRef{Builtin: ScalarUpload}
			case len(s.Enum) > 0:
				f.Type = TypeRef{Named: synthesizeInlineEnum(svc, pathHint, s)}
			default:
				f.Type = TypeRef{Builtin: ScalarString}
			}
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
			} else if s.Items != nil {
				item := openapiPropToField(svc, pathHint+"Item", "item", s.Items, false)
				f.Type = item.Type
			} else {
				f.Type = TypeRef{Builtin: ScalarString}
			}
		case "object":
			if s.AdditionalProperties.Schema != nil {
				val := openapiPropToField(svc, pathHint+"Value", "v", s.AdditionalProperties.Schema, false)
				f.Type = TypeRef{Map: &MapType{
					KeyType:   TypeRef{Builtin: ScalarString},
					ValueType: val.Type,
				}}
			} else if len(s.Properties) > 0 {
				f.Type = TypeRef{Named: synthesizeInlineObject(svc, pathHint, s)}
			} else {
				f.Type = TypeRef{Builtin: ScalarString}
			}
		}
	}
	return f
}

// synthesizeInlineObject registers a TypeObject for an anonymous
// inline schema under the supplied name and returns that name.
// Pre-registers the placeholder before recursing so cyclic property
// shapes (rare in OpenAPI but possible) terminate.
func synthesizeInlineObject(svc *Service, name string, s *openapi3.Schema) string {
	if name == "" {
		name = "AnonymousObject"
	}
	if existing, ok := svc.Types[name]; ok && existing.TypeKind == TypeObject {
		return name
	}
	t := &Type{
		Name:        name,
		TypeKind:    TypeObject,
		Description: s.Description,
		OriginKind:  KindOpenAPI,
	}
	svc.Types[name] = t
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	required := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		required[r] = true
	}
	for _, k := range keys {
		childHint := name + pascalIdent(k)
		t.Fields = append(t.Fields, openapiPropToField(svc, childHint, k, s.Properties[k], required[k]))
	}
	return name
}

// synthesizeInlineEnum registers a TypeEnum for an anonymous inline
// string-with-enum schema and returns the chosen name.
func synthesizeInlineEnum(svc *Service, name string, s *openapi3.Schema) string {
	if name == "" {
		name = "AnonymousEnum"
	}
	if existing, ok := svc.Types[name]; ok && existing.TypeKind == TypeEnum {
		return name
	}
	t := &Type{
		Name:        name,
		TypeKind:    TypeEnum,
		Description: s.Description,
		OriginKind:  KindOpenAPI,
	}
	for _, v := range s.Enum {
		if str, ok := v.(string); ok {
			t.Enum = append(t.Enum, EnumValue{Name: str})
		}
	}
	svc.Types[name] = t
	return name
}

// pascalIdent uppercases the leading rune of an identifier and strips
// non-alphanumeric runes. Used to compose synthesized inline-type
// names from parent + child path segments ("createThing" + "Body" →
// "createThingBody"). Multi-word camelCase inputs aren't normalized;
// matches the IR convention of carrying source-format names verbatim.
func pascalIdent(s string) string {
	clean := sanitizeIdent(s)
	if clean == "" {
		return ""
	}
	r := []rune(clean)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}

// sanitizeIdent keeps the leading-case of s intact while stripping
// runes that aren't valid in identifiers. Used to project an
// operationId into a synthesized-type-name root segment.
func sanitizeIdent(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			out = append(out, r)
		}
	}
	return string(out)
}

// synthesizeInlineUnion examines an inline schema for oneOf/anyOf
// with $ref'd named variants. When all variants resolve to bare
// schema names, registers a TypeUnion entry under a deterministic
// "AOrB"-shaped synthesised name and returns that name; the caller
// uses it as a TypeRef.Named. Returns "" when the schema isn't an
// inline union, has no variants, or has any anonymous (non-$ref)
// variant that the IR can't safely name.
func synthesizeInlineUnion(svc *Service, s *openapi3.Schema) string {
	if s == nil || svc == nil {
		return ""
	}
	if len(s.OneOf) == 0 && len(s.AnyOf) == 0 {
		return ""
	}
	variants := s.OneOf
	if len(variants) == 0 {
		variants = s.AnyOf
	}
	parts := make([]string, 0, len(variants))
	for _, v := range variants {
		if v == nil || v.Ref == "" {
			return ""
		}
		segs := strings.Split(v.Ref, "/")
		parts = append(parts, segs[len(segs)-1])
	}
	if len(parts) == 0 {
		return ""
	}
	name := strings.Join(parts, "Or")
	if existing, ok := svc.Types[name]; ok {
		// Already synthesised (or a real component schema collides
		// with the synthesised name — rare; defer to the existing
		// entry rather than overwrite).
		_ = existing
		return name
	}
	t := &Type{
		Name:       name,
		TypeKind:   TypeUnion,
		OriginKind: KindOpenAPI,
		Variants:   parts,
	}
	if s.Discriminator != nil {
		t.DiscriminatorProperty = s.Discriminator.PropertyName
		if len(s.Discriminator.Mapping) > 0 {
			t.DiscriminatorMapping = map[string]string{}
			for k, ref := range s.Discriminator.Mapping {
				segs := strings.Split(ref.Ref, "/")
				t.DiscriminatorMapping[k] = segs[len(segs)-1]
			}
		}
	}
	svc.Types[name] = t
	return name
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
		svc.Operations = append(svc.Operations, ingestOpenAPIOp(svc, v.method, path, v.op))
	}
}

func ingestOpenAPIOp(svc *Service, method, path string, op *openapi3.Operation) *Operation {
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

	// Path/query/header/cookie params → Args. pathHint is built from
	// the operation name so synthesized inline schemas land under
	// stable, traceable names.
	opHint := sanitizeIdent(out.Name)
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
			arg.Type = openapiPropToField(svc, opHint+pascalIdent(p.Name), p.Name, p.Schema, p.Required).Type
		} else {
			arg.Type = TypeRef{Builtin: ScalarString}
		}
		out.Args = append(out.Args, arg)
	}

	// Body → "body" arg, located OpenAPILocation="body".
	//
	// multipart/form-data is checked before application/json: when both
	// are declared the gateway uses the multipart shape (uploads are
	// the unique value-add and JSON is always representable elsewhere).
	// The form-data schema's properties are flattened to top-level
	// Args; binary properties land as TypeRef{Builtin: ScalarUpload}.
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		body := op.RequestBody.Value
		if mt, ok := body.Content["multipart/form-data"]; ok && mt.Schema != nil && mt.Schema.Value != nil {
			ingestOpenAPIFormDataBody(svc, out, opHint, mt.Schema.Value, body)
		} else if mt, ok := body.Content["application/json"]; ok && mt.Schema != nil {
			out.Args = append(out.Args, &Arg{
				Name:            "body",
				Type:            openapiPropToField(svc, opHint+"Body", "body", mt.Schema, body.Required).Type,
				Required:        body.Required,
				Description:     body.Description,
				OpenAPILocation: "body",
			})
		}
	}

	// Response: prefer 200, then 201, then default. One TypeRef.
	if op.Responses != nil {
		out.Output = openapiResponseTypeRef(svc, opHint, op.Responses)
	}
	return out
}

// ingestOpenAPIFormDataBody flattens a multipart/form-data request
// body schema into top-level Args on `out`. Each property becomes one
// Arg with OpenAPILocation="formdata"; properties typed
// string/format:binary land as TypeRef{Builtin: ScalarUpload}, others
// follow the regular openapiPropToField mapping. Array-of-binary
// properties land as repeated ScalarUpload (`[Upload!]!` once required
// + ItemRequired are honored downstream).
//
// The Operation is flagged MultipartBody so the HTTP ingress decodes
// inbound requests as multipart and the dispatcher builds outbound
// multipart bodies.
//
// Property names land in declaration order alphabetically so the SDL
// is reproducible across runs (the wire format is name-keyed, not
// position-keyed, so the order is purely a presentation choice).
func ingestOpenAPIFormDataBody(svc *Service, out *Operation, opHint string, s *openapi3.Schema, body *openapi3.RequestBody) {
	out.MultipartBody = true
	if len(s.Properties) == 0 {
		return
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
	for _, k := range keys {
		propRef := s.Properties[k]
		field := openapiPropToField(svc, opHint+"FormData"+pascalIdent(k), k, propRef, required[k])
		out.Args = append(out.Args, &Arg{
			Name:            k,
			Type:            field.Type,
			Repeated:        field.Repeated,
			Required:        required[k],
			ItemRequired:    field.Repeated && required[k],
			Description:     field.Description,
			OpenAPILocation: "formdata",
		})
	}
	// body.Required / body.Description belong to the whole body, not
	// any single Arg — they're already reflected per-property via the
	// required[] set above. Keep the closure tight to avoid leaking
	// shadowed metadata.
	_ = body
}

func openapiResponseTypeRef(svc *Service, opHint string, r *openapi3.Responses) *TypeRef {
	for _, code := range []string{"200", "201"} {
		resp := r.Status(parseStatusCode(code))
		if resp != nil && resp.Value != nil {
			if mt, ok := resp.Value.Content["application/json"]; ok && mt.Schema != nil {
				ref := openapiPropToField(svc, opHint+"Response", "response", mt.Schema, false).Type
				return &ref
			}
		}
	}
	if def := r.Default(); def != nil && def.Value != nil {
		if mt, ok := def.Value.Content["application/json"]; ok && mt.Schema != nil {
			ref := openapiPropToField(svc, opHint+"Response", "response", mt.Schema, false).Type
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
