package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// schemaModel is a comparison-friendly view of an SDL document. We
// parse two SDLs into this shape and walk them side-by-side to emit
// breaking and informational changes.
type schemaModel struct {
	objects map[string]*objectType
	inputs  map[string]*inputType
	enums   map[string]*enumType
	scalars map[string]struct{}
	// unions, interfaces omitted — gateway doesn't emit them today.
}

type objectType struct {
	fields map[string]*fieldDef
}

type fieldDef struct {
	typ  string
	args map[string]*argDef
	dep  bool
}

type argDef struct {
	typ          string
	hasDefault   bool
	defaultValue string
}

type inputType struct {
	fields map[string]*inputField
}

type inputField struct {
	typ          string
	hasDefault   bool
	defaultValue string
}

type enumType struct {
	values map[string]bool // value → deprecated?
}

func parseSchemaModel(sdl string) (*schemaModel, error) {
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(sdl)})})
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	m := &schemaModel{
		objects: map[string]*objectType{},
		inputs:  map[string]*inputType{},
		enums:   map[string]*enumType{},
		scalars: map[string]struct{}{},
	}
	for _, def := range doc.Definitions {
		switch d := def.(type) {
		case *ast.ObjectDefinition:
			ot := &objectType{fields: map[string]*fieldDef{}}
			for _, f := range d.Fields {
				fd := &fieldDef{
					typ:  typeNodeString(f.Type),
					args: map[string]*argDef{},
					dep:  hasDeprecated(f.Directives),
				}
				for _, a := range f.Arguments {
					fd.args[a.Name.Value] = &argDef{
						typ:          typeNodeString(a.Type),
						hasDefault:   a.DefaultValue != nil,
						defaultValue: valueNodeString(a.DefaultValue),
					}
				}
				ot.fields[f.Name.Value] = fd
			}
			m.objects[d.Name.Value] = ot
		case *ast.InputObjectDefinition:
			it := &inputType{fields: map[string]*inputField{}}
			for _, f := range d.Fields {
				it.fields[f.Name.Value] = &inputField{
					typ:          typeNodeString(f.Type),
					hasDefault:   f.DefaultValue != nil,
					defaultValue: valueNodeString(f.DefaultValue),
				}
			}
			m.inputs[d.Name.Value] = it
		case *ast.EnumDefinition:
			et := &enumType{values: map[string]bool{}}
			for _, v := range d.Values {
				et.values[v.Name.Value] = hasDeprecated(v.Directives)
			}
			m.enums[d.Name.Value] = et
		case *ast.ScalarDefinition:
			m.scalars[d.Name.Value] = struct{}{}
		}
	}
	return m, nil
}

func typeNodeString(t ast.Type) string {
	switch x := t.(type) {
	case *ast.NonNull:
		return typeNodeString(x.Type) + "!"
	case *ast.List:
		return "[" + typeNodeString(x.Type) + "]"
	case *ast.Named:
		return x.Name.Value
	}
	return ""
}

func valueNodeString(v ast.Value) string {
	if v == nil {
		return ""
	}
	if s, ok := v.GetValue().(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v.GetValue())
}

func hasDeprecated(dirs []*ast.Directive) bool {
	for _, d := range dirs {
		if d.Name.Value == "deprecated" {
			return true
		}
	}
	return false
}

// change is one schema delta. Severity is "breaking" or "info".
type change struct {
	severity string
	msg      string
}

