package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/graphql-go/graphql"
)

// AddOpenAPIBytes registers an in-memory OpenAPI 3.x spec. Same shape
// as AddOpenAPI but skips the file/HTTP fetch — useful when the
// gateway hosts its own huma-defined routes and self-ingests the
// generated spec at boot.
func (g *Gateway) AddOpenAPIBytes(specBytes []byte, opts ...ServiceOption) error {
	return g.addOpenAPIFromBytes(specBytes, "<inline>", opts...)
}

// AddOpenAPI registers an OpenAPI 3.x specification so its operations
// become GraphQL fields. GET operations land on Query; everything else
// (POST/PUT/PATCH/DELETE) lands on Mutation. Each operation's path,
// query, and body parameters become field arguments; the 200/201
// response schema becomes the field return type.
//
// specSource may be a local file path or an http(s) URL pointing at
// the live spec — huma services typically expose this at
// /openapi.json. The spec is fetched and parsed once at registration;
// changes require a restart (dynamic update is a future follow-up).
//
// Required ServiceOption: gateway.To("http://addr"). Optional As(ns)
// sets the GraphQL namespace prefix; default is the spec's title or
// the URL host.
func (g *Gateway) AddOpenAPI(specSource string, opts ...ServiceOption) error {
	specBytes, err := readOpenAPISpec(specSource)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): %w", specSource, err)
	}
	return g.addOpenAPIFromBytes(specBytes, specSource, opts...)
}

func (g *Gateway) addOpenAPIFromBytes(specBytes []byte, label string, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if sc.conn == nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): missing To(host:port or http url)", label)
	}
	addr, err := openAPIBaseURL(sc.conn)
	if err != nil {
		return err
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): parse: %w", label, err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): validate: %w", label, err)
	}
	ns := sc.namespace
	if ns == "" {
		if doc.Info != nil && doc.Info.Title != "" {
			ns = sanitizeNamespace(doc.Info.Title)
		} else {
			ns = "openapi"
		}
	}
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("gateway: AddOpenAPI: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	if g.openAPISources == nil {
		g.openAPISources = map[string]*openAPISource{}
	}
	if _, exists := g.openAPISources[ns]; exists {
		return fmt.Errorf("gateway: AddOpenAPI: namespace %s already registered", ns)
	}
	g.openAPISources[ns] = &openAPISource{
		namespace: ns,
		baseURL:   addr,
		doc:       doc,
		hash:      sha256.Sum256(specBytes),
	}
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// openAPISource is what AddOpenAPI stores. Hash supports schema-diff /
// services-list parity checks alongside proto-based services.
type openAPISource struct {
	namespace string
	baseURL   string
	doc       *openapi3.T
	hash      [32]byte
}

// readOpenAPISpec fetches a spec from a URL or reads from disk.
func readOpenAPISpec(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := http.Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("status %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return readFile(src)
}

// openAPIBaseURL extracts the base URL from a ServiceOption. Accepts
// strings of the form "http://host:port" or just "host:port" (sugar
// for http://host:port).
func openAPIBaseURL(c any) (string, error) {
	lc, ok := c.(*lazyConn)
	if !ok {
		return "", fmt.Errorf("AddOpenAPI: To(...) must be a host:port or http(s):// URL string")
	}
	addr := lc.addr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/"), nil
}

func sanitizeNamespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ', r == '-':
			b.WriteRune('_')
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "openapi"
	}
	return out
}

// ---------------------------------------------------------------------
// Schema assembly: walk operations, add Query/Mutation fields.
// ---------------------------------------------------------------------

// buildOpenAPIFields walks every registered OpenAPI source and builds
// query and mutation fields. Returns (queries, mutations). Conflicting
// field names within the same root error.
func (g *Gateway) buildOpenAPIFields(tb *openAPITypeBuilder) (graphql.Fields, graphql.Fields, error) {
	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	names := make([]string, 0, len(g.openAPISources))
	for n := range g.openAPISources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, ns := range names {
		src := g.openAPISources[ns]
		if g.internal[ns] {
			continue
		}
		paths := src.doc.Paths
		if paths == nil {
			continue
		}
		pathKeys := make([]string, 0)
		for k := range paths.Map() {
			pathKeys = append(pathKeys, k)
		}
		sort.Strings(pathKeys)
		for _, p := range pathKeys {
			pathItem := paths.Map()[p]
			for _, op := range listOperations(p, pathItem) {
				field, err := g.buildOpenAPIField(tb, src, op)
				if err != nil {
					return nil, nil, err
				}
				name := openAPIFieldName(ns, op)
				switch strings.ToUpper(op.Method) {
				case "GET":
					if _, exists := queries[name]; exists {
						return nil, nil, fmt.Errorf("openapi field name collision (Query): %s", name)
					}
					queries[name] = field
				default:
					if _, exists := mutations[name]; exists {
						return nil, nil, fmt.Errorf("openapi field name collision (Mutation): %s", name)
					}
					mutations[name] = field
				}
			}
		}
	}
	return queries, mutations, nil
}

