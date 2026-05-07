package ir

import (
	"fmt"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
)

// RenderOpenAPI projects svc onto an OpenAPI 3.0 document.
// Same-kind shortcut (KindOpenAPI Origin) emits the source spec
// verbatim. Cross-kind synthesizes from the canonical fields:
//
//   - Operation.HTTPMethod / .HTTPPath drive the path + verb when
//     present; cross-kind ingest from proto sets HTTPMethod="POST"
//     and HTTPPath="/<package>.<Service>/<Method>" by default.
//   - Args land as parameters (by OpenAPILocation when set; else
//     query for primitives, "body" for object refs).
//   - Output renders as the 200 response schema.
//   - Service.Types map onto components/schemas. Object Types
//     become object schemas; Enum Types become string-enum schemas.
//   - oneOf / interface / map render lossy when not natively
//     representable.
func RenderOpenAPI(svc *Service) (*openapi3.T, error) {
	if svc.OriginKind == KindOpenAPI {
		if doc, ok := svc.Origin.(*openapi3.T); ok && doc != nil {
			return doc, nil
		}
	}
	title := svc.Namespace
	if svc.Version != "" {
		title = svc.Namespace + " " + svc.Version
	}
	doc := &openapi3.T{
		OpenAPI: "3.0.0",
		Info: &openapi3.Info{
			Title:       title,
			Version:     svc.Version,
			Description: svc.Description,
		},
		Paths: &openapi3.Paths{},
		Components: &openapi3.Components{
			Schemas: openapi3.Schemas{},
		},
	}

	// Components first so $ref resolution lines up.
	for _, name := range stableTypeOrder(svc) {
		t := svc.Types[name.Name]
		if t == nil {
			continue
		}
		switch t.TypeKind {
		case TypeObject, TypeInput:
			doc.Components.Schemas[t.Name] = renderOpenAPIObject(t)
		case TypeEnum:
			doc.Components.Schemas[t.Name] = renderOpenAPIEnum(t)
		}
	}

	// Operations.
	for _, op := range svc.Operations {
		path := op.HTTPPath
		method := op.HTTPMethod
		if path == "" {
			// Synthetic path for proto-origin / unknown: treat the
			// canonical Service+op like a gRPC-over-HTTP route.
			ns := svc.ServiceName
			if ns == "" {
				ns = svc.Namespace
			}
			path = fmt.Sprintf("/%s/%s", ns, op.Name)
		}
		if method == "" {
			if op.Kind == OpQuery {
				method = "GET"
			} else {
				method = "POST"
			}
		}
		oop := renderOpenAPIOp(svc, op, method)
		item := doc.Paths.Value(path)
		if item == nil {
			item = &openapi3.PathItem{}
			doc.Paths.Set(path, item)
		}
		switch method {
		case "GET":
			item.Get = oop
		case "POST":
			item.Post = oop
		case "PUT":
			item.Put = oop
		case "PATCH":
			item.Patch = oop
		case "DELETE":
			item.Delete = oop
		}
	}
	return doc, nil
}

func renderOpenAPIObject(t *Type) *openapi3.SchemaRef {
	if t.OriginKind == KindOpenAPI {
		if r, ok := t.Origin.(*openapi3.SchemaRef); ok && r != nil {
			return r
		}
	}
	props := openapi3.Schemas{}
	required := []string{}
	for _, f := range t.Fields {
		props[f.Name] = renderOpenAPIFieldSchema(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	return &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type:        &openapi3.Types{"object"},
			Description: t.Description,
			Properties:  props,
			Required:    required,
		},
	}
}

func renderOpenAPIEnum(t *Type) *openapi3.SchemaRef {
	if t.OriginKind == KindOpenAPI {
		if r, ok := t.Origin.(*openapi3.SchemaRef); ok && r != nil {
			return r
		}
	}
	enumVals := make([]any, len(t.Enum))
	for i, ev := range t.Enum {
		enumVals[i] = ev.Name
	}
	return &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Type:        &openapi3.Types{"string"},
			Description: t.Description,
			Enum:        enumVals,
		},
	}
}