func diffModels(old, new *schemaModel) []change {
	var out []change

	// Object types
	for name := range old.objects {
		if _, ok := new.objects[name]; !ok {
			out = append(out, change{"breaking", "type removed: " + name})
		}
	}
	for name, no := range new.objects {
		oo, ok := old.objects[name]
		if !ok {
			out = append(out, change{"info", "type added: " + name})
			continue
		}
		out = append(out, diffObject(name, oo, no)...)
	}

	// Input types — fields are read by clients via arguments, so the
	// breakage rules are inverted relative to output objects.
	for name := range old.inputs {
		if _, ok := new.inputs[name]; !ok {
			out = append(out, change{"breaking", "input removed: " + name})
		}
	}
	for name, ni := range new.inputs {
		oi, ok := old.inputs[name]
		if !ok {
			out = append(out, change{"info", "input added: " + name})
			continue
		}
		out = append(out, diffInput(name, oi, ni)...)
	}

	// Enums
	for name := range old.enums {
		if _, ok := new.enums[name]; !ok {
			out = append(out, change{"breaking", "enum removed: " + name})
		}
	}
	for name, ne := range new.enums {
		oe, ok := old.enums[name]
		if !ok {
			out = append(out, change{"info", "enum added: " + name})
			continue
		}
		for v := range oe.values {
			if _, ok := ne.values[v]; !ok {
				out = append(out, change{"breaking", fmt.Sprintf("enum value removed: %s.%s", name, v)})
			}
		}
		for v := range ne.values {
			if _, ok := oe.values[v]; !ok {
				out = append(out, change{"info", fmt.Sprintf("enum value added: %s.%s", name, v)})
			}
		}
	}

	// Scalars
	for name := range old.scalars {
		if _, ok := new.scalars[name]; !ok {
			out = append(out, change{"breaking", "scalar removed: " + name})
		}
	}
	for name := range new.scalars {
		if _, ok := old.scalars[name]; !ok {
			out = append(out, change{"info", "scalar added: " + name})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].severity != out[j].severity {
			return out[i].severity == "breaking"
		}
		return out[i].msg < out[j].msg
	})
	return out
}

func diffObject(name string, oo, no *objectType) []change {
	var out []change
	for fn := range oo.fields {
		if _, ok := no.fields[fn]; !ok {
			out = append(out, change{"breaking", fmt.Sprintf("field removed: %s.%s", name, fn)})
		}
	}
	for fn, nf := range no.fields {
		of, ok := oo.fields[fn]
		if !ok {
			out = append(out, change{"info", fmt.Sprintf("field added: %s.%s", name, fn)})
			continue
		}
		if of.typ != nf.typ {
			// Output type change: any change is potentially breaking.
			// Loosening non-null → nullable in OUTPUT position is also
			// breaking because clients may have non-null typings.
			out = append(out, change{"breaking", fmt.Sprintf("field type changed: %s.%s %s → %s", name, fn, of.typ, nf.typ)})
		}
		// Args
		for an := range of.args {
			if _, ok := nf.args[an]; !ok {
				out = append(out, change{"breaking", fmt.Sprintf("arg removed: %s.%s(%s)", name, fn, an)})
			}
		}
		for an, na := range nf.args {
			oa, ok := of.args[an]
			if !ok {
				// New required arg = breaking; new optional arg = info.
				if strings.HasSuffix(na.typ, "!") && !na.hasDefault {
					out = append(out, change{"breaking", fmt.Sprintf("required arg added: %s.%s(%s: %s)", name, fn, an, na.typ)})
				} else {
					out = append(out, change{"info", fmt.Sprintf("optional arg added: %s.%s(%s: %s)", name, fn, an, na.typ)})
				}
				continue
			}
			if oa.typ != na.typ {
				out = append(out, change{"breaking", fmt.Sprintf("arg type changed: %s.%s(%s) %s → %s", name, fn, an, oa.typ, na.typ)})
			}
		}
		// Deprecation flips are informational.
		if !of.dep && nf.dep {
			out = append(out, change{"info", fmt.Sprintf("field deprecated: %s.%s", name, fn)})
		}
	}
	return out
}

func diffInput(name string, oi, ni *inputType) []change {
	var out []change
	// Removing a field from input is breaking only if clients passed
	// it; we conservatively flag removal as breaking.
	for fn := range oi.fields {
		if _, ok := ni.fields[fn]; !ok {
			out = append(out, change{"breaking", fmt.Sprintf("input field removed: %s.%s", name, fn)})
		}
	}
	for fn, nf := range ni.fields {
		of, ok := oi.fields[fn]
		if !ok {
			// Adding a required input field with no default = breaking.
			if strings.HasSuffix(nf.typ, "!") && !nf.hasDefault {
				out = append(out, change{"breaking", fmt.Sprintf("required input field added: %s.%s: %s", name, fn, nf.typ)})
			} else {
				out = append(out, change{"info", fmt.Sprintf("optional input field added: %s.%s: %s", name, fn, nf.typ)})
			}
			continue
		}
		if of.typ != nf.typ {
			out = append(out, change{"breaking", fmt.Sprintf("input field type changed: %s.%s %s → %s", name, fn, of.typ, nf.typ)})
		}
	}
	return out
}
