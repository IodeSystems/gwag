package gat

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// huma does not see request parameters declared on an ANONYMOUS EMBEDDED struct.
// Not in the OpenAPI document, and not in its runtime binder:
//
//	type scope struct {
//	    Dir string `query:"dir"`
//	}
//	type input struct {
//	    scope                        // <- dir is invisible
//	    Spec string `query:"spec"`
//	}
//
// The failure is silent and easy to misread. `?dir=…` is accepted and ignored,
// so a handler reads the zero value and any default it falls back to looks like
// correct behaviour. The OpenAPI document omits the parameter, so a generated
// client cannot send it. gat notices only because it builds its surfaces from
// that document — the parameter is simply absent from the GraphQL schema and the
// proto request message.
//
// The fix is to declare the fields directly on each input struct. Verbose, but
// it is the only spelling huma actually reads.
//
// gat cannot repair this — huma owns parameter discovery — so it reports it
// instead, at mount time, rather than letting a schema ship with holes in it.

// paramTags are the huma struct tags that declare a request parameter.
var paramTags = []string{"query", "path", "header", "cookie"}

// embeddedParam is one parameter lost to an embedded struct.
type embeddedParam struct {
	Operation string
	Input     string
	Embedded  string
	Tag       string
	Name      string
	Field     string
}

func (e embeddedParam) String() string {
	return fmt.Sprintf("%s: %s.%s.%s has `%s:%q`, which huma will not see",
		e.Operation, e.Input, e.Embedded, e.Field, e.Tag, e.Name)
}

// findEmbeddedParams walks an input type for parameters declared on anonymous
// embedded structs, recursively.
func findEmbeddedParams(opID string, t reflect.Type) []embeddedParam {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return walkEmbedded(opID, t.Name(), t, "", map[reflect.Type]bool{})
}

func walkEmbedded(opID, inputName string, t reflect.Type, path string, seen map[reflect.Type]bool) []embeddedParam {
	if seen[t] {
		return nil // a self-referential embed; already reported
	}
	seen[t] = true
	defer delete(seen, t)

	var out []embeddedParam
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.Anonymous {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		here := f.Name
		if path != "" {
			here = path + "." + f.Name
		}
		for j := range ft.NumField() {
			inner := ft.Field(j)
			for _, tag := range paramTags {
				v, ok := inner.Tag.Lookup(tag)
				if !ok {
					continue
				}
				out = append(out, embeddedParam{
					Operation: opID,
					Input:     inputName,
					Embedded:  here,
					Tag:       tag,
					Name:      strings.SplitN(v, ",", 2)[0],
					Field:     inner.Name,
				})
			}
		}
		// An embed inside an embed is just as invisible.
		out = append(out, walkEmbedded(opID, inputName, ft, here, seen)...)
	}
	return out
}

// embeddedParamError builds the mount-time error. It names every lost parameter
// and states the fix, because the symptom on its own reads like anything but a
// struct-embedding problem.
func embeddedParamError(found []embeddedParam) error {
	if len(found) == 0 {
		return nil
	}
	lines := make([]string, 0, len(found))
	for _, e := range found {
		lines = append(lines, "  "+e.String())
	}
	sort.Strings(lines)
	return fmt.Errorf(
		"gat: %d request parameter(s) declared on an anonymous embedded struct:\n%s\n\n"+
			"huma does not read parameters from embedded structs — they are absent from the\n"+
			"OpenAPI document and are not bound at runtime, so the request silently receives\n"+
			"the zero value. Declare these fields directly on each input struct instead.\n"+
			"Call g.AllowEmbeddedParams(true) before RegisterHuma to downgrade this to a warning.",
		len(found), strings.Join(lines, "\n"))
}

// embeddedParams collects lost parameters across every captured operation.
func (g *Gateway) embeddedParams() []embeddedParam {
	var out []embeddedParam
	for _, c := range g.captured {
		out = append(out, findEmbeddedParams(c.op.OperationID, c.inputType)...)
	}
	return out
}

// AllowEmbeddedParams downgrades the embedded-parameter check from a mount-time
// error to a log warning. Call before RegisterHuma.
//
// The check exists because huma does not read parameters declared on an
// anonymous embedded struct: they are absent from the OpenAPI document and are
// not bound at runtime, so the handler silently receives zero values. That is a
// bug in every case gat has seen, which is why the default is to fail.
//
// Reach for this only to unblock an existing codebase while the inputs are being
// flattened — not as a permanent setting.
//
// Stability: experimental
func (g *Gateway) AllowEmbeddedParams(b bool) *Gateway {
	g.allowEmbeddedParams = b
	return g
}
