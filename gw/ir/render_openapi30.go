package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPI30Version is the "openapi" field of a document that
// RenderOpenAPIWith converted down to 3.0. 3.0.3, not 3.0.0: the
// nullable forms the conversion writes (`allOf: [$ref], nullable:
// true`, `nullable` beside `enum`) are the ones 3.0.3 clarified. A
// synthesized spec (proto or GraphQL source) keeps its "3.0.0".
const OpenAPI30Version = "3.0.3"

// Extensions the down-conversion writes. Each one keeps something the
// target version has no field for, so nothing in the source is
// dropped; tools of the target version ignore them.
const (
	// xOAS31 holds, verbatim, the members of an object that OpenAPI
	// 3.1 has and 3.0 does not: JSON Schema 2020-12 keywords
	// (prefixItems, if/then/else, ...), info.summary,
	// license.identifier, components.pathItems, and the like. The 3.1
	// up-conversion puts them back.
	xOAS31 = "x-gwag-oas31"
	// xOAS32 is xOAS31 for members OpenAPI 3.2 added (tag parent and
	// kind, $self, querystring parameters, ...). Nothing puts them
	// back: no output version has them.
	xOAS32 = "x-gwag-oas32"
	// xWebhooks carries a 3.1 document's webhooks in a 3.0 one; ReDoc
	// reads this extension.
	xWebhooks = "x-webhooks"
	// xItemSchema carries a 3.2 Media Type itemSchema, verbatim, in a
	// 3.0 or 3.1 document. IngestOpenAPI reads it back.
	xItemSchema = "x-item-schema"
)

// The JSON Schema dialects a document gets when it names none. A
// jsonSchemaDialect equal to one of them says nothing and is dropped.
var defaultSchemaDialects = map[string]bool{
	"https://spec.openapis.org/oas/3.1/dialect/base":       true,
	"https://spec.openapis.org/oas/3.2/dialect/2025-09-17": true,
}

// downcastOpenAPI returns a copy of doc (an OpenAPI 3.1 or 3.2
// document) as the target version, "3.0" or "3.1". The conversion
// works on the JSON form so that every member survives, including the
// ones kin-openapi keeps only as raw siblings ($ref siblings, unknown
// keywords). doc is not modified.
func downcastOpenAPI(doc *openapi3.T, target string) (*openapi3.T, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("openapi %s: marshal: %w", target, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("openapi %s: decode: %w", target, err)
	}
	c := &downcaster{
		from32: doc.IsOpenAPI32OrLater(),
		to30:   target == "3.0",
		root:   root,
		defs:   map[string]string{},
	}
	c.document()
	if c.to30 {
		root["openapi"] = OpenAPI30Version
	} else {
		root["openapi"] = OpenAPI31Version
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("openapi %s: encode: %w", target, err)
	}
	res := &openapi3.T{}
	if err := json.Unmarshal(out, res); err != nil {
		return nil, fmt.Errorf("openapi %s: copy: %w", target, err)
	}
	return res, nil
}

type downcaster struct {
	from32 bool // the source is 3.2: 3.2 members move out
	to30   bool // the target is 3.0: 3.1 members move out too

	root map[string]any

	// Source components looked up while converting, saved before the
	// conversion moves them.
	pathItems  map[string]any // components.pathItems (3.1)
	mediaTypes map[string]any // components.mediaTypes (3.2)
	qsParams   map[string]bool
	mtls       map[string]bool // mutualTLS security schemes (3.1)

	// $defs moved to components.schemas: old JSON pointer → new $ref.
	defs    map[string]string
	pending []pendingDef
	names   map[string]bool // components.schemas names in use
}

type pendingDef struct {
	name   string
	schema any
}

type jmap = map[string]any

