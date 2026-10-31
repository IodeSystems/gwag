package ir

import (
	"encoding/json"
	"sort"
	"strconv"
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
// HTTP method (GET→POST→PUT→PATCH→DELETE→QUERY, then OpenAPI 3.2
// additionalOperations sorted by method) so the output is
// reproducible across runs. HEAD / OPTIONS / TRACE / CONNECT are not
// ingested; UnsupportedOpenAPIOperations lists them.
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
		Ref:         extString(s.Extensions, xRefExtension),
		Annotations: extAnnotations(s.Extensions),
		OriginKind:  KindOpenAPI,
		Origin:      ref,
	}
	// Pre-register so recursive openapiPropToField calls that synthesize
	// inline objects naming themselves "<name>_<field>" don't see the
	// containing type as missing and re-enter ingest.
	svc.Types[name] = t
	// oneOf / anyOf at the schema root → TypeUnion. $ref'd variants use
	// their leaf name; inline object variants are synthesised as
	// "<name>VariantN" object Types. Non-object variants (scalars) can't
	// be GraphQL union members and are skipped. Discriminator metadata
	// stays accessible via Origin (the *openapi3.SchemaRef) when the
	// renderer needs it.
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		t.TypeKind = TypeUnion
		t.Variants, _ = openapiUnionVariants(svc, name, s.OneOf, s.AnyOf)
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
				// 0-based sequential numbers (per EnumValue.Number contract);
				// len(t.Enum) stays correct when non-string entries are skipped.
				t.Enum = append(t.Enum, EnumValue{Name: str, Number: int32(len(t.Enum))})
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
	// A nullable or annotated $ref: `allOf: [$ref]` (+ `nullable`), or
	// a oneOf / anyOf whose only non-null branch is one schema (the
	// 3.1 spelling `oneOf: [$ref, {type: null}]`). Project the inner
	// schema; the wrapper adds nullability and its description.
	if inner, null := openapiNullableWrapper(s); inner != nil {
		g := openapiPropToField(svc, pathHint, name, inner, required)
		g.Nullable = g.Nullable || null || s.Nullable || openapiTypeHasNull(s)
		if s.Description != "" {
			g.Description = s.Description
		}
		return g
	}
	f.Description = s.Description
	f.Ref = extString(s.Extensions, xRefExtension)
	f.Annotations = extAnnotations(s.Extensions)
	f.Format = s.Format
	f.Pattern = s.Pattern
	// A field that may be null (3.0 `nullable:true`, or 3.1 `type` list
	// containing "null") must render nullable in GraphQL even when required.
	f.Nullable = s.Nullable || openapiTypeHasNull(s)
	// Inline oneOf / anyOf: synthesise a deterministic union Type
	// ("AOrB") in svc.Types and point the field at it. $ref'd and inline
	// *object* variants are named (inline ones as "<hint>VariantN");
	// non-object variants (scalars) fall through to the scalar fallback
	// below, since a GraphQL union can't hold them.
	if union := synthesizeInlineUnion(svc, pathHint, s); union != "" {
		f.Type = TypeRef{Named: union}
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
			if name := extString(s.Extensions, ScalarExtension); validScalarName(name) {
				f.Type.ScalarName = name
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
			// Element non-nullability: a direct $ref item is a non-null named type
			// (a nullable element surfaces as an inline `nullable` schema, caught by
			// the inline branch's item.Nullable). This is what makes a list render
			// `[T!]` rather than `[T]` — without it every element is nullable, which
			// is wrong for the common `[]T` (non-pointer Go slice) case.
			if s.Items != nil && s.Items.Ref != "" {
				parts := strings.Split(s.Items.Ref, "/")
				f.Type = TypeRef{Named: parts[len(parts)-1]}
				f.ItemRequired = true
			} else if s.Items != nil {
				item := openapiPropToField(svc, pathHint+"Item", "item", s.Items, false)
				f.Type = item.Type
				f.ItemRequired = !item.Nullable
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
			// 0-based sequential numbers (per EnumValue.Number contract);
			// len(t.Enum) stays correct when non-string entries are skipped.
			t.Enum = append(t.Enum, EnumValue{Name: str, Number: int32(len(t.Enum))})
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

// isGraphQLName reports whether s is a valid GraphQL identifier — the /^[_A-Za-z][_0-9A-Za-z]*$/
// constraint graphql.NewSchema enforces on type/field/argument names. Used to keep OpenAPI
// parameters whose names can't be represented as GraphQL args (e.g. hyphenated headers) off the
// GraphQL surface instead of panicking the schema build.
func isGraphQLName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			// valid anywhere
		case r >= '0' && r <= '9':
			if i == 0 {
				return false // GraphQL names may not start with a digit
			}
		default:
			return false
		}
	}
	return true
}

