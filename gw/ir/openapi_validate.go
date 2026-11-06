package ir

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/getkin/kin-openapi/openapi3"
)

// Validation modes for an arg the spec declares base64: see
// NewOpenAPIValidator.
const (
	ValidationStrict = "strict"
	ValidationWarn   = "warn"
	ValidationOff    = "off"
)

// CheckValidation reports whether v is a validation mode. "" is the
// default, strict.
//
// Stability: experimental
func CheckValidation(v string) error {
	switch v {
	case "", ValidationStrict, ValidationWarn, ValidationOff:
		return nil
	}
	return fmt.Errorf("validation %q: want strict, warn or off", v)
}

// DecodeBase64 reads a base64 value the way protojson reads bytes:
// standard or URL-safe alphabet, with or without padding.
//
// Stability: experimental
func DecodeBase64(s string) ([]byte, error) {
	enc := base64.StdEncoding
	if strings.ContainsAny(s, "-_") {
		enc = base64.URLEncoding
	}
	if len(s)%4 != 0 {
		enc = enc.WithPadding(base64.NoPadding)
	}
	return enc.DecodeString(s)
}

// OpenAPIValidator checks one operation's canonical args against the
// base64 declarations in its parameters and JSON body. A nil validator
// accepts everything.
//
// Stability: experimental
type OpenAPIValidator struct {
	root       *base64Node
	resp       *base64Node // success response body; nil when it declares nothing
	hasArgs    bool
	label      string
	warned     atomic.Int32
	respWarned atomic.Int32
}

// maxValidationWarnings caps warn-mode log lines per operation. Paths
// hold client-chosen map keys and list indexes, so "once per path"
// would let a client grow the log without bound.
const maxValidationWarnings = 16

// NewOpenAPIValidator compiles op. It returns nil when op declares
// nothing to check, so an operation without base64 fields costs one
// nil check per call. label names the operation in warnings.
//
// A multipart body is checked for the string parts that are not file
// uploads (see ingestOpenAPIFormDataBody): a plain format:byte part
// is a file, but contentEncoding: base64, or a format:byte wrapped in
// allOf / oneOf / nullable, is base64 text. A oneOf / anyOf with
// several branches passes when any branch whose JSON type matches the
// value accepts it. The success response is compiled too, for
// ValidateResponse.
//
// Stability: experimental
func NewOpenAPIValidator(op *openapi3.Operation, label string) *OpenAPIValidator {
	if op == nil {
		return nil
	}
	c := base64Compiler{nodes: map[*openapi3.Schema]*base64Node{}, seen: map[base64Visit]bool{}}
	root := &base64Node{props: map[string]*base64Node{}}
	add := func(name string, ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil {
			return
		}
		if n := root.props[name]; n != nil {
			c.merge(n, ref.Value)
			return
		}
		root.props[name] = c.node(ref.Value)
	}
	for _, p := range op.Parameters {
		if p != nil && p.Value != nil {
			add(p.Value.Name, p.Value.Schema)
		}
	}
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		body := op.RequestBody.Value
		if mt, multipart := body.Content["multipart/form-data"]; multipart {
			// Args are keyed by property name. An upload part carries
			// a *Upload, not text, so it is skipped as ingest does.
			if mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
				for name, prop := range mt.Schema.Value.Properties {
					if prop != nil && prop.Ref == "" && openapiByteProp(prop) {
						continue
					}
					add(name, prop)
				}
			}
		} else if mt, ok := body.Content["application/json"]; ok && mt != nil {
			add("body", mt.Schema)
		}
	}
	var resp *base64Node
	if op.Responses != nil {
		for _, code := range []string{"200", "201"} {
			r := op.Responses.Value(code)
			if r == nil || r.Value == nil {
				continue
			}
			if mt := r.Value.Content["application/json"]; mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
				resp = c.node(mt.Schema.Value)
				break
			}
		}
	}
	hasArgs := root.declares(map[*base64Node]bool{})
	if !resp.declares(map[*base64Node]bool{}) {
		resp = nil
	}
	if !hasArgs && resp == nil {
		return nil
	}
	return &OpenAPIValidator{root: root, resp: resp, hasArgs: hasArgs, label: label}
}

// Validate returns an error naming the args that do not decode. In
// warn mode it logs them, up to maxValidationWarnings times for the
// operation, and returns nil; off does not check. The error and the
// log carry paths, never values.
func (v *OpenAPIValidator) Validate(args map[string]any, mode string) error {
	if v == nil || !v.hasArgs || mode == ValidationOff {
		return nil
	}
	var bad []string
	v.root.walk("", args, &bad)
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	if mode != ValidationWarn {
		return fmt.Errorf("invalid base64 in %s", strings.Join(bad, ", "))
	}
	if v.warned.Load() < maxValidationWarnings && v.warned.Add(1) <= maxValidationWarnings {
		log.Printf("warning: %s: invalid base64 in %q", v.label, bad)
	}
	return nil
}

// ValidateResponse checks an upstream response body against the
// base64 declarations in the operation's success response. It only
// acts in warn mode and only logs: strict would turn an upstream's
// bad bytes into a client error, and the client sent nothing wrong.
// Paths are logged, never values. body is the decoded JSON.
func (v *OpenAPIValidator) ValidateResponse(body any, mode string) {
	if v == nil || v.resp == nil || mode != ValidationWarn {
		return
	}
	var bad []string
	v.resp.walk("", body, &bad)
	if len(bad) == 0 {
		return
	}
	sort.Strings(bad)
	if v.respWarned.Load() < maxValidationWarnings && v.respWarned.Add(1) <= maxValidationWarnings {
		log.Printf("warning: %s: response has invalid base64 in %q", v.label, bad)
	}
}