func (c *downcaster) document() {
	root := c.root
	comps, _ := root["components"].(jmap)
	c.names = map[string]bool{}
	c.qsParams = map[string]bool{}
	c.mtls = map[string]bool{}
	if comps != nil {
		c.pathItems, _ = comps["pathItems"].(jmap)
		c.mediaTypes, _ = comps["mediaTypes"].(jmap)
		if s, ok := comps["schemas"].(jmap); ok {
			for k := range s {
				c.names[k] = true
			}
		}
		if ps, ok := comps["parameters"].(jmap); ok {
			for k, p := range ps {
				if m, ok := p.(jmap); ok && m["in"] == "querystring" {
					c.qsParams[k] = true
				}
			}
		}
		if ss, ok := comps["securitySchemes"].(jmap); ok {
			for k, s := range ss {
				if m, ok := s.(jmap); ok && m["type"] == "mutualTLS" {
					c.mtls[k] = true
				}
			}
		}
	}

	if info, ok := root["info"].(jmap); ok {
		c.filter(info, "info")
		if lic, ok := info["license"].(jmap); ok {
			c.filter(lic, "license")
		}
	}
	c.servers(root)
	if tags, ok := root["tags"].([]any); ok {
		for _, t := range tags {
			if m, ok := t.(jmap); ok {
				c.filter(m, "tag")
				c.externalDocs(m)
			}
		}
	}
	c.externalDocs(root)
	if paths, ok := root["paths"].(jmap); ok {
		for _, k := range sortedKeys(paths) {
			c.pathItemAt(paths, k, "#/paths/"+escapePointer(k))
		}
	} else if c.to30 {
		root["paths"] = jmap{} // optional in 3.1, required in 3.0
	}
	if hooks, ok := root["webhooks"].(jmap); ok {
		for _, k := range sortedKeys(hooks) {
			c.pathItemAt(hooks, k, "#/webhooks/"+escapePointer(k))
		}
		if c.to30 {
			delete(root, "webhooks")
			existing, _ := root[xWebhooks].(jmap)
			for k, v := range existing {
				if _, ok := hooks[k]; !ok {
					hooks[k] = v
				}
			}
			root[xWebhooks] = hooks
		}
	}
	if comps != nil {
		c.components(comps)
	}
	c.security(root)
	if d, ok := root["jsonSchemaDialect"].(string); ok && (c.to30 || c.from32) {
		if defaultSchemaDialects[d] {
			delete(root, "jsonSchemaDialect")
		}
	}
	c.filter(root, "doc")

	// $defs found on the way become components; they may hold more.
	for len(c.pending) > 0 {
		p := c.pending[0]
		c.pending = c.pending[1:]
		comps, _ := c.root["components"].(jmap)
		if comps == nil {
			comps = jmap{}
			c.root["components"] = comps
		}
		schemas, _ := comps["schemas"].(jmap)
		if schemas == nil {
			schemas = jmap{}
			comps["schemas"] = schemas
		}
		schemas[p.name] = p.schema
		c.schemaAt(schemas, p.name, "#/components/schemas/"+escapePointer(p.name))
	}
	if len(c.defs) > 0 {
		c.rewriteRefs(c.root)
	}
}