type openAPIOperation struct {
	Method   string
	Path     string
	Op       *openapi3.Operation
	PathItem *openapi3.PathItem
}

func listOperations(path string, item *openapi3.PathItem) []openAPIOperation {
	out := []openAPIOperation{}
	verbs := []struct {
		verb string
		op   *openapi3.Operation
	}{
		{"GET", item.Get},
		{"POST", item.Post},
		{"PUT", item.Put},
		{"PATCH", item.Patch},
		{"DELETE", item.Delete},
	}
	for _, v := range verbs {
		if v.op == nil {
			continue
		}
		out = append(out, openAPIOperation{Method: v.verb, Path: path, Op: v.op, PathItem: item})
	}
	return out
}

// openAPIFieldName builds a GraphQL field name from an operation.
// Prefers operationId (huma sets these); falls back to a method+path
// slug. Always prefixed with the namespace so cross-spec collisions
// don't trample.
func openAPIFieldName(ns string, op openAPIOperation) string {
	id := op.Op.OperationID
	if id == "" {
		id = strings.ToLower(op.Method) + pathToSlug(op.Path)
	}
	return ns + "_" + lowerCamel(sanitizeNamespace(id))
}

func pathToSlug(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r == '/' || r == '{' || r == '}':
			b.WriteRune('_')
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "_")
}

func (g *Gateway) buildOpenAPIField(tb *openAPITypeBuilder, src *openAPISource, op openAPIOperation) (*graphql.Field, error) {
	args := graphql.FieldConfigArgument{}

	// Path + query parameters → args.
	for _, paramRef := range op.Op.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		if p.In != "path" && p.In != "query" {
			continue // skip header/cookie for v1
		}
		t, err := tb.inputTypeFromSchema(p.Schema)
		if err != nil {
			return nil, err
		}
		if p.Required {
			t = graphql.NewNonNull(t)
		}
		args[p.Name] = &graphql.ArgumentConfig{Type: t}
	}

	// Request body → 'body' arg (input object).
	if op.Op.RequestBody != nil && op.Op.RequestBody.Value != nil {
		body := op.Op.RequestBody.Value
		if mt, ok := body.Content["application/json"]; ok && mt.Schema != nil {
			t, err := tb.inputTypeFromSchema(mt.Schema)
			if err != nil {
				return nil, err
			}
			if body.Required {
				t = graphql.NewNonNull(t)
			}
			args["body"] = &graphql.ArgumentConfig{Type: t}
		}
	}

	// Response: prefer 200, then 201, then 'default'.
	out, err := tb.responseType(op.Op)
	if err != nil {
		return nil, err
	}
	method := op.Method
	pathTemplate := op.Path
	baseURL := src.baseURL

	return &graphql.Field{
		Type: out,
		Args: args,
		Resolve: func(rp graphql.ResolveParams) (any, error) {
			return dispatchOpenAPI(rp.Context, method, baseURL, pathTemplate, op.Op, rp.Args)
		},
	}, nil
}

