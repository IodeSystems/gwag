package ir

import (
	"encoding/json"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPI31Version is the "openapi" field RenderOpenAPIWith writes
// when RenderOpenAPIOptions.OpenAPI31 is set. 3.1.0, not 3.1.1: it is
// what huma emits, and some tools match the version string exactly.
// The two patch releases describe the same schema dialect.
const OpenAPI31Version = "3.1.0"

// RenderOpenAPIOptions selects the output of RenderOpenAPIWith.
//
// Stability: experimental
type RenderOpenAPIOptions struct {
	// OpenAPI31 emits an OpenAPI 3.1 document (OpenAPI31Version)
	// instead of 3.0.0. Schemas use JSON Schema 2020-12 forms:
	// `nullable: true` becomes a "null" entry in the type list (or a
	// {"type": "null"} branch when the schema has no type), boolean
	// exclusiveMinimum / exclusiveMaximum become numeric bounds, and a
	// schema `example` becomes `examples: [...]`. An OpenAPI-origin
	// spec is converted the same way. Off, the output is RenderOpenAPI's:
	// an OpenAPI-origin spec that is already 3.1 stays 3.1. v3 makes
	// this the default.
	OpenAPI31 bool
}

// ParseOpenAPIVersion reads the `openapi` query parameter of the schema
// routes: "3.0" or "3.1" (a full "3.0.0" / "3.1.0" also works). Empty
// gives the server default, def31.
//
// Stability: experimental
func ParseOpenAPIVersion(v string, def31 bool) (RenderOpenAPIOptions, error) {
	switch v {
	case "":
		return RenderOpenAPIOptions{OpenAPI31: def31}, nil
	case "3.0", "3.0.0":
		return RenderOpenAPIOptions{}, nil
	case "3.1", OpenAPI31Version:
		return RenderOpenAPIOptions{OpenAPI31: true}, nil
	}
	return RenderOpenAPIOptions{}, fmt.Errorf("openapi=%q: want 3.0 or 3.1", v)
}

// RenderOpenAPIWith is RenderOpenAPI with options. The zero value
// gives the same document as RenderOpenAPI. With OpenAPI31 the result
// is a converted copy; the service's Origin spec is not modified.
//
// Stability: experimental
func RenderOpenAPIWith(svc *Service, opts RenderOpenAPIOptions) (*openapi3.T, error) {
	doc, err := RenderOpenAPI(svc)
	if err != nil || !opts.OpenAPI31 {
		return doc, err
	}
	return toOpenAPI31(doc)
}

// toOpenAPI31 returns a 3.1 copy of doc. The copy goes through JSON so
// schemas shared with svc.Origin (the same-kind shortcut returns them
// as-is) are never written to; $ref targets come back as plain refs.
func toOpenAPI31(doc *openapi3.T) (*openapi3.T, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("openapi 3.1: marshal: %w", err)
	}
	out := &openapi3.T{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("openapi 3.1: copy: %w", err)
	}
	out.OpenAPI = OpenAPI31Version
	walkDocSchemas(out, schemaTo31)
	return out, nil
}

// walkDocSchemas calls fn on every inline schema reachable from doc:
// components, and the parameters, headers, request bodies and
// responses of every path, callback and component. fn is applied
// parent-first and recurses into sub-schemas itself.
func walkDocSchemas(doc *openapi3.T, fn func(*openapi3.SchemaRef)) {
	content := func(c openapi3.Content) {
		for _, mt := range c {
			if mt != nil {
				fn(mt.Schema)
			}
		}
	}
	param := func(p *openapi3.ParameterRef) {
		if p != nil && p.Value != nil {
			fn(p.Value.Schema)
			content(p.Value.Content)
		}
	}
	header := func(h *openapi3.HeaderRef) {
		if h != nil && h.Value != nil {
			fn(h.Value.Schema)
			content(h.Value.Content)
		}
	}
	body := func(b *openapi3.RequestBodyRef) {
		if b != nil && b.Value != nil {
			content(b.Value.Content)
		}
	}
	response := func(r *openapi3.ResponseRef) {
		if r != nil && r.Value != nil {
			content(r.Value.Content)
			for _, h := range r.Value.Headers {
				header(h)
			}
		}
	}
	var pathItem func(*openapi3.PathItem)
	pathItem = func(item *openapi3.PathItem) {
		if item == nil {
			return
		}
		for _, p := range item.Parameters {
			param(p)
		}
		for _, op := range item.Operations() {
			for _, p := range op.Parameters {
				param(p)
			}
			body(op.RequestBody)
			if op.Responses != nil {
				for _, r := range op.Responses.Map() {
					response(r)
				}
			}
			for _, cb := range op.Callbacks {
				if cb != nil && cb.Value != nil {
					for _, it := range cb.Value.Map() {
						pathItem(it)
					}
				}
			}
		}
	}
	if doc.Paths != nil {
		for _, item := range doc.Paths.Map() {
			pathItem(item)
		}
	}
	for _, item := range doc.Webhooks {
		pathItem(item)
	}
	if c := doc.Components; c != nil {
		for _, s := range c.Schemas {
			fn(s)
		}
		for _, p := range c.Parameters {
			param(p)
		}
		for _, h := range c.Headers {
			header(h)
		}
		for _, b := range c.RequestBodies {
			body(b)
		}
		for _, r := range c.Responses {
			response(r)
		}
		for _, cb := range c.Callbacks {
			if cb != nil && cb.Value != nil {
				for _, it := range cb.Value.Map() {
					pathItem(it)
				}
			}
		}
	}
}

