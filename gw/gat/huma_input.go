package gat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// inputCheck runs the request checks huma.Register runs between the
// middlewares and the handler: parameter schema validation, body schema
// validation, then input Resolvers. Over REST huma does this itself.
// GraphQL and gRPC bind args straight into *I, so without inputCheck a
// constraint (maxLength, pattern, minimum) or a Resolver that rejects
// held over REST only.
//
// huma validates the raw request; gat has the bound struct. Values are
// taken from the struct (JSON round-trip, so Go types validate as JSON
// does), and presence from the transport args: an argument the client
// did not send is not validated unless it is required, as over REST.
type inputCheck struct {
	registry  huma.Registry
	params    []paramCheck
	body      *bodyCheck
	resolvers [][]int // field paths, huma's findResolvers order
	bodyArg   string  // IR arg carrying the body; set at ingest
}

type paramCheck struct {
	name, loc string
	required  bool
	schema    *huma.Schema
	path      []int
}

type bodyCheck struct {
	path     []int
	schema   *huma.Schema
	required bool
}

// newInputCheck plans the checks for op as huma documented it on api.
// Call after huma registered op. Returns nil when there is nothing to
// check.
func newInputCheck(api huma.API, op huma.Operation, inputType reflect.Type) *inputCheck {
	c := &inputCheck{registry: api.OpenAPI().Components.Schemas}
	tagLookup := map[string][]int{}
	var bodyIdx []int
	collect(inputType, nil, tagLookup, &bodyIdx)
	if doc := documentedOp(api.OpenAPI(), op.OperationID); doc != nil {
		if !doc.SkipValidateParams {
			for _, p := range doc.Parameters {
				path, ok := tagLookup[p.In+":"+p.Name]
				if !ok || !c.constrained(p.Schema, map[*huma.Schema]bool{}) {
					continue
				}
				c.params = append(c.params, paramCheck{name: p.Name, loc: p.In, required: p.Required, schema: p.Schema, path: path})
			}
		}
		if !doc.SkipValidateBody && doc.RequestBody != nil && bodyIdx != nil {
			if mt := doc.RequestBody.Content["application/json"]; mt != nil && c.constrained(mt.Schema, map[*huma.Schema]bool{}) {
				c.body = &bodyCheck{path: bodyIdx, schema: mt.Schema, required: doc.RequestBody.Required}
			}
		}
	}
	findResolverPaths(inputType, nil, &c.resolvers, map[reflect.Type]bool{})
	if len(c.params) == 0 && c.body == nil && len(c.resolvers) == 0 {
		return nil
	}
	return c
}

// validatedFormats are the formats huma.Validate checks. Others (int32,
// int64, double) only document the type.
var validatedFormats = map[string]bool{
	"date-time": true, "date-time-http": true, "date": true, "duration": true, "time": true,
	"email": true, "idn-email": true, "hostname": true, "idn-hostname": true,
	"ipv4": true, "ipv6": true, "ip": true, "uri": true, "iri": true,
	"uri-reference": true, "iri-reference": true, "uri-template": true, "uuid": true,
	"json-pointer": true, "relative-json-pointer": true, "regex": true,
}

// constrained reports whether s, or a schema it reaches, has a keyword
// that can reject a value of the bound Go type. Binding into *I already
// enforces the types, so a schema without one cannot fail validation
// and is not checked. Most schemas have none; this keeps the cost of
// the checks to the ops that declare constraints.
func (c *inputCheck) constrained(s *huma.Schema, seen map[*huma.Schema]bool) bool {
	if s == nil {
		return false
	}
	if s.Ref != "" {
		s = c.registry.SchemaFromRef(s.Ref)
		if s == nil {
			return false
		}
	}
	if seen[s] {
		return false
	}
	seen[s] = true
	if validatedFormats[s.Format] || s.ContentEncoding != "" || len(s.Enum) > 0 || s.Const != nil ||
		s.Minimum != nil || s.ExclusiveMinimum != nil || s.Maximum != nil || s.ExclusiveMaximum != nil ||
		s.MultipleOf != nil || s.MinLength != nil || s.MaxLength != nil || s.Pattern != "" ||
		s.MinItems != nil || s.MaxItems != nil || s.UniqueItems ||
		s.MinProperties != nil || s.MaxProperties != nil || len(s.DependentRequired) > 0 ||
		len(s.OneOf) > 0 || len(s.AnyOf) > 0 || len(s.AllOf) > 0 || s.Not != nil {
		return true
	}
	if c.constrained(s.Items, seen) {
		return true
	}
	if addl, ok := s.AdditionalProperties.(*huma.Schema); ok && c.constrained(addl, seen) {
		return true
	}
	for name, p := range s.Properties {
		// huma's schema-link hook adds $schema (format uri) to every
		// body schema; no Go input field produces it.
		if name == "$schema" {
			continue
		}
		if c.constrained(p, seen) {
			return true
		}
	}
	return false
}