// dispatchOpenAPI substitutes path params, encodes query + body, sends
// the HTTP request, and decodes the JSON response.
func dispatchOpenAPI(
	ctx context.Context,
	method, baseURL, pathTemplate string,
	op *openapi3.Operation,
	gqlArgs map[string]any,
) (any, error) {
	resolvedPath := pathTemplate
	queryArgs := url.Values{}
	for _, paramRef := range op.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		v, ok := gqlArgs[p.Name]
		if !ok {
			continue
		}
		strVal := fmt.Sprintf("%v", v)
		switch p.In {
		case "path":
			resolvedPath = strings.ReplaceAll(resolvedPath, "{"+p.Name+"}", url.PathEscape(strVal))
		case "query":
			queryArgs.Add(p.Name, strVal)
		}
	}

	full := baseURL + resolvedPath
	if len(queryArgs) > 0 {
		full += "?" + queryArgs.Encode()
	}

	var body io.Reader
	if bv, ok := gqlArgs["body"]; ok && bv != nil {
		b, err := json.Marshal(bv)
		if err != nil {
			return nil, fmt.Errorf("openapi: marshal body: %w", err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	forwardOpenAPIHeaders(ctx, req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openapi: %s %s: %s: %s", method, full, resp.Status, strings.TrimSpace(string(respBytes)))
	}
	if len(respBytes) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("openapi: decode response: %w", err)
	}
	return out, nil
}

// readFile is os.ReadFile in a function var so tests can swap it.
var readFile = os.ReadFile

// forwardedOpenAPIHeaders is the static set forwarded from the inbound
// GraphQL request onto outbound OpenAPI dispatches. v1 is auth-only —
// enough to make admin_* mutations work end-to-end with one bearer
// token. Tier-2 follow-up: per-source configurable list (mTLS,
// service-account tokens, etc. — see docs/plan.md).
var forwardedOpenAPIHeaders = []string{"Authorization"}

func forwardOpenAPIHeaders(ctx context.Context, out *http.Request) {
	in := HTTPRequestFromContext(ctx)
	if in == nil {
		return
	}
	for _, h := range forwardedOpenAPIHeaders {
		if v := in.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
}

// ---------------------------------------------------------------------
// Type mapper: JSON Schema → GraphQL.
// ---------------------------------------------------------------------

type openAPITypeBuilder struct {
	mu       sync.Mutex
	objects  map[string]*graphql.Object
	inputs   map[string]*graphql.InputObject
	enums    map[string]*graphql.Enum
	jsonScalar *graphql.Scalar
}

func newOpenAPITypeBuilder() *openAPITypeBuilder {
	return &openAPITypeBuilder{
		objects: map[string]*graphql.Object{},
		inputs:  map[string]*graphql.InputObject{},
		enums:   map[string]*graphql.Enum{},
		jsonScalar: graphql.NewScalar(graphql.ScalarConfig{
			Name:        "JSON",
			Description: "Untyped JSON value (used as a fallback for OpenAPI schemas the gateway can't map exactly).",
			Serialize:   func(v any) any { return v },
			ParseValue:  func(v any) any { return v },
		}),
	}
}

// primaryType strips "null" from an OpenAPI 3.1 multi-type
// declaration, returning the single non-null type. Returns "" if the
// schema has zero or multiple non-null types (we treat those as
// opaque JSON).
func primaryType(s *openapi3.Schema) string {
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

// outputTypeFromSchema returns a GraphQL output Type for the given
// OpenAPI schema. Unsupported shapes (oneOf/anyOf/allOf, mixed types)
// fall back to the JSON scalar.
func (tb *openAPITypeBuilder) outputTypeFromSchema(ref *openapi3.SchemaRef) (graphql.Output, error) {
	if ref == nil || ref.Value == nil {
		return tb.jsonScalar, nil
	}
	s := ref.Value
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return tb.jsonScalar, nil
	}
	if pt := primaryType(s); pt != "" {
		switch pt {
		case "string":
			if len(s.Enum) > 0 {
				return tb.enumFor(ref, s)
			}
			return graphql.String, nil
		case "integer":
			return graphql.Int, nil
		case "number":
			return graphql.Float, nil
		case "boolean":
			return graphql.Boolean, nil
		case "array":
			elem, err := tb.outputTypeFromSchema(s.Items)
			if err != nil {
				return nil, err
			}
			return graphql.NewList(elem), nil
		case "object":
			return tb.objectFor(ref, s)
		}
	}
	return tb.jsonScalar, nil
}

func (tb *openAPITypeBuilder) inputTypeFromSchema(ref *openapi3.SchemaRef) (graphql.Input, error) {
	if ref == nil || ref.Value == nil {
		return tb.jsonScalar, nil
	}
	s := ref.Value
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return tb.jsonScalar, nil
	}
	if pt := primaryType(s); pt != "" {
		switch pt {
		case "string":
			if len(s.Enum) > 0 {
				e, err := tb.enumFor(ref, s)
				if err != nil {
					return nil, err
				}
				return e, nil
			}
			return graphql.String, nil
		case "integer":
			return graphql.Int, nil
		case "number":
			return graphql.Float, nil
		case "boolean":
			return graphql.Boolean, nil
		case "array":
			elem, err := tb.inputTypeFromSchema(s.Items)
			if err != nil {
				return nil, err
			}
			return graphql.NewList(elem), nil
		case "object":
			return tb.inputObjectFor(ref, s)
		}
	}
	return tb.jsonScalar, nil
}

func (tb *openAPITypeBuilder) responseType(op *openapi3.Operation) (graphql.Output, error) {
	if op.Responses == nil {
		return tb.jsonScalar, nil
	}
	for _, code := range []string{"200", "201"} {
		r := op.Responses.Status(parseStatus(code))
		if r != nil && r.Value != nil {
			if mt, ok := r.Value.Content["application/json"]; ok && mt.Schema != nil {
				return tb.outputTypeFromSchema(mt.Schema)
			}
		}
	}
	if r := op.Responses.Default(); r != nil && r.Value != nil {
		if mt, ok := r.Value.Content["application/json"]; ok && mt.Schema != nil {
			return tb.outputTypeFromSchema(mt.Schema)
		}
	}
	return tb.jsonScalar, nil
}

func parseStatus(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (tb *openAPITypeBuilder) objectFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (graphql.Output, error) {
	name := schemaName(ref, s, "Object")
	if obj, ok := tb.objects[name]; ok {
		return obj, nil
	}
	// Pre-register an empty Object to handle recursive refs.
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name: name,
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			fields := graphql.Fields{}
			propNames := make([]string, 0, len(s.Properties))
			for k := range s.Properties {
				propNames = append(propNames, k)
			}
			sort.Strings(propNames)
			for _, k := range propNames {
				if !validGraphQLName(k) {
					continue // e.g. $schema (JSON Schema metaschema)
				}
				p := s.Properties[k]
				t, err := tb.outputTypeFromSchema(p)
				if err != nil {
					continue
				}
				if isRequired(s.Required, k) {
					t = graphql.NewNonNull(t)
				}
				fields[lowerCamel(k)] = &graphql.Field{Type: t}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.Field{Type: graphql.String}
			}
			return fields
		}),
	})
	tb.objects[name] = obj
	return obj, nil
}