// schemaTo31 rewrites one schema and its sub-schemas from 3.0 forms
// to 3.1 forms. A $ref is left alone: the copy has no Value behind it,
// and its target is reached through components.
func schemaTo31(ref *openapi3.SchemaRef) {
	if ref == nil || ref.Ref != "" || ref.Value == nil {
		return
	}
	s := ref.Value

	if s.Nullable {
		s.Nullable = false
		switch {
		case s.Type != nil && len(*s.Type) > 0:
			if !s.Type.Includes(openapi3.TypeNull) {
				t := append(openapi3.Types{}, *s.Type...)
				t = append(t, openapi3.TypeNull)
				s.Type = &t
			}
			if len(s.Enum) > 0 && !enumHasNull(s.Enum) {
				s.Enum = append(s.Enum, nil)
			}
		case len(s.OneOf) > 0:
			s.OneOf = append(s.OneOf, nullSchema())
		case len(s.AnyOf) > 0:
			s.AnyOf = append(s.AnyOf, nullSchema())
		case len(s.AllOf) == 1:
			// `allOf: [$ref], nullable: true` is the 3.0 way to make a
			// reference nullable.
			s.OneOf = openapi3.SchemaRefs{s.AllOf[0], nullSchema()}
			s.AllOf = nil
		case len(s.AllOf) > 1:
			all := &openapi3.SchemaRef{Value: &openapi3.Schema{AllOf: s.AllOf}}
			s.OneOf = openapi3.SchemaRefs{all, nullSchema()}
			s.AllOf = nil
		}
		// No type and no composition: the schema already accepts null.
	}

	s.ExclusiveMin, s.Min = exclusiveTo31(s.ExclusiveMin, s.Min)
	s.ExclusiveMax, s.Max = exclusiveTo31(s.ExclusiveMax, s.Max)

	if s.Example != nil {
		if len(s.Examples) == 0 {
			s.Examples = []any{s.Example}
		}
		s.Example = nil
	}

	for _, list := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf, s.AllOf, s.PrefixItems} {
		for _, sub := range list {
			schemaTo31(sub)
		}
	}
	for _, m := range []openapi3.Schemas{s.Properties, s.PatternProperties, s.DependentSchemas} {
		for _, sub := range m {
			schemaTo31(sub)
		}
	}
	schemaTo31(s.Not)
	schemaTo31(s.Items)
	schemaTo31(s.Contains)
	schemaTo31(s.PropertyNames)
	schemaTo31(s.AdditionalProperties.Schema)
	schemaTo31(s.UnevaluatedItems.Schema)
	schemaTo31(s.UnevaluatedProperties.Schema)
}

// exclusiveTo31 turns a 3.0 boolean bound into a 3.1 numeric one:
// `minimum: 5, exclusiveMinimum: true` → `exclusiveMinimum: 5`. A false
// flag is dropped. A numeric bound is already 3.1.
func exclusiveTo31(b openapi3.ExclusiveBound, limit *float64) (openapi3.ExclusiveBound, *float64) {
	if b.Bool == nil {
		return b, limit
	}
	if !*b.Bool || limit == nil {
		return openapi3.ExclusiveBound{}, limit
	}
	v := *limit
	return openapi3.ExclusiveBound{Value: &v}, nil
}

func nullSchema() *openapi3.SchemaRef {
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeNull}}}
}

func enumHasNull(vals []any) bool {
	for _, v := range vals {
		if v == nil {
			return true
		}
	}
	return false
}