// fields lists the members each object has in 3.0 (v30) and the ones
// 3.1 added (v31). A member in neither is a 3.2 addition or unknown.
var fields = map[string]struct{ v30, v31 map[string]bool }{
	"doc": {set("openapi", "info", "servers", "paths", "components", "security", "tags", "externalDocs"),
		set("webhooks", "jsonSchemaDialect")},
	"info":         {set("title", "description", "termsOfService", "contact", "license", "version"), set("summary")},
	"license":      {set("name", "url"), set("identifier")},
	"server":       {set("url", "description", "variables"), nil},
	"externalDocs": {set("description", "url"), nil},
	"tag":          {set("name", "description", "externalDocs"), nil},
	"components": {set("schemas", "responses", "parameters", "examples", "requestBodies", "headers",
		"securitySchemes", "links", "callbacks"), set("pathItems")},
	"pathItem": {set("$ref", "summary", "description", "get", "put", "post", "delete", "options",
		"head", "patch", "trace", "servers", "parameters"), nil},
	"operation": {set("tags", "summary", "description", "externalDocs", "operationId", "parameters",
		"requestBody", "responses", "callbacks", "deprecated", "security", "servers"), nil},
	"parameter": {set("name", "in", "description", "required", "deprecated", "allowEmptyValue",
		"style", "explode", "allowReserved", "schema", "example", "examples", "content"), nil},
	"header": {set("description", "required", "deprecated", "allowEmptyValue", "style", "explode",
		"allowReserved", "schema", "example", "examples", "content"), nil},
	"requestBody":    {set("description", "content", "required"), nil},
	"mediaType":      {set("schema", "example", "examples", "encoding"), nil},
	"encoding":       {set("contentType", "headers", "style", "explode", "allowReserved"), nil},
	"response":       {set("description", "headers", "content", "links"), nil},
	"example":        {set("summary", "description", "value", "externalValue"), nil},
	"link":           {set("operationRef", "operationId", "parameters", "requestBody", "description", "server"), nil},
	"securityScheme": {set("type", "description", "name", "in", "scheme", "bearerFormat", "flows", "openIdConnectUrl"), nil},
	"oauthFlows":     {set("implicit", "password", "clientCredentials", "authorizationCode"), nil},
	"discriminator":  {set("propertyName", "mapping"), nil},
	"xml":            {set("name", "namespace", "prefix", "attribute", "wrapped"), nil},
	"ref":            {set("$ref"), set("summary", "description")},
	"schema": {set("$ref", "title", "multipleOf", "maximum", "exclusiveMaximum", "minimum",
		"exclusiveMinimum", "maxLength", "minLength", "pattern", "maxItems", "minItems", "uniqueItems",
		"maxProperties", "minProperties", "required", "enum", "type", "allOf", "oneOf", "anyOf", "not",
		"items", "properties", "additionalProperties", "description", "format", "default", "nullable",
		"discriminator", "readOnly", "writeOnly", "xml", "externalDocs", "example", "deprecated"),
		set("const", "examples", "prefixItems", "contains", "minContains", "maxContains",
			"patternProperties", "dependentSchemas", "propertyNames", "unevaluatedItems",
			"unevaluatedProperties", "if", "then", "else", "dependentRequired", "$defs", "$schema",
			"$comment", "$id", "$anchor", "$dynamicRef", "$dynamicAnchor", "$vocabulary",
			"contentMediaType", "contentEncoding", "contentSchema")},
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// filter moves the members of m that the target version lacks into
// m's xOAS31 / xOAS32 extension. Schemas are only filtered for a 3.0
// target: 3.1 and 3.2 share the JSON Schema dialect.
func (c *downcaster) filter(m jmap, kind string) {
	f := fields[kind]
	if kind == "schema" && !c.to30 {
		return
	}
	for _, k := range sortedKeys(m) {
		if strings.HasPrefix(k, "x-") || f.v30[k] {
			continue
		}
		if f.v31[k] {
			if c.to30 {
				keep(m, xOAS31, k)
			}
			continue
		}
		switch {
		case c.from32:
			keep(m, xOAS32, k)
		case c.to30:
			keep(m, xOAS31, k)
		}
	}
}

// keep moves m[key] into the extension object m[bag].
func keep(m jmap, bag, key string) {
	v, ok := m[key]
	if !ok {
		return
	}
	delete(m, key)
	b, _ := m[bag].(jmap)
	if b == nil {
		b = jmap{}
		m[bag] = b
	}
	b[key] = v
}

func (c *downcaster) servers(m jmap) {
	list, _ := m["servers"].([]any)
	for _, s := range list {
		if sm, ok := s.(jmap); ok {
			c.filter(sm, "server")
		}
	}
}

func (c *downcaster) externalDocs(m jmap) {
	if e, ok := m["externalDocs"].(jmap); ok {
		c.filter(e, "externalDocs")
	}
}

// refObject handles a Reference Object outside a schema. 3.0 allows
// only $ref; 3.1 also summary and description.
func (c *downcaster) refObject(m jmap) bool {
	if _, ok := m["$ref"].(string); !ok {
		return false
	}
	c.filter(m, "ref")
	return true
}

func (c *downcaster) pathItemAt(parent jmap, key, ptr string) {
	item, _ := parent[key].(jmap)
	if item == nil {
		return
	}
	if c.to30 {
		// A $ref to components.pathItems (3.1) has no 3.0 target:
		// inline it. The sibling summary / description win.
		for i := 0; i < 8; i++ {
			ref, _ := item["$ref"].(string)
			name, ok := strings.CutPrefix(ref, "#/components/pathItems/")
			if !ok {
				break
			}
			target, _ := c.pathItems[unescapePointer(name)].(jmap)
			if target == nil {
				break
			}
			inlined := deepCopy(target).(jmap)
			delete(item, "$ref")
			for k, v := range item {
				inlined[k] = v
			}
			item = inlined
			parent[key] = item
		}
	}
	if c.from32 {
		extra, _ := item[xAdditionalOperations].(jmap)
		move := func(method string, op any) {
			if extra == nil {
				extra = jmap{}
				item[xAdditionalOperations] = extra
			}
			if _, ok := extra[method]; !ok {
				extra[method] = op
			}
		}
		if op, ok := item["query"]; ok {
			delete(item, "query")
			move(openapi3.MethodQuery, op)
		}
		if ops, ok := item["additionalOperations"].(jmap); ok {
			delete(item, "additionalOperations")
			for _, m := range sortedKeys(ops) {
				move(m, ops[m])
			}
		}
	}
	c.filter(item, "pathItem")
	c.servers(item)
	c.parameters(item, ptr)
	for _, m := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
		if op, ok := item[m].(jmap); ok {
			c.operation(op, ptr+"/"+m)
		}
	}
	if extra, ok := item[xAdditionalOperations].(jmap); ok {
		for _, m := range sortedKeys(extra) {
			if op, ok := extra[m].(jmap); ok {
				c.operation(op, ptr+"/"+xAdditionalOperations+"/"+escapePointer(m))
			}
		}
	}
}