// openapiUnionVariants resolves a oneOf/anyOf variant list into IR
// variant Type names. $ref'd variants use their leaf schema name;
// inline object variants are synthesised as "<hint>VariantN" object
// Types (registered in svc.Types). allRepresentable is false when a
// variant is a non-object schema (scalar / array / nested union) — a
// GraphQL union can only hold objects, so the caller may fall back to a
// JSON-shaped projection. oneOf is preferred; anyOf is the fallback.
func openapiUnionVariants(svc *Service, hint string, oneOf, anyOf []*openapi3.SchemaRef) (names []string, allRepresentable bool) {
	variants := oneOf
	if len(variants) == 0 {
		variants = anyOf
	}
	allRepresentable = true
	for i, v := range variants {
		switch {
		case v != nil && v.Ref != "":
			segs := strings.Split(v.Ref, "/")
			names = append(names, segs[len(segs)-1])
		case v != nil && v.Value != nil && isOpenAPIObjectSchema(v.Value):
			vname := hint + "Variant" + strconv.Itoa(i+1)
			openapiSchemaToType(svc, vname, v)
			names = append(names, vname)
		default:
			allRepresentable = false
		}
	}
	return names, allRepresentable
}

// isOpenAPIObjectSchema reports whether s describes an object shape (so
// it can be a GraphQL union member).
func isOpenAPIObjectSchema(s *openapi3.Schema) bool {
	return len(s.Properties) > 0 || primaryOpenAPIType(s) == "object"
}

// synthesizeInlineUnion examines an inline schema for oneOf/anyOf and,
// when every variant is representable (a $ref'd object or an inline
// object — see openapiUnionVariants), registers a TypeUnion under a
// deterministic "AOrB"-shaped name and returns it for use as a
// TypeRef.Named. hint names any inline object variants. Returns "" when
// the schema isn't an inline union, has no variants, or has a
// non-object variant the IR can't model as a union member.
func synthesizeInlineUnion(svc *Service, hint string, s *openapi3.Schema) string {
	if s == nil || svc == nil {
		return ""
	}
	if len(s.OneOf) == 0 && len(s.AnyOf) == 0 {
		return ""
	}
	names, allRepresentable := openapiUnionVariants(svc, hint, s.OneOf, s.AnyOf)
	if !allRepresentable || len(names) == 0 {
		return ""
	}
	name := strings.Join(names, "Or")
	if _, ok := svc.Types[name]; ok {
		// Already synthesised (or a real component schema collides with
		// the synthesised name — rare; defer to the existing entry).
		return name
	}
	t := &Type{
		Name:       name,
		TypeKind:   TypeUnion,
		OriginKind: KindOpenAPI,
		Variants:   names,
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
// openapiNullableWrapper returns the one schema s wraps, or nil:
// `allOf` with a single entry and no shape of its own, or `oneOf` /
// `anyOf` with exactly one branch besides `{type: null}` branches
// (or a single branch with `nullable`). null reports whether a null branch was dropped.
func openapiNullableWrapper(s *openapi3.Schema) (inner *openapi3.SchemaRef, null bool) {
	if len(s.Properties) > 0 || (s.Type != nil && len(*s.Type) > 0 && !(len(*s.Type) == 1 && (*s.Type)[0] == "null")) {
		return nil, false
	}
	switch {
	case len(s.AllOf) == 1 && len(s.OneOf) == 0 && len(s.AnyOf) == 0:
		return s.AllOf[0], false
	case len(s.AllOf) == 0 && (len(s.OneOf) == 0) != (len(s.AnyOf) == 0):
		branches := s.OneOf
		if len(branches) == 0 {
			branches = s.AnyOf
		}
		for _, b := range branches {
			if b != nil && b.Ref == "" && b.Value != nil && b.Value.Type != nil && len(*b.Value.Type) == 1 && (*b.Value.Type)[0] == "null" {
				null = true
				continue
			}
			if inner != nil {
				return nil, false
			}
			inner = b
		}
		if !null && !s.Nullable {
			return nil, false // a one-branch union stays a union
		}
		return inner, null
	}
	return nil, false
}

// OpenAPI 3.1 multi-type list (Schema.Type is now []string-shaped).
// Mirrors gw/openapi.go's primaryType but local to the ir package.
// openapiTypeHasNull reports whether an OpenAPI 3.1 type list includes
// "null" (the 3.1 spelling of nullability).
func openapiTypeHasNull(s *openapi3.Schema) bool {
	if s == nil || s.Type == nil {
		return false
	}
	for _, t := range *s.Type {
		if t == "null" {
			return true
		}
	}
	return false
}

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
		{openapi3.MethodQuery, item.Query},
	}
	for _, v := range verbs {
		if v.op == nil {
			continue
		}
		svc.Operations = append(svc.Operations, ingestOpenAPIOp(svc, v.method, path, v.op))
	}
	// OpenAPI 3.2 additionalOperations: custom HTTP methods, sorted for
	// stable output.
	for _, method := range additionalOpenAPIMethods(item) {
		svc.Operations = append(svc.Operations, ingestOpenAPIOp(svc, method, path, item.AdditionalOperations[method]))
	}
	// The same, spelled as the x-additional-operations extension (a
	// 3.0 document).
	ext := extAdditionalOperations(svc, item)
	for _, method := range sortedOpMethods(ext) {
		svc.Operations = append(svc.Operations, ingestOpenAPIOp(svc, method, path, ext[method]))
	}
}