func renderOpenAPIFieldSchema(f *Field) *openapi3.SchemaRef {
	if f.Repeated {
		inner := renderOpenAPIRefForType(f.Type)
		return &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type:  &openapi3.Types{"array"},
				Items: inner,
			},
		}
	}
	if f.Type.IsMap() {
		val := renderOpenAPIRefForType(f.Type.Map.ValueType)
		return &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type: &openapi3.Types{"object"},
				AdditionalProperties: openapi3.AdditionalProperties{
					Schema: val,
				},
			},
		}
	}
	return renderOpenAPIRefForType(f.Type)
}

func renderOpenAPIRefForType(r TypeRef) *openapi3.SchemaRef {
	if r.IsNamed() {
		return &openapi3.SchemaRef{Ref: "#/components/schemas/" + r.Named}
	}
	if r.IsMap() {
		return &openapi3.SchemaRef{
			Value: &openapi3.Schema{
				Type: &openapi3.Types{"object"},
				AdditionalProperties: openapi3.AdditionalProperties{
					Schema: renderOpenAPIRefForType(r.Map.ValueType),
				},
			},
		}
	}
	return primitiveOpenAPI(r.Builtin)
}

func primitiveOpenAPI(s ScalarKind) *openapi3.SchemaRef {
	switch s {
	case ScalarBool:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"boolean"}}}
	case ScalarBytes:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}, Format: "byte"}}
	case ScalarInt32, ScalarUInt32:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"integer"}, Format: "int32"}}
	case ScalarInt64, ScalarUInt64:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"integer"}, Format: "int64"}}
	case ScalarFloat:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"number"}, Format: "float"}}
	case ScalarDouble:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"number"}, Format: "double"}}
	case ScalarTimestamp:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}, Format: "date-time"}}
	case ScalarID, ScalarString:
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{"string"}}}
}

func renderOpenAPIOp(svc *Service, op *Operation, method string) *openapi3.Operation {
	if op.OriginKind == KindOpenAPI {
		if oop, ok := op.Origin.(*openapi3.Operation); ok && oop != nil {
			return oop
		}
	}
	out := &openapi3.Operation{
		OperationID: op.Name,
		Description: op.Description,
		Tags:        op.Tags,
		Responses:   openapi3.NewResponses(),
	}
	for _, a := range op.Args {
		switch a.OpenAPILocation {
		case "body", "":
			// Default to body when location unset and the type is
			// non-primitive; query for primitives.
			if a.Type.IsNamed() || a.OpenAPILocation == "body" {
				out.RequestBody = &openapi3.RequestBodyRef{
					Value: &openapi3.RequestBody{
						Required:    a.Required,
						Description: a.Description,
						Content: openapi3.Content{
							"application/json": &openapi3.MediaType{
								Schema: renderOpenAPIRefForType(a.Type),
							},
						},
					},
				}
				continue
			}
			out.Parameters = append(out.Parameters, &openapi3.ParameterRef{
				Value: &openapi3.Parameter{
					Name:        a.Name,
					In:          "query",
					Required:    a.Required,
					Description: a.Description,
					Schema:      renderOpenAPIRefForType(a.Type),
				},
			})
		default:
			out.Parameters = append(out.Parameters, &openapi3.ParameterRef{
				Value: &openapi3.Parameter{
					Name:        a.Name,
					In:          a.OpenAPILocation,
					Required:    a.Required,
					Description: a.Description,
					Schema:      renderOpenAPIRefForType(a.Type),
				},
			})
		}
	}
	if op.Output != nil {
		desc := "ok"
		out.Responses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{
				Description: &desc,
				Content: openapi3.Content{
					"application/json": &openapi3.MediaType{
						Schema: renderOpenAPIRefForType(*op.Output),
					},
				},
			},
		})
	} else {
		desc := "ok"
		out.Responses.Set("200", &openapi3.ResponseRef{
			Value: &openapi3.Response{Description: &desc},
		})
	}
	_ = sort.StringSlice(nil) // keep import usage stable; sort referenced via stableTypeOrder elsewhere
	return out
}