func (c *downcaster) operation(op jmap, ptr string) {
	c.filter(op, "operation")
	c.externalDocs(op)
	c.servers(op)
	c.parameters(op, ptr)
	if rb, ok := op["requestBody"].(jmap); ok && !c.refObject(rb) {
		c.filter(rb, "requestBody")
		c.content(rb, ptr+"/requestBody")
	}
	if rs, ok := op["responses"].(jmap); ok {
		for _, code := range sortedKeys(rs) {
			if r, ok := rs[code].(jmap); ok {
				c.response(r, ptr+"/responses/"+escapePointer(code))
			}
		}
	} else if c.to30 {
		// Optional in 3.1, required (and non-empty) in 3.0.
		op["responses"] = jmap{"default": jmap{"description": ""}}
	}
	if cbs, ok := op["callbacks"].(jmap); ok {
		for _, name := range sortedKeys(cbs) {
			c.callback(cbs[name], ptr+"/callbacks/"+escapePointer(name))
		}
	}
	c.security(op)
}

func (c *downcaster) callback(v any, ptr string) {
	cb, _ := v.(jmap)
	if cb == nil || c.refObject(cb) {
		return
	}
	for _, expr := range sortedKeys(cb) {
		if strings.HasPrefix(expr, "x-") {
			continue
		}
		c.pathItemAt(cb, expr, ptr+"/"+escapePointer(expr))
	}
}

// parameters converts owner["parameters"]. A 3.2 querystring
// parameter has no earlier equivalent and moves to owner's xOAS32.
func (c *downcaster) parameters(owner jmap, ptr string) {
	list, ok := owner["parameters"].([]any)
	if !ok {
		return
	}
	var kept, moved []any
	for i, p := range list {
		m, _ := p.(jmap)
		if m == nil {
			kept = append(kept, p)
			continue
		}
		if c.from32 && c.isQueryString(m) {
			moved = append(moved, p)
			continue
		}
		c.parameter(m, ptr+"/parameters/"+strconv.Itoa(i), false)
		kept = append(kept, p)
	}
	if len(moved) == 0 {
		return
	}
	if len(kept) == 0 {
		delete(owner, "parameters")
	} else {
		owner["parameters"] = kept
	}
	b, _ := owner[xOAS32].(jmap)
	if b == nil {
		b = jmap{}
		owner[xOAS32] = b
	}
	b["parameters"] = moved
}

func (c *downcaster) isQueryString(m jmap) bool {
	if ref, ok := m["$ref"].(string); ok {
		name, ok := strings.CutPrefix(ref, "#/components/parameters/")
		return ok && c.qsParams[unescapePointer(name)]
	}
	return m["in"] == "querystring"
}

func (c *downcaster) parameter(m jmap, ptr string, header bool) {
	if c.refObject(m) {
		return
	}
	if header {
		c.filter(m, "header")
	} else {
		c.filter(m, "parameter")
	}
	if _, ok := m["schema"]; ok {
		c.schemaAt(m, "schema", ptr+"/schema")
	}
	c.content(m, ptr)
	c.examples(m)
}

func (c *downcaster) headers(owner jmap, ptr string) {
	hs, _ := owner["headers"].(jmap)
	for _, k := range sortedKeys(hs) {
		if h, ok := hs[k].(jmap); ok {
			c.parameter(h, ptr+"/headers/"+escapePointer(k), true)
		}
	}
}

func (c *downcaster) examples(owner jmap) {
	exs, _ := owner["examples"].(jmap)
	for _, k := range sortedKeys(exs) {
		if e, ok := exs[k].(jmap); ok {
			c.example(e)
		}
	}
}

func (c *downcaster) example(e jmap) {
	if c.refObject(e) {
		return
	}
	if c.from32 {
		// 3.2 dataValue is the value as data; earlier versions call it
		// value.
		_, hasValue := e["value"]
		_, hasExt := e["externalValue"]
		if dv, ok := e["dataValue"]; ok && !hasValue && !hasExt {
			e["value"] = dv
		}
	}
	c.filter(e, "example")
}

func (c *downcaster) response(r jmap, ptr string) {
	if c.refObject(r) {
		return
	}
	if _, ok := r["description"]; !ok {
		// Optional in 3.2, required before.
		r["description"] = ""
	}
	c.filter(r, "response")
	c.headers(r, ptr)
	c.content(r, ptr)
	if links, ok := r["links"].(jmap); ok {
		for _, k := range sortedKeys(links) {
			if l, ok := links[k].(jmap); ok && !c.refObject(l) {
				c.filter(l, "link")
				if s, ok := l["server"].(jmap); ok {
					c.filter(s, "server")
				}
			}
		}
	}
}

func (c *downcaster) content(owner jmap, ptr string) {
	content, _ := owner["content"].(jmap)
	for _, k := range sortedKeys(content) {
		mt, _ := content[k].(jmap)
		if mt == nil {
			continue
		}
		if c.from32 {
			// A $ref to components.mediaTypes (3.2): inline it.
			if ref, ok := mt["$ref"].(string); ok {
				if name, ok := strings.CutPrefix(ref, "#/components/mediaTypes/"); ok {
					if target, ok := c.mediaTypes[unescapePointer(name)].(jmap); ok {
						mt = deepCopy(target).(jmap)
						content[k] = mt
					}
				}
			}
		}
		c.mediaType(mt, ptr+"/content/"+escapePointer(k))
	}
}

