package gat

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// huma discovers request parameters by walking the input struct, and it skips
// every field that is not exported — `_findInType` bails on `!f.IsExported()`
// before it ever reads the tag. Embedded structs are otherwise walked normally
// ("Always process embedded structs"), so the trap is not embedding. It is
// embedding an UNEXPORTED type:
//
//	type scope struct { Dir string `query:"dir"` }   // lowercase type name ⇒
//	type input struct {                             // unexported FIELD name ⇒
//	    scope                                       // invisible to huma
//	    Spec string `query:"spec"`
//	}
//
//	type Scope struct { Dir string `query:"dir"` }   // exported ⇒ works
//	type input struct {
//	    Scope
//	    Spec string `query:"spec"`
//	}
//
// An embedded field takes its name from its type, so a lowercase type name makes
// the field unexported and the whole struct disappears. The same rule silently
// drops an ordinary unexported field carrying a parameter tag.
//
// The failure is quiet in the worst way: `?dir=…` is accepted and ignored, the
// handler reads the zero value, and any default it falls back to looks like
// correct behaviour. The OpenAPI document omits the parameter, so a generated
// client cannot send it either. gat notices only because it builds GraphQL and
// proto from that document.
//
// The fix is one character: export the type. gat cannot apply it — huma owns
// parameter discovery, and Go has no way to promote a field's visibility — so it
// reports instead, at mount time.

// paramTags are the huma struct tags that declare a request parameter.
var paramTags = []string{"query", "path", "header", "cookie"}

// hiddenParam is one parameter huma will not see because the field carrying it,
// or the embedded struct holding it, is unexported.
type hiddenParam struct {
	Operation string
	Input     string
	Path      string // embed path, empty for a direct field
	Tag       string
	Name      string
	Field     string
	Reason    string // what is unexported
}

func (e hiddenParam) String() string {
	where := e.Input
	if e.Path != "" {
		where += "." + e.Path
	}
	return fmt.Sprintf("%s: %s.%s has `%s:%q` — %s, so huma will not see it",
		e.Operation, where, e.Field, e.Tag, e.Name, e.Reason)
}

// findHiddenParams walks an input type for parameter tags huma cannot reach:
// on an unexported field, or inside an unexported embedded struct. Exported
// embeds are walked through, because huma walks them too.
func findHiddenParams(opID string, t reflect.Type) []hiddenParam {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	return walkHidden(opID, t.Name(), t, "", true, map[reflect.Type]bool{})
}

// reachable is false once the walk has passed through an unexported embed —
// everything below it is invisible regardless of its own visibility.
func walkHidden(opID, inputName string, t reflect.Type, path string, reachable bool, seen map[reflect.Type]bool) []hiddenParam {
	if seen[t] {
		return nil
	}
	seen[t] = true
	defer delete(seen, t)

	var out []hiddenParam
	for i := range t.NumField() {
		f := t.Field(i)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		if f.Anonymous && ft.Kind() == reflect.Struct {
			here := f.Name
			if path != "" {
				here = path + "." + f.Name
			}
			// An embedded field is named after its type, so an unexported type
			// name makes the embed itself unreachable.
			out = append(out, walkHidden(opID, inputName, ft, here, reachable && f.IsExported(), seen)...)
			continue
		}

		for _, tag := range paramTags {
			v, ok := f.Tag.Lookup(tag)
			if !ok {
				continue
			}
			reason := ""
			switch {
			case !reachable:
				reason = "it sits inside an unexported embedded struct"
			case !f.IsExported():
				reason = "the field is unexported"
			default:
				continue // exported and reachable: huma sees it
			}
			out = append(out, hiddenParam{
				Operation: opID, Input: inputName, Path: path, Tag: tag,
				Name: strings.SplitN(v, ",", 2)[0], Field: f.Name, Reason: reason,
			})
		}
	}
	return out
}

// hiddenParamError builds the mount-time error. It names every unreachable
// parameter and states the fix, because the symptom on its own reads like
// anything but a visibility problem.
func hiddenParamError(found []hiddenParam) error {
	if len(found) == 0 {
		return nil
	}
	lines := make([]string, 0, len(found))
	for _, e := range found {
		lines = append(lines, "  "+e.String())
	}
	sort.Strings(lines)
	return fmt.Errorf(
		"gat: %d request parameter(s) unreachable by huma:\n%s\n\n"+
			"huma skips unexported fields when it discovers parameters, and an embedded field\n"+
			"takes its name from its type \u2014 so embedding an unexported type hides everything\n"+
			"inside it. The parameter is absent from the OpenAPI document and is never bound,\n"+
			"so the handler silently reads the zero value.\n\n"+
			"Fix: export the type or field (`scope` -> `Scope`). Exported embeds work fine.\n"+
			"Call g.AllowEmbeddedParams(true) before RegisterHuma to downgrade this to a warning.",
		len(found), strings.Join(lines, "\n"))
}

// CheckEmbeddedParams reports request parameters huma cannot reach in the given
// input types — on an unexported field, or inside an embedded unexported type —
// as a single error naming each one.
//
// RegisterHuma already applies this to every operation registered through
// gat.Register. This is the same detector, exported for the inputs gat cannot
// see: operations registered with plain huma.Register, or a codebase using huma
// without gat at all. gat cannot recover those Go types from the OpenAPI
// document, precisely because the parameter is missing from it.
//
// Intended as a test assertion:
//
//	if err := gat.CheckEmbeddedParams(reflect.TypeFor[myInput]()); err != nil {
//	    t.Fatal(err)
//	}
//
// Returns nil when every parameter is declared directly on its input struct.
//
// Stability: experimental
func CheckEmbeddedParams(inputs ...reflect.Type) error {
	var found []hiddenParam
	for _, t := range inputs {
		name := "input"
		if t != nil {
			name = t.String()
		}
		found = append(found, findHiddenParams(name, t)...)
	}
	return hiddenParamError(found)
}

// hiddenParams collects unreachable parameters across every captured operation.
func (g *Gateway) hiddenParams() []hiddenParam {
	var out []hiddenParam
	for _, c := range g.captured {
		out = append(out, findHiddenParams(c.op.OperationID, c.inputType)...)
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