func (tb *openAPITypeBuilder) inputObjectFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (graphql.Input, error) {
	name := schemaName(ref, s, "Input") + "Input"
	if io, ok := tb.inputs[name]; ok {
		return io, nil
	}
	io := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
			fields := graphql.InputObjectConfigFieldMap{}
			propNames := make([]string, 0, len(s.Properties))
			for k := range s.Properties {
				propNames = append(propNames, k)
			}
			sort.Strings(propNames)
			for _, k := range propNames {
				if !validGraphQLName(k) {
					continue
				}
				p := s.Properties[k]
				t, err := tb.inputTypeFromSchema(p)
				if err != nil {
					continue
				}
				if isRequired(s.Required, k) {
					t = graphql.NewNonNull(t)
				}
				fields[lowerCamel(k)] = &graphql.InputObjectFieldConfig{Type: t}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.InputObjectFieldConfig{Type: graphql.String}
			}
			return fields
		}),
	})
	tb.inputs[name] = io
	return io, nil
}

// validGraphQLName matches /^[_A-Za-z][_A-Za-z0-9]*$/. JSON Schema
// allows things like "$schema" that GraphQL forbids; skip those at
// type-build time.
func validGraphQLName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func (tb *openAPITypeBuilder) enumFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (*graphql.Enum, error) {
	name := schemaName(ref, s, "Enum")
	if e, ok := tb.enums[name]; ok {
		return e, nil
	}
	values := graphql.EnumValueConfigMap{}
	for _, v := range s.Enum {
		vs := fmt.Sprintf("%v", v)
		values[sanitizeNamespace(vs)] = &graphql.EnumValueConfig{Value: vs}
	}
	e := graphql.NewEnum(graphql.EnumConfig{Name: name, Values: values})
	tb.enums[name] = e
	return e, nil
}

func schemaName(ref *openapi3.SchemaRef, s *openapi3.Schema, fallback string) string {
	if ref != nil && ref.Ref != "" {
		// "#/components/schemas/Pet" → "Pet"
		parts := strings.Split(ref.Ref, "/")
		return sanitizeNamespace(parts[len(parts)-1])
	}
	if s != nil && s.Title != "" {
		return sanitizeNamespace(s.Title)
	}
	return fallback
}

func isRequired(req []string, name string) bool {
	for _, r := range req {
		if r == name {
			return true
		}
	}
	return false
}