func (c *downcaster) mediaType(mt jmap, ptr string) {
	if c.from32 {
		if is, ok := mt["itemSchema"]; ok {
			delete(mt, "itemSchema")
			mt[xItemSchema] = is
		}
	}
	c.filter(mt, "mediaType")
	if _, ok := mt["schema"]; ok {
		c.schemaAt(mt, "schema", ptr+"/schema")
	}
	c.examples(mt)
	if enc, ok := mt["encoding"].(jmap); ok {
		for _, k := range sortedKeys(enc) {
			if e, ok := enc[k].(jmap); ok {
				c.filter(e, "encoding")
				c.headers(e, ptr+"/encoding/"+escapePointer(k))
			}
		}
	}
}

func (c *downcaster) components(comps jmap) {
	if c.to30 {
		// mutualTLS (3.1) has no 3.0 scheme type.
		if ss, ok := comps["securitySchemes"].(jmap); ok && len(c.mtls) > 0 {
			moved := jmap{}
			for name := range c.mtls {
				moved[name] = ss[name]
				delete(ss, name)
			}
			b, _ := comps[xOAS31].(jmap)
			if b == nil {
				b = jmap{}
				comps[xOAS31] = b
			}
			b["securitySchemes"] = moved
		}
	}
	if c.from32 {
		if ps, ok := comps["parameters"].(jmap); ok && len(c.qsParams) > 0 {
			moved := jmap{}
			for name := range c.qsParams {
				moved[name] = ps[name]
				delete(ps, name)
			}
			b, _ := comps[xOAS32].(jmap)
			if b == nil {
				b = jmap{}
				comps[xOAS32] = b
			}
			b["parameters"] = moved
		}
	}
	c.filter(comps, "components")
	if s, ok := comps["schemas"].(jmap); ok {
		for _, k := range sortedKeys(s) {
			c.schemaAt(s, k, "#/components/schemas/"+escapePointer(k))
		}
	}
	if ps, ok := comps["parameters"].(jmap); ok {
		for _, k := range sortedKeys(ps) {
			if p, ok := ps[k].(jmap); ok {
				c.parameter(p, "#/components/parameters/"+escapePointer(k), false)
			}
		}
	}
	c.headers(comps, "#/components")
	if rs, ok := comps["responses"].(jmap); ok {
		for _, k := range sortedKeys(rs) {
			if r, ok := rs[k].(jmap); ok {
				c.response(r, "#/components/responses/"+escapePointer(k))
			}
		}
	}
	if rbs, ok := comps["requestBodies"].(jmap); ok {
		for _, k := range sortedKeys(rbs) {
			if rb, ok := rbs[k].(jmap); ok && !c.refObject(rb) {
				c.filter(rb, "requestBody")
				c.content(rb, "#/components/requestBodies/"+escapePointer(k))
			}
		}
	}
	c.examples(comps)
	if links, ok := comps["links"].(jmap); ok {
		for _, k := range sortedKeys(links) {
			if l, ok := links[k].(jmap); ok && !c.refObject(l) {
				c.filter(l, "link")
			}
		}
	}
	if cbs, ok := comps["callbacks"].(jmap); ok {
		for _, k := range sortedKeys(cbs) {
			c.callback(cbs[k], "#/components/callbacks/"+escapePointer(k))
		}
	}
	if !c.to30 {
		if pis, ok := comps["pathItems"].(jmap); ok {
			for _, k := range sortedKeys(pis) {
				c.pathItemAt(pis, k, "#/components/pathItems/"+escapePointer(k))
			}
		}
	}
	if ss, ok := comps["securitySchemes"].(jmap); ok {
		for _, k := range sortedKeys(ss) {
			s, _ := ss[k].(jmap)
			if s == nil || c.refObject(s) {
				continue
			}
			c.filter(s, "securityScheme")
			if flows, ok := s["flows"].(jmap); ok {
				c.filter(flows, "oauthFlows")
			}
		}
	}
}

// security drops mutualTLS schemes (3.1) from owner's security
// requirements for a 3.0 target, keeping the original list in
// owner's xOAS31. A requirement that needed one is dropped.
func (c *downcaster) security(owner jmap) {
	list, ok := owner["security"].([]any)
	if !ok || !c.to30 || len(c.mtls) == 0 {
		return
	}
	var out []any
	changed := false
	for _, req := range list {
		m, _ := req.(jmap)
		uses := false
		for name := range m {
			if c.mtls[name] {
				uses = true
			}
		}
		if uses {
			changed = true
			continue
		}
		out = append(out, req)
	}
	if !changed {
		return
	}
	b, _ := owner[xOAS31].(jmap)
	if b == nil {
		b = jmap{}
		owner[xOAS31] = b
	}
	b["security"] = deepCopy(list)
	if len(out) == 0 {
		delete(owner, "security")
	} else {
		owner["security"] = out
	}
}