// extAdditionalOperations decodes item's x-additional-operations
// extension, where a 3.0 document keeps QUERY and custom-method
// operations (RenderOpenAPI and the 3.0 down-conversion write it).
// $refs into the document's components are resolved, as the loader
// does for native fields. A method the Path Item has natively, or one
// a 3.0 Path Item has a field for, is left out.
func extAdditionalOperations(svc *Service, item *openapi3.PathItem) map[string]*openapi3.Operation {
	v, ok := item.Extensions[xAdditionalOperations]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	ops := map[string]*openapi3.Operation{}
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil
	}
	doc, _ := svc.Origin.(*openapi3.T)
	for m, op := range ops {
		switch {
		case op == nil, item.GetOperation(m) != nil, item.AdditionalOperations[m] != nil:
			delete(ops, m)
		case m != openapi3.MethodQuery && fixedPathItemMethod(m):
			delete(ops, m)
		default:
			resolveOperationRefs(doc, op)
		}
	}
	return ops
}

func fixedPathItemMethod(m string) bool {
	for _, f := range openapi3.PathItemMethods() {
		if f == m {
			return true
		}
	}
	return false
}

func sortedOpMethods(ops map[string]*openapi3.Operation) []string {
	out := make([]string, 0, len(ops))
	for m := range ops {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// resolveOperationRefs points the $refs of a decoded operation (its
// parameters, request body, responses and their schemas) at doc's
// components.
func resolveOperationRefs(doc *openapi3.T, op *openapi3.Operation) {
	if doc == nil || doc.Components == nil {
		return
	}
	c := doc.Components
	content := func(ct openapi3.Content) {
		for _, mt := range ct {
			if mt != nil {
				resolveSchemaRefs(doc, mt.Schema)
				resolveSchemaRefs(doc, mt.ItemSchema)
			}
		}
	}
	for _, p := range op.Parameters {
		if p == nil {
			continue
		}
		if p.Value == nil {
			if t := c.Parameters[refLeaf(p.Ref, "#/components/parameters/")]; t != nil {
				p.Value = t.Value
			}
			continue
		}
		resolveSchemaRefs(doc, p.Value.Schema)
		content(p.Value.Content)
	}
	if rb := op.RequestBody; rb != nil {
		if rb.Value == nil {
			if t := c.RequestBodies[refLeaf(rb.Ref, "#/components/requestBodies/")]; t != nil {
				rb.Value = t.Value
			}
		} else {
			content(rb.Value.Content)
		}
	}
	if op.Responses != nil {
		for _, r := range op.Responses.Map() {
			if r == nil {
				continue
			}
			if r.Value == nil {
				if t := c.Responses[refLeaf(r.Ref, "#/components/responses/")]; t != nil {
					r.Value = t.Value
				}
				continue
			}
			content(r.Value.Content)
		}
	}
}

// resolveSchemaRefs sets the Value of every component $ref in a
// decoded schema tree. Component schemas are already resolved and are
// not walked.
func resolveSchemaRefs(doc *openapi3.T, ref *openapi3.SchemaRef) {
	if ref == nil {
		return
	}
	if ref.Ref != "" {
		if ref.Value == nil && doc != nil && doc.Components != nil {
			if t := doc.Components.Schemas[refLeaf(ref.Ref, "#/components/schemas/")]; t != nil {
				ref.Value = t.Value
			}
		}
		return
	}
	s := ref.Value
	if s == nil {
		return
	}
	for _, list := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf, s.AllOf, s.PrefixItems} {
		for _, sub := range list {
			resolveSchemaRefs(doc, sub)
		}
	}
	for _, m := range []openapi3.Schemas{s.Properties, s.PatternProperties, s.DependentSchemas} {
		for _, sub := range m {
			resolveSchemaRefs(doc, sub)
		}
	}
	for _, sub := range []*openapi3.SchemaRef{s.Not, s.Items, s.Contains, s.PropertyNames,
		s.AdditionalProperties.Schema, s.If, s.Then, s.Else, s.ContentSchema} {
		resolveSchemaRefs(doc, sub)
	}
}

// refLeaf returns the component name of a local $ref under prefix, or
// "" for any other ref.
func refLeaf(ref, prefix string) string {
	name, ok := strings.CutPrefix(ref, prefix)
	if !ok || strings.Contains(name, "/") {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
}

// withExtItemSchema returns op with each response media type's
// x-item-schema extension (where a 3.0 or 3.1 document keeps a 3.2
// itemSchema) set as its ItemSchema, so OpenAPIStreamOf sees the
// stream. op itself is not modified: the result is a copy when there
// is anything to set.
func withExtItemSchema(doc *openapi3.T, op *openapi3.Operation) *openapi3.Operation {
	if op == nil || op.Responses == nil {
		return op
	}
	var out *openapi3.Operation
	for code, rr := range op.Responses.Map() {
		if rr == nil || rr.Value == nil {
			continue
		}
		var content openapi3.Content
		for name, mt := range rr.Value.Content {
			if mt == nil || mt.ItemSchema != nil {
				continue
			}
			v, ok := mt.Extensions[xItemSchema]
			if !ok {
				continue
			}
			raw, err := json.Marshal(v)
			if err != nil {
				continue
			}
			item := &openapi3.SchemaRef{}
			if err := json.Unmarshal(raw, item); err != nil {
				continue
			}
			resolveSchemaRefs(doc, item)
			if content == nil {
				content = make(openapi3.Content, len(rr.Value.Content))
				for k, v := range rr.Value.Content {
					content[k] = v
				}
			}
			cp := *mt
			cp.ItemSchema = item
			content[name] = &cp
		}
		if content == nil {
			continue
		}
		if out == nil {
			cp := *op
			cp.Responses = openapi3.NewResponses()
			cp.Responses.Extensions = op.Responses.Extensions
			for k, v := range op.Responses.Map() {
				cp.Responses.Set(k, v)
			}
			out = &cp
		}
		resp := *rr.Value
		resp.Content = content
		out.Responses.Set(code, &openapi3.ResponseRef{Value: &resp})
	}
	if out == nil {
		return op
	}
	return out
}

// xAdditionalOperations is the Path Item extension RenderOpenAPI uses
// to carry QUERY and custom-method operations in a 3.0 document,
// which has no field for them. Keyed by HTTP method.
const xAdditionalOperations = "x-additional-operations"

// additionalOpenAPIMethods returns item's additionalOperations keys,
// sorted. A key that collides with a fixed Path Item field is left
// out (Validate rejects those, but ir callers may skip Validate),
// matching kin-openapi's PathItem.Operations().
func additionalOpenAPIMethods(item *openapi3.PathItem) []string {
	if len(item.AdditionalOperations) == 0 {
		return nil
	}
	fixed := map[string]bool{}
	for _, m := range openapi3.PathItemMethods() {
		fixed[m] = true
	}
	out := make([]string, 0, len(item.AdditionalOperations))
	for m, op := range item.AdditionalOperations {
		if op == nil || fixed[m] {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// UnsupportedOpenAPIOperations lists the operations in doc that
// IngestOpenAPI does not turn into IR operations, one "METHOD /path"
// entry each, sorted. Today that is HEAD, OPTIONS, TRACE and
// CONNECT: they have no response body or are transport-level, so
// there is no GraphQL field to give them. Callers log these at
// registration so a dropped operation is visible.
func UnsupportedOpenAPIOperations(doc *openapi3.T) []string {
	if doc == nil || doc.Paths == nil {
		return nil
	}
	var out []string
	for path, item := range doc.Paths.Map() {
		if item == nil {
			continue
		}
		for _, v := range []struct {
			method string
			op     *openapi3.Operation
		}{
			{"HEAD", item.Head},
			{"OPTIONS", item.Options},
			{"TRACE", item.Trace},
			{"CONNECT", item.Connect},
		} {
			if v.op != nil {
				out = append(out, v.method+" "+path)
			}
		}
	}
	sort.Strings(out)
	return out
}

func ingestOpenAPIOp(svc *Service, method, path string, op *openapi3.Operation) *Operation {
	doc, _ := svc.Origin.(*openapi3.T)
	op = withExtItemSchema(doc, op)
	out := &Operation{
		Name:        op.OperationID,
		Description: op.Description,
		Ref:         extString(op.Extensions, xRefExtension),
		Annotations: extAnnotations(op.Extensions),
		HTTPMethod:  method,
		HTTPPath:    path,
		Tags:        op.Tags,
		OriginKind:  KindOpenAPI,
		Origin:      op,
	}
	if out.Name == "" {
		out.Name = method + path
	}
	// QUERY (OpenAPI 3.2) is safe and idempotent like GET but carries
	// a request body, so it is a Query field with a "body" arg. Custom
	// additionalOperations methods declare no semantics; treat them as
	// Mutations, the safe assumption for an unknown verb.
	switch strings.ToUpper(method) {
	case "GET", "HEAD", openapi3.MethodQuery:
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
		// GraphQL argument names must match /^[_A-Za-z][_0-9A-Za-z]*$/, the rule graphql.NewSchema
		// enforces. Transport headers/cookies commonly carry hyphenated names (X-Forwarded-For,
		// User-Agent) that aren't valid GraphQL identifiers; ingesting them as Args would panic the
		// schema build. They're transport metadata rather than GraphQL inputs, so omit them from the
		// GraphQL/gRPC surface — the REST (OpenAPI/huma) surface still serves them, and a handler that
		// needs such a value can read it from middleware/context. (path/query params, which virtually
		// always have identifier-safe names, are left untouched.)
		if (p.In == "header" || p.In == "cookie") && !isGraphQLName(p.Name) {
			continue
		}
		arg := &Arg{
			Name:            p.Name,
			Required:        p.Required,
			Description:     p.Description,
			OpenAPILocation: p.In,
		}
		if p.Schema != nil && p.Schema.Value != nil {
			// Keep the list-ness: an array query param (?ids=a&ids=b) is
			// a list argument, not a string.
			f := openapiPropToField(svc, opHint+pascalIdent(p.Name), p.Name, p.Schema, p.Required)
			arg.Type, arg.Repeated, arg.ItemRequired = f.Type, f.Repeated, f.ItemRequired
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

	// Response: prefer 200, then 201, then default. An array body is a
	// list output.
	if op.Responses != nil {
		if f := openapiResponseField(svc, opHint, op.Responses); f != nil {
			out.Output = &f.Type
			out.OutputRepeated, out.OutputItemRequired = f.Repeated, f.ItemRequired
		}
	}

	// Streamed success response (OpenAPI 3.2 itemSchema on a
	// sequential media type) → subscription; Output is one event.
	// OpenAPIStreamOf already returns nil when a unary JSON response
	// exists, so an op offering both stays unary.
	if st := OpenAPIStreamOf(op); st != nil {
		f := openapiPropToField(svc, opHint+"Event", "event", st.Item, true)
		out.Kind = OpSubscription
		out.Output = &f.Type
		out.OutputRepeated, out.OutputItemRequired = f.Repeated, f.ItemRequired
		out.OutputRequired = !f.Nullable
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

func openapiResponseField(svc *Service, opHint string, r *openapi3.Responses) *Field {
	for _, code := range []string{"200", "201"} {
		resp := r.Status(parseStatusCode(code))
		if resp != nil && resp.Value != nil {
			if mt, ok := resp.Value.Content["application/json"]; ok && mt.Schema != nil {
				return openapiPropToField(svc, opHint+"Response", "response", mt.Schema, false)
			}
		}
	}
	if def := r.Default(); def != nil && def.Value != nil {
		if mt, ok := def.Value.Content["application/json"]; ok && mt.Schema != nil {
			return openapiPropToField(svc, opHint+"Response", "response", mt.Schema, false)
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
