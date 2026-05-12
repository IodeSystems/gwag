package gat

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/iodesystems/gwag/gw/ir"
)

// inprocDispatcher invokes a captured huma handler directly. GraphQL
// args (map[string]any keyed by ir.Arg.Name) are bound into a fresh
// instance of the handler's typed input struct via reflection; the
// handler's typed output is unwrapped (huma convention: the JSON body
// lives in a field named `Body`).
type inprocDispatcher struct {
	captured *capturedOp
	op       *ir.Operation
	bodyArg  string // name of the body Arg, if any; empty when none
}

func newInprocDispatcher(c *capturedOp, op *ir.Operation) ir.Dispatcher {
	var bodyArg string
	for _, a := range op.Args {
		if strings.EqualFold(a.OpenAPILocation, "body") {
			bodyArg = a.Name
			break
		}
	}
	return &inprocDispatcher{captured: c, op: op, bodyArg: bodyArg}
}

func (d *inprocDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	inPtr := reflect.New(d.captured.inputType)
	in := inPtr.Elem()

	if err := bindInput(in, d.op, args, d.bodyArg); err != nil {
		return nil, fmt.Errorf("gat: bind input for %s: %w", d.op.Name, err)
	}

	out, err := d.captured.invoke(ctx, inPtr.Interface())
	if err != nil {
		return nil, err
	}
	return extractBody(out), nil
}

// bindInput maps GraphQL args into the typed huma input struct. Huma
// uses field tags for non-body locations (`path:"id"`, `query:"limit"`,
// `header:"X-Foo"`, `cookie:"sid"`); the request body is a field
// literally named `Body` whose type is the body schema.
func bindInput(in reflect.Value, op *ir.Operation, args map[string]any, bodyArg string) error {
	t := in.Type()

	tagLookup := map[string]int{} // location:name → field index
	bodyIdx := -1
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Name == "Body" {
			bodyIdx = i
		}
		for _, loc := range []string{"path", "query", "header", "cookie"} {
			if v, ok := f.Tag.Lookup(loc); ok {
				// huma allows `path:"id,required"` etc. — keep only
				// the name before the first comma.
				name := strings.SplitN(v, ",", 2)[0]
				tagLookup[loc+":"+name] = i
			}
		}
	}

	for _, a := range op.Args {
		if a.Name == bodyArg {
			continue
		}
		v, present := args[a.Name]
		if !present {
			continue
		}
		idx, ok := tagLookup[strings.ToLower(a.OpenAPILocation)+":"+a.Name]
		if !ok {
			// Some huma inputs omit explicit tags for path params
			// when the field name matches the param name. Fall back
			// to case-insensitive field-name match.
			for i := 0; i < t.NumField(); i++ {
				if strings.EqualFold(t.Field(i).Name, a.Name) {
					idx = i
					ok = true
					break
				}
			}
		}
		if !ok {
			continue // best-effort; ignore unmappable args
		}
		if err := assignValue(in.Field(idx), v); err != nil {
			return fmt.Errorf("arg %q: %w", a.Name, err)
		}
	}

	if bodyArg != "" && bodyIdx >= 0 {
		if v, present := args[bodyArg]; present && v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("marshal body: %w", err)
			}
			if err := json.Unmarshal(b, in.Field(bodyIdx).Addr().Interface()); err != nil {
				return fmt.Errorf("unmarshal body: %w", err)
			}
		}
	}

	return nil
}

// assignValue converts v to dst's type via JSON round-trip when the
// types don't match directly. Cheap and correct for the JSON-shaped
// data GraphQL hands us.
func assignValue(dst reflect.Value, v any) error {
	if !dst.CanSet() {
		return fmt.Errorf("field not settable")
	}
	src := reflect.ValueOf(v)
	if src.IsValid() && src.Type().AssignableTo(dst.Type()) {
		dst.Set(src)
		return nil
	}
	if src.IsValid() && src.Type().ConvertibleTo(dst.Type()) {
		dst.Set(src.Convert(dst.Type()))
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst.Addr().Interface())
}

// extractBody pulls the JSON payload off a huma output. Huma outputs
// commonly look like `type Out struct { Body T }`; the Body field is
// the JSON response. For outputs without a Body field, return the
// dereferenced struct as-is.
func extractBody(out any) any {
	if out == nil {
		return nil
	}
	v := reflect.ValueOf(out)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return out
	}
	if body := v.FieldByName("Body"); body.IsValid() {
		return body.Interface()
	}
	return v.Interface()
}