// schemaAt converts the schema at parent[key], at JSON pointer ptr.
func (c *downcaster) schemaAt(parent jmap, key, ptr string) {
	parent[key] = c.schema(parent[key], ptr)
}

func (c *downcaster) schemaList(s jmap, key, ptr string) {
	list, _ := s[key].([]any)
	for i := range list {
		list[i] = c.schema(list[i], ptr+"/"+key+"/"+strconv.Itoa(i))
	}
}

func (c *downcaster) schemaMap(s jmap, key, ptr string) {
	m, _ := s[key].(jmap)
	for _, k := range sortedKeys(m) {
		m[k] = c.schema(m[k], ptr+"/"+key+"/"+escapePointer(k))
	}
}

// schema returns the target-version form of one schema and its
// sub-schemas.
func (c *downcaster) schema(v any, ptr string) any {
	if !c.to30 {
		c.schema32(v)
		return v
	}
	if b, ok := v.(bool); ok {
		// Boolean schemas are 2020-12 only.
		if b {
			return jmap{}
		}
		return jmap{"not": jmap{}}
	}
	s, _ := v.(jmap)
	if s == nil {
		return v
	}
	c.hoistDefs(s, ptr)
	if c.refSiblings(s) {
		return s
	}
	c.typeList(s)
	c.nullBranches(s)
	if cv, ok := s["const"]; ok {
		if _, hasEnum := s["enum"]; !hasEnum {
			delete(s, "const")
			s["enum"] = []any{cv}
			if cv == nil {
				s["nullable"] = true
			}
		}
	}
	exclusive30(s, "exclusiveMinimum", "minimum", 1)
	exclusive30(s, "exclusiveMaximum", "maximum", -1)
	if exs, ok := s["examples"].([]any); ok {
		_, hasExample := s["example"]
		if !hasExample && len(exs) > 0 {
			s["example"] = exs[0]
		}
		if len(exs) == 1 && (!hasExample || jsonEqual(exs[0], s["example"])) {
			delete(s, "examples")
		}
		// More than one: the list stays in xOAS31 (filter below).
	}
	contentFormat(s)
	if _, ok := s["prefixItems"]; ok {
		// With prefixItems, items means "the items after them"; in 3.0
		// it means every item. Both move out, items becomes any.
		keep(s, xOAS31, "items")
		s["items"] = jmap{}
	}
	c.filter(s, "schema")
	if t, _ := s["type"].(string); t == "array" {
		if _, ok := s["items"]; !ok {
			s["items"] = jmap{} // required with type array in 3.0
		}
	}
	c.subObjects(s)
	for _, k := range []string{"allOf", "oneOf", "anyOf"} {
		c.schemaList(s, k, ptr)
	}
	if _, ok := s["not"]; ok {
		c.schemaAt(s, "not", ptr+"/not")
	}
	if _, ok := s["items"]; ok {
		c.schemaAt(s, "items", ptr+"/items")
	}
	c.schemaMap(s, "properties", ptr)
	if _, ok := s["additionalProperties"].(jmap); ok {
		c.schemaAt(s, "additionalProperties", ptr+"/additionalProperties")
	}
	return s
}

// schema32 handles a schema for a 3.1 target from a 3.2 source: the
// dialect is the same, only Discriminator and XML gained members.
func (c *downcaster) schema32(v any) {
	s, _ := v.(jmap)
	if s == nil {
		return
	}
	c.subObjects(s)
	for _, k := range []string{"allOf", "oneOf", "anyOf", "prefixItems"} {
		list, _ := s[k].([]any)
		for _, sub := range list {
			c.schema32(sub)
		}
	}
	for _, k := range []string{"properties", "patternProperties", "dependentSchemas", "$defs"} {
		m, _ := s[k].(jmap)
		for _, sub := range m {
			c.schema32(sub)
		}
	}
	for _, k := range []string{"not", "items", "contains", "propertyNames", "additionalProperties",
		"unevaluatedItems", "unevaluatedProperties", "if", "then", "else", "contentSchema"} {
		c.schema32(s[k])
	}
}

func (c *downcaster) subObjects(s jmap) {
	if d, ok := s["discriminator"].(jmap); ok {
		c.filter(d, "discriminator")
	}
	if x, ok := s["xml"].(jmap); ok {
		c.filter(x, "xml")
	}
	c.externalDocs(s)
}