// documentedOp finds op in doc by OperationID. Looked up by ID, not
// path: a huma.Group registers under a prefixed path.
func documentedOp(doc *huma.OpenAPI, id string) *huma.Operation {
	for _, item := range doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op != nil && op.OperationID == id {
				return op
			}
		}
	}
	return nil
}

// apply runs the checks on in behind the middlewares' ctx. A nil
// check passes.
func (c *inputCheck) apply(ctx context.Context, op *huma.Operation, in any, args map[string]any) error {
	if c == nil {
		return nil
	}
	return c.run(newInprocHumaContext(ctx, op), in, args)
}

// run checks in, the bound *I. args are the transport args it was bound
// from (GraphQL arguments, or the gRPC request as args); hctx is the
// huma.Context after the middlewares, which Resolvers receive.
func (c *inputCheck) run(hctx huma.Context, in any, args map[string]any) error {
	v := reflect.ValueOf(in).Elem()
	res := &huma.ValidateResult{}
	pb := huma.NewPathBuffer(make([]byte, 0, 64), 0)

	for _, p := range c.params {
		if raw, ok := args[p.name]; (!ok || raw == nil) && !p.required {
			continue
		}
		pb.Reset()
		pb.Push(p.loc)
		pb.Push(p.name)
		f, err := fieldByPath(v, p.path)
		if err != nil {
			res.Add(pb, nil, err.Error())
			continue
		}
		val, err := jsonValue(f.Interface())
		if err != nil {
			res.Add(pb, nil, err.Error())
			continue
		}
		huma.Validate(c.registry, p.schema, pb, huma.ModeWriteToServer, val, res)
	}

	if c.body != nil {
		raw := args[c.bodyArg]
		if c.bodyArg == "" {
			raw = nil
		}
		if raw != nil || c.body.required {
			pb.Reset()
			pb.Push("body")
			f, err := fieldByPath(v, c.body.path)
			if err == nil {
				var val any
				if val, err = jsonValue(f.Interface()); err == nil {
					val = c.prune(val, raw, c.body.schema)
					huma.Validate(c.registry, c.body.schema, pb, huma.ModeWriteToServer, val, res)
				}
			}
			if err != nil {
				res.Add(pb, nil, err.Error())
			}
		}
	}

	// huma runs the Resolvers even after validation errors and reports
	// all of them together.
	for _, path := range c.resolvers {
		pb.Reset()
		eachResolver(v, path, pb, func(item reflect.Value) {
			switch r := item.Interface().(type) {
			case huma.Resolver:
				res.Errors = append(res.Errors, r.Resolve(hctx)...)
			case huma.ResolverWithPath:
				res.Errors = append(res.Errors, r.Resolve(hctx, pb)...)
			}
		})
	}

	if len(res.Errors) == 0 {
		return nil
	}
	// As huma: the last error that carries a status sets it.
	status := http.StatusUnprocessableEntity
	for i := len(res.Errors) - 1; i >= 0; i-- {
		var se huma.StatusError
		if errors.As(res.Errors[i], &se) {
			status = se.GetStatus()
			break
		}
	}
	msgs := make([]string, len(res.Errors))
	for i, err := range res.Errors {
		msgs[i] = err.Error()
	}
	return &requestRejection{status: status, msg: "validation failed: " + strings.Join(msgs, "; ")}
}