// base64Node is the part of a schema that leads to base64 strings.
// Recursive schemas compile to cyclic nodes.
type base64Node struct {
	leaf   bool
	props  map[string]*base64Node
	items  *base64Node
	values *base64Node // additionalProperties
	// kinds is the JSON types the schema allows (kindString ...); zero
	// means any.
	kinds uint8
	// groups holds one oneOf / anyOf each: a value passes a group when
	// some branch of matching JSON type accepts it.
	groups [][]*base64Node
}

const (
	kindString uint8 = 1 << iota
	kindNumber
	kindBool
	kindObject
	kindArray
)

// jsonKind returns the kind of a decoded JSON value, or 0 for null
// and Go types a decoded body does not hold.
func jsonKind(v any) uint8 {
	switch v.(type) {
	case string:
		return kindString
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return kindNumber
	case bool:
		return kindBool
	case map[string]any:
		return kindObject
	case []any:
		return kindArray
	}
	return 0
}

func (n *base64Node) declares(seen map[*base64Node]bool) bool {
	if n == nil || seen[n] {
		return false
	}
	seen[n] = true
	if n.leaf || n.items.declares(seen) || n.values.declares(seen) {
		return true
	}
	for _, g := range n.groups {
		for _, b := range g {
			if b.declares(seen) {
				return true
			}
		}
	}
	for _, c := range n.props {
		if c.declares(seen) {
			return true
		}
	}
	return false
}

func (n *base64Node) walk(path string, v any, bad *[]string) {
	if k := jsonKind(v); k != 0 {
		for _, g := range n.groups {
			walkGroup(g, k, path, v, bad)
		}
	}
	switch x := v.(type) {
	case string:
		if n.leaf {
			if _, err := DecodeBase64(x); err != nil {
				*bad = append(*bad, path)
			}
		}
	case []any:
		if n.items != nil {
			for i, e := range x {
				n.items.walk(path+"["+strconv.Itoa(i)+"]", e, bad)
			}
		}
	case map[string]any:
		for k, e := range x {
			c := n.props[k]
			if c == nil {
				c = n.values
			}
			if c == nil {
				continue
			}
			p := k
			if path != "" {
				p = path + "." + k
			}
			c.walk(p, e, bad)
		}
	}
}

// walkGroup checks v against the branches of one oneOf / anyOf. A
// branch applies when its JSON type allows v's kind. The group passes
// when any applicable branch accepts; this also covers a value that
// fits several branches. When every applicable branch rejects, the
// paths of the closest one (fewest) are reported.
func walkGroup(g []*base64Node, kind uint8, path string, v any, bad *[]string) {
	var best []string
	applied := false
	for _, b := range g {
		if b.kinds != 0 && b.kinds&kind == 0 {
			continue
		}
		var got []string
		b.walk(path, v, &got)
		if len(got) == 0 {
			return
		}
		if !applied || len(got) < len(best) {
			best = got
		}
		applied = true
	}
	*bad = append(*bad, best...)
}

type base64Visit struct {
	n *base64Node
	s *openapi3.Schema
}

type base64Compiler struct {
	nodes map[*openapi3.Schema]*base64Node
	seen  map[base64Visit]bool
}

func (c *base64Compiler) node(s *openapi3.Schema) *base64Node {
	if n := c.nodes[s]; n != nil {
		return n
	}
	n := &base64Node{}
	c.nodes[s] = n
	c.merge(n, s)
	return n
}

// merge adds s to n. allOf branches and a nullable wrapper describe
// the same value, so they merge into the one node.
func (c *base64Compiler) merge(n *base64Node, s *openapi3.Schema) {
	v := base64Visit{n, s}
	if c.seen[v] {
		return
	}
	c.seen[v] = true
	if s.Type != nil {
		for _, t := range *s.Type {
			switch t {
			case "string":
				n.kinds |= kindString
			case "integer", "number":
				n.kinds |= kindNumber
			case "boolean":
				n.kinds |= kindBool
			case "object":
				n.kinds |= kindObject
			case "array":
				n.kinds |= kindArray
			}
		}
	}
	if s.Type != nil && s.Type.Includes("string") && (s.Format == "byte" || s.ContentEncoding == "base64") {
		n.leaf = true
	}
	for k, ref := range s.Properties {
		if ref == nil || ref.Value == nil {
			continue
		}
		if existing := n.props[k]; existing != nil {
			c.merge(existing, ref.Value)
			continue
		}
		if n.props == nil {
			n.props = map[string]*base64Node{}
		}
		n.props[k] = c.node(ref.Value)
	}
	if s.Items != nil && s.Items.Value != nil {
		if n.items != nil {
			c.merge(n.items, s.Items.Value)
		} else {
			n.items = c.node(s.Items.Value)
		}
	}
	if ap := s.AdditionalProperties.Schema; ap != nil && ap.Value != nil {
		if n.values != nil {
			c.merge(n.values, ap.Value)
		} else {
			n.values = c.node(ap.Value)
		}
	}
	for _, ref := range s.AllOf {
		if ref != nil && ref.Value != nil {
			c.merge(n, ref.Value)
		}
	}
	if inner, _ := openapiNullableWrapper(s); inner != nil && inner.Value != nil && len(s.AllOf) == 0 {
		c.merge(n, inner.Value)
		return
	}
	for _, list := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf} {
		var g []*base64Node
		for _, ref := range list {
			if ref == nil || ref.Value == nil || isNullTypeSchema(ref.Value) {
				continue
			}
			g = append(g, c.node(ref.Value))
		}
		if len(g) > 1 {
			n.groups = append(n.groups, g)
		}
	}
}

func isNullTypeSchema(s *openapi3.Schema) bool {
	return s.Type != nil && len(*s.Type) == 1 && (*s.Type)[0] == "null"
}