// hoistDefs moves s's $defs (3.1) to components.schemas and records
// the new $ref of each, so references into them can be rewritten.
func (c *downcaster) hoistDefs(s jmap, ptr string) {
	defs, ok := s["$defs"].(jmap)
	if !ok {
		return
	}
	delete(s, "$defs")
	parent := "Def"
	if name, ok := strings.CutPrefix(ptr, "#/components/schemas/"); ok && !strings.Contains(name, "/") {
		parent = componentName(unescapePointer(name))
	}
	for _, k := range sortedKeys(defs) {
		base := componentName(k)
		name := base
		if c.names[name] {
			name = parent + "_" + base
		}
		for i := 2; c.names[name]; i++ {
			name = parent + "_" + base + strconv.Itoa(i)
		}
		c.names[name] = true
		c.defs[ptr+"/$defs/"+escapePointer(k)] = "#/components/schemas/" + escapePointer(name)
		c.pending = append(c.pending, pendingDef{name: name, schema: defs[k]})
	}
}

// refSiblings rewrites a $ref with sibling keywords (3.1; 3.0 ignores
// them) as `allOf: [{$ref}]` plus the siblings, the 3.0 form that
// applies both. It reports whether s is a $ref, which then needs no
// more conversion beyond its siblings.
func (c *downcaster) refSiblings(s jmap) bool {
	ref, ok := s["$ref"].(string)
	if !ok {
		return false
	}
	siblings := false
	for k := range s {
		if k != "$ref" && !strings.HasPrefix(k, "x-") {
			siblings = true
		}
	}
	if !siblings {
		return true
	}
	delete(s, "$ref")
	all, _ := s["allOf"].([]any)
	s["allOf"] = append([]any{jmap{"$ref": ref}}, all...)
	return false
}

// typeList turns a 2020-12 type list into 3.0 forms: "null" becomes
// nullable, more than one other type becomes an anyOf of one-type
// schemas.
func (c *downcaster) typeList(s jmap) {
	var types []string
	switch t := s["type"].(type) {
	case string:
		if t != "null" {
			return
		}
		types = []string{t}
	case []any:
		for _, e := range t {
			if str, ok := e.(string); ok {
				types = append(types, str)
			}
		}
	default:
		return
	}
	var nonNull []string
	hasNull := false
	for _, t := range types {
		if t == "null" {
			hasNull = true
		} else {
			nonNull = append(nonNull, t)
		}
	}
	delete(s, "type")
	if hasNull {
		s["nullable"] = true
	}
	switch len(nonNull) {
	case 0:
		if _, ok := s["enum"]; !ok && hasNull {
			s["enum"] = []any{nil} // null is the only value
		}
	case 1:
		s["type"] = nonNull[0]
	default:
		// anyOf, not oneOf: integer values are numbers too.
		branches := make([]any, 0, len(nonNull))
		for _, t := range nonNull {
			b := jmap{"type": t}
			if t == "array" {
				if items, ok := s["items"]; ok {
					b["items"] = items
					delete(s, "items")
				} else {
					b["items"] = jmap{}
				}
			}
			branches = append(branches, b)
		}
		if _, ok := s["anyOf"]; !ok {
			s["anyOf"] = branches
		} else {
			all, _ := s["allOf"].([]any)
			s["allOf"] = append(all, jmap{"anyOf": branches})
		}
	}
}

// nullBranches drops `{type: null}` branches from oneOf / anyOf and
// sets nullable instead. One branch left becomes `allOf: [branch]`,
// the 3.0 nullable-$ref form.
func (c *downcaster) nullBranches(s jmap) {
	for _, k := range []string{"oneOf", "anyOf"} {
		list, ok := s[k].([]any)
		if !ok {
			continue
		}
		var rest []any
		for _, b := range list {
			if isNullSchema(b) {
				continue
			}
			rest = append(rest, b)
		}
		if len(rest) == len(list) {
			continue
		}
		s["nullable"] = true
		_, hasAll := s["allOf"]
		switch {
		case len(rest) == 0:
			delete(s, k)
			if _, ok := s["enum"]; !ok {
				s["enum"] = []any{nil}
			}
		case len(rest) == 1 && !hasAll:
			delete(s, k)
			s["allOf"] = rest
		default:
			s[k] = rest
		}
	}
}

// isNullSchema reports whether v accepts only null: `{type: null}`,
// `{type: [null]}`, `{const: null}` or `{enum: [null]}`, with
// annotations at most.
func isNullSchema(v any) bool {
	m, _ := v.(jmap)
	if m == nil {
		return false
	}
	null := false
	for k, val := range m {
		switch k {
		case "type":
			switch t := val.(type) {
			case string:
				null = t == "null"
			case []any:
				null = len(t) == 1 && t[0] == "null"
			}
			if !null {
				return false
			}
		case "const":
			if val != nil {
				return false
			}
			null = true
		case "enum":
			e, _ := val.([]any)
			if len(e) != 1 || e[0] != nil {
				return false
			}
			null = true
		case "description", "title", "$comment", "deprecated":
		default:
			if !strings.HasPrefix(k, "x-") {
				return false
			}
		}
	}
	return null
}