// jsonValue is v as encoding/json would decode it from a request body.
func jsonValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// prune drops the properties of val that the client did not send
// (absent or null in raw) and that s does not require, so a zero value
// the struct filled in is not validated as if it were sent. Required
// properties stay: GraphQL always sends them, and an absent proto3
// field is its zero value.
func (c *inputCheck) prune(val, raw any, s *huma.Schema) any {
	if s != nil && s.Ref != "" {
		s = c.registry.SchemaFromRef(s.Ref)
	}
	if s == nil {
		return val
	}
	switch v := val.(type) {
	case map[string]any:
		r, _ := raw.(map[string]any)
		if len(s.Properties) == 0 {
			return val
		}
		for k, child := range v {
			if rv, ok := r[k]; ok && rv != nil {
				v[k] = c.prune(child, rv, s.Properties[k])
			} else if !slices.Contains(s.Required, k) {
				delete(v, k)
			}
		}
	case []any:
		if r, ok := raw.([]any); ok && len(r) == len(v) {
			for i := range v {
				v[i] = c.prune(v[i], r[i], s.Items)
			}
		}
	}
	return val
}

// collectionElem marks a path step into the elements of a slice, array,
// or map, as in huma.
const collectionElem = -1

// findResolverPaths ports huma's findResolvers: the field path to every
// type whose pointer implements huma.Resolver or huma.ResolverWithPath,
// parents before children.
func findResolverPaths(t reflect.Type, path []int, out *[][]int, visited map[reflect.Type]bool) {
	t = derefType(t)
	matched := false
	if pt := reflect.PointerTo(t); pt.Implements(resolverType) || pt.Implements(resolverWithPathType) {
		*out = append(*out, slices.Clone(path))
		matched = true
	}
	switch t.Kind() {
	case reflect.Struct:
		if visited[t] {
			return
		}
		visited[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || (matched && f.Anonymous) {
				continue
			}
			findResolverPaths(f.Type, append(slices.Clone(path), i), out, visited)
		}
		delete(visited, t)
	case reflect.Slice, reflect.Array, reflect.Map:
		findResolverPaths(t.Elem(), append(slices.Clone(path), collectionElem), out, visited)
	}
}

var (
	resolverType         = reflect.TypeFor[huma.Resolver]()
	resolverWithPathType = reflect.TypeFor[huma.ResolverWithPath]()
)

// eachResolver ports huma's findResult.everyPB: it walks path through
// v, naming the location in pb as huma does, and calls f with a pointer
// to each value found.
func eachResolver(v reflect.Value, path []int, pb *huma.PathBuffer, f func(reflect.Value)) {
	v = reflect.Indirect(v)
	if !v.IsValid() {
		return // an omitted optional value
	}
	if len(path) == 0 {
		if v.CanAddr() {
			f(v.Addr())
		} else {
			p := reflect.New(v.Type())
			p.Elem().Set(v)
			f(p)
		}
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		field := v.Type().Field(path[0])
		pops := 0
		if !field.Anonymous {
			pops++
			switch {
			case field.Tag.Get("path") != "" && pb.Len() == 0:
				pb.Push("path")
				pb.Push(field.Tag.Get("path"))
				pops++
			case field.Tag.Get("query") != "" && pb.Len() == 0:
				pb.Push("query")
				pb.Push(strings.SplitN(field.Tag.Get("query"), ",", 2)[0])
				pops++
			case field.Tag.Get("header") != "" && pb.Len() == 0:
				pb.Push("header")
				pb.Push(field.Tag.Get("header"))
				pops++
			default:
				pb.Push(jsonFieldName(field))
			}
		}
		eachResolver(v.Field(path[0]), path[1:], pb, f)
		for ; pops > 0; pops-- {
			pb.Pop()
		}
	case reflect.Slice, reflect.Array:
		for j := 0; j < v.Len(); j++ {
			pb.PushIndex(j)
			eachResolver(v.Index(j), path[1:], pb, f)
			pb.Pop()
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			pb.Push(fmt.Sprint(k.Interface()))
			// Map values are not addressable: resolve a copy, write it back.
			item := reflect.New(v.Type().Elem()).Elem()
			item.Set(v.MapIndex(k))
			eachResolver(item, path[1:], pb, f)
			v.SetMapIndex(k, item)
			pb.Pop()
		}
	}
}

// jsonFieldName is huma's jsonName: the json tag name, else the
// lower-cased field name.
func jsonFieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" {
		return strings.Split(tag, ",")[0]
	}
	return strings.ToLower(f.Name)
}