// exclusive30 turns a numeric exclusive bound into the 3.0 pair
// `minimum: x, exclusiveMinimum: true`. With an inclusive bound beside
// it the stricter of the two is kept; both is the same set of values.
// dir is 1 for a lower bound, -1 for an upper one.
func exclusive30(s jmap, exKey, key string, dir float64) {
	ex, ok := s[exKey]
	if !ok {
		return
	}
	exVal, ok := jsonFloat(ex)
	if !ok {
		return // boolean: already 3.0
	}
	if inc, has := s[key]; has {
		if incVal, ok := jsonFloat(inc); ok && (incVal-exVal)*dir > 0 {
			// The inclusive bound is stricter; the other one says
			// nothing more, and is kept for the way back.
			keep(s, xOAS31, exKey)
			return
		}
	}
	s[key] = ex
	s[exKey] = true
}

// contentFormat gives a string with contentEncoding /
// contentMediaType (3.1) the 3.0 format for it: base64 → byte,
// binary content → binary. The keywords themselves move to xOAS31.
func contentFormat(s jmap) {
	enc, hasEnc := s["contentEncoding"].(string)
	cmt, hasCMT := s["contentMediaType"].(string)
	if _, has := s["format"]; has || (!hasEnc && !hasCMT) {
		return
	}
	switch {
	case hasEnc && enc == "base64":
		s["format"] = "byte"
	case !hasEnc && hasCMT && isBinaryMediaType(cmt):
		s["format"] = "binary"
	}
}

// isBinaryMediaType reports whether content of media type t is not
// text: not text/*, JSON or XML.
func isBinaryMediaType(t string) bool {
	base, _, err := mime.ParseMediaType(t)
	if err != nil {
		return false
	}
	if strings.HasPrefix(base, "text/") || base == "application/xml" || strings.HasSuffix(base, "+xml") {
		return false
	}
	return !isJSONMediaType(base)
}

// rewriteRefs points every $ref (and discriminator mapping) into a
// moved $defs entry at its new component.
func (c *downcaster) rewriteRefs(v any) {
	switch t := v.(type) {
	case jmap:
		for k, sub := range t {
			if str, ok := sub.(string); ok && k == "$ref" {
				t[k] = c.newRef(str)
				continue
			}
			if k == "mapping" {
				if m, ok := sub.(jmap); ok {
					for mk, mv := range m {
						if str, ok := mv.(string); ok {
							m[mk] = c.newRef(str)
						}
					}
				}
			}
			c.rewriteRefs(sub)
		}
	case []any:
		for _, sub := range t {
			c.rewriteRefs(sub)
		}
	}
}

func (c *downcaster) newRef(ref string) string {
	for i := 0; i < 16; i++ {
		best := ""
		for old := range c.defs {
			if (ref == old || strings.HasPrefix(ref, old+"/")) && len(old) > len(best) {
				best = old
			}
		}
		if best == "" {
			return ref
		}
		ref = c.defs[best] + ref[len(best):]
	}
	return ref
}

// restoreOAS31 puts the members a 3.0 down-conversion kept in xOAS31
// back on their objects, for the 3.1 up-conversion. A kept member
// replaces the 3.0 one it stood in for (items, security). raw is the JSON
// of a document; the result is raw unchanged when it has none.
func restoreOAS31(raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte(`"`+xOAS31+`"`)) {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case jmap:
			if b, ok := t[xOAS31].(jmap); ok {
				delete(t, xOAS31)
				for k, kv := range b {
					// Maps merge (components.securitySchemes keeps the
					// schemes 3.0 has); anything else is replaced.
					cur, _ := t[k].(jmap)
					if add, ok := kv.(jmap); ok && cur != nil {
						for ak, av := range add {
							cur[ak] = av
						}
						continue
					}
					t[k] = kv
				}
			}
			for _, sub := range t {
				walk(sub)
			}
		case []any:
			for _, sub := range t {
				walk(sub)
			}
		}
	}
	walk(root)
	return json.Marshal(root)
}

func sortedKeys(m jmap) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case jmap:
		out := make(jmap, len(t))
		for k, sub := range t {
			out[k] = deepCopy(sub)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, sub := range t {
			out[i] = deepCopy(sub)
		}
		return out
	}
	return v
}

func jsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

func jsonEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// componentName makes s a valid component key (^[a-zA-Z0-9.\-_]+$).
func componentName(s string) string {
	out := []rune(s)
	for i, r := range out {
		if !(r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			out[i] = '_'
		}
	}
	if len(out) == 0 {
		return "Def"
	}
	return string(out)
}
