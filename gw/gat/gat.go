// Package gat (GraphQL API Translator) is gwag's embedded sibling: a
// single-server, in-process translator that turns OpenAPI / proto /
// GraphQL specs into a GraphQL surface mountable on the adopter's
// existing huma router. No NATS, no cluster, no admin endpoints, no
// MCP — just spec-to-GraphQL with HTTP dispatch.
package gat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/IodeSystems/graphql-go/v2"
	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// Gateway is the embedded GraphQL translator. Given service specs
// (OpenAPI, proto, or custom dispatchers), it produces a GraphQL
// schema and serves queries.
//
// Two registration paths feed it:
//
//   - BYO IR: gat.New(ServiceRegistration{...}) takes pre-ingested
//     ir.Service values plus dispatchers (HTTP loopback or custom).
//     Schema is built eagerly.
//
//   - Paired huma: gat.New() returns an empty gateway. Each operation
//     is added with gat.Register (a drop-in for huma.Register that
//     also captures the handler ref for in-process dispatch). When the
//     adopter calls gat.RegisterHuma(api, g, prefix), gat reads
//     api.OpenAPI(), ingests it into IR, wires the captured handlers
//     as dispatchers, builds the schema, and mounts the GraphQL +
//     schema-view endpoints onto the adopter's huma router.
//
// Stability: stable
type Gateway struct {
	schema      *graphql.Schema
	annotations *ir.AnnotationIndex
	registry    *ir.DispatchRegistry
	services    []*ir.Service

	// planCache holds parsed + validated + planned query state so a
	// repeated query skips straight to execution. Built by build();
	// nil only on a gateway whose schema never assembled, which the
	// handler treats as "not ready".
	planCache *graphql.PlanCache

	// captured holds ops registered via gat.Register for paired huma
	// dispatch. Empty when only the BYO-IR path is used.
	captured []*capturedOp

	// subs holds streaming ops from RegisterSSE and Subscribe. subAPI
	// is the side huma API Subscribe's synthetic ops are described on
	// (nil until the first Subscribe).
	subs   []*streamOp
	subAPI huma.API

	// built is true once the schema has been assembled (either by
	// New(regs...) or by RegisterHuma). Subsequent registrations are
	// rejected.
	built bool

	// longAsNumber opts the Long scalar into honest number|string output —
	// a JSON number when JS-safe (|v| <= 2^53-1), a decimal string when
	// larger. Default false = legacy always-string. Set via LongAsNumber()
	// before the schema builds (i.e. before New(regs...) returns or before
	// RegisterHuma on the empty-gateway path). Input stays lenient either way.
	longAsNumber bool

	// emptyNilSlices (default true) makes required output list fields render
	// non-null (`[T!]!`) and coerces nil Go slices to `[]` at dispatch, so an
	// empty/absent slice serializes as `[]` rather than null — clients never
	// see a null list (better DX). Set EmptyNilSlices(false) for the legacy
	// behavior (nullable lists, nil → JSON null).
	emptyNilSlices bool

	// allowEmbeddedParams downgrades the embedded-parameter check from a
	// mount-time error to a log warning. Off by default: the condition
	// silently drops request parameters, so failing loudly is the safer
	// default.
	allowEmbeddedParams bool

	// observe, when set (ObserveDispatch), is invoked after every in-process
	// operation dispatch for per-operation metrics. nil = no observation.
	observe DispatchObserver
	// trusted holds the persisted documents (TrustedDocuments); nil =
	// queries only.
	trusted *trustedDocs
	// exports caches the @export paths of batched documents.
	exports exportCache
	// hook, when set (HookDispatch), runs around every in-process
	// operation call with its context. nil = off.
	hook DispatchHook

	// parallel caps how many query operations of one GraphQL request run
	// concurrently (ParallelQueries). 0 = serial, the default. maxOps caps
	// operations per request (MaxOperations); 0 = no cap. maxBody caps
	// the GraphQL request body (MaxRequestBytes).
	parallel int
	maxOps   int
	maxBody  int64

	// maxDepth (MaxDepth) and noIntrospection (DisableIntrospection)
	// become plan-cache validation rules: checked once per distinct
	// query.
	maxDepth        int
	noIntrospection bool

	// pubsub is gat's in-process publish/subscribe primitive, always
	// available via PubSub(). mesh is the optional best-effort
	// cross-node fanout layer — nil until EnablePeerMesh is called.
	// subAuth is the HMAC secret gating the WebSocket subscribe
	// endpoint — nil until EnableSubscribeAuth is called (endpoint
	// open).
	pubsub  *pubSub
	mesh    *peerMesh
	subAuth []byte
}

// ServiceRegistration pairs an IR service with its dispatch config.
//
// Stability: stable
type ServiceRegistration struct {
	Service     *ir.Service
	BaseURL     string                        // upstream HTTP base URL for OpenAPI dispatch
	Dispatchers map[ir.SchemaID]ir.Dispatcher // custom dispatchers keyed by SchemaID
}

// New constructs a gat gateway. Two usage patterns:
//
// Paired huma (recommended):
//
//	g := gat.New()
//	gat.Register(api, g, huma.Operation{...}, listProjectsHandler)
//	gat.Register(api, g, huma.Operation{...}, getProjectHandler)
//	gat.RegisterHuma(api, g, "/api")  // mounts /api/graphql + /api/schema/*
//
// BYO IR (advanced; bring pre-ingested services + dispatchers):
//
//	doc, _ := loader.LoadFromData(specBytes)
//	svc := ir.IngestOpenAPI(doc)
//	svc.Namespace = "pets"
//	svc.Version = "v1"
//	ir.PopulateSchemaIDs(svc)
//	gw, _ := gat.New(gat.ServiceRegistration{
//	    Service: svc,
//	    BaseURL: "http://localhost:8081",
//	})
//	http.Handle("/graphql", gw.Handler())
//
// Stability: stable
func New(regs ...ServiceRegistration) (*Gateway, error) {
	g := &Gateway{
		registry:       ir.NewDispatchRegistry(),
		pubsub:         newPubSub(),
		emptyNilSlices: true,
		maxBody:        defaultMaxRequestBytes,
		maxOps:         defaultMaxOperations,
	}
	if len(regs) == 0 {
		// Empty gateway — adopter will populate via gat.Register and
		// finalize via gat.RegisterHuma.
		return g, nil
	}
	if err := g.addRegistrations(regs); err != nil {
		return nil, err
	}
	if err := g.build(); err != nil {
		return nil, err
	}
	return g, nil
}

// LongAsNumber opts this gateway's Long scalar into honest number|string output —
// a JSON number when JS-safe (|v| <= 2^53-1), a decimal string when larger. Call it
// before the schema builds (empty New() → LongAsNumber(true) → Register… → RegisterHuma).
// Returns g for chaining; a no-op once built. Input is lenient (number or string) either way.
func (g *Gateway) LongAsNumber(b bool) *Gateway {
	g.longAsNumber = b
	return g
}

// EmptyNilSlices toggles the empty-list contract (default on). When on, required
// output list fields render non-null (`[T!]!`) and the dispatch coerces nil Go
// slices to `[]`, so clients never receive a null list. Off restores the legacy
// behavior (nullable lists; a nil slice serializes to JSON null). Call before the
// schema builds. Returns g for chaining; a no-op once built.
func (g *Gateway) EmptyNilSlices(b bool) *Gateway {
	g.emptyNilSlices = b
	return g
}

// DispatchObserver is called after every in-process operation call — GraphQL field or gRPC
// unary call — with the operation id, how long it took (huma middlewares, input checks and
// handler), and its error (nil on success). Keep it cheap; it runs on the hot path, and
// concurrently under ParallelQueries.
type DispatchObserver func(operationID string, dur time.Duration, err error)

// MaxRequestBytes caps the GraphQL request body (default 1 MiB, huma's
// REST default; n <= 0: no cap). A larger body is refused with 413
// before it is decoded. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) MaxRequestBytes(n int64) *Gateway {
	g.maxBody = n
	return g
}

// ObserveDispatch sets the per-operation dispatch observer (see DispatchObserver). Call before
// RegisterHuma. Returns g for chaining. nil disables observation. Stability: experimental.
func (g *Gateway) ObserveDispatch(fn DispatchObserver) *Gateway {
	g.observe = fn
	return g
}

// DispatchHook runs before every in-process operation call — GraphQL field or gRPC unary
// call — with the request context and operation id. The context it returns is the one the
// huma middlewares and handler see, so a hook can start a trace span or tag a logger;
// done, if non-nil, is called with the call's error when it returns. gat takes no tracing
// dependency: wire OpenTelemetry (or anything else) here.
type DispatchHook func(ctx context.Context, operationID string) (_ context.Context, done func(error))

// HookDispatch sets the per-operation dispatch hook (see DispatchHook). Call before
// RegisterHuma. Returns g for chaining. nil disables it. Stability: experimental.
func (g *Gateway) HookDispatch(fn DispatchHook) *Gateway {
	g.hook = fn
	return g
}

// instrument wraps c.invoke with the observer and hook, once. Unset,
// it leaves invoke alone, so they cost nothing when off.
func (g *Gateway) instrument(c *capturedOp) {
	if c.instrumented || (g.observe == nil && g.hook == nil) {
		return
	}
	c.instrumented = true
	inner, id, observe, hook := c.invoke, c.op.OperationID, g.observe, g.hook
	c.invoke = func(ctx context.Context, in any, args map[string]any) (any, error) {
		var done func(error)
		if hook != nil {
			ctx, done = hook(ctx, id)
		}
		var start time.Time
		if observe != nil {
			start = time.Now()
		}
		out, err := inner(ctx, in, args)
		if observe != nil {
			observe(id, time.Since(start), err)
		}
		if done != nil {
			done(err)
		}
		return out, err
	}
}

// addRegistrations attaches BYO-IR services and wires their
// dispatchers onto g. Caller is responsible for invoking g.build()
// once all registrations are in.
func (g *Gateway) addRegistrations(regs []ServiceRegistration) error {
	for _, reg := range regs {
		svc := reg.Service
		if svc.Namespace == "" {
			svc.Namespace = "openapi"
		}
		if svc.Version == "" {
			svc.Version = "v1"
		}
		g.services = append(g.services, svc)

		for _, op := range svc.FlatOperations() {
			if d, ok := reg.Dispatchers[op.SchemaID]; ok {
				g.registry.Set(op.SchemaID, d)
				continue
			}
			if reg.BaseURL != "" && svc.OriginKind == ir.KindOpenAPI {
				g.registry.Set(op.SchemaID, newOpenAPIDispatcher(reg.BaseURL, svc, op))
			}
		}
	}
	return nil
}

// build assembles the GraphQL schema from g.services and g.registry.
// Idempotent: callers may invoke it multiple times if more services
// are added between rounds (RegisterHuma does this when paired
// captured ops are present).
func (g *Gateway) build() error {
	g.wrapLimits()
	longScalar, jsonScalar := ir.StandardScalarsWith(g.longAsNumber)
	annotations := ir.NewAnnotationIndex()
	schema, err := ir.RenderGraphQLRuntime(g.services, g.registry, ir.RuntimeOptions{
		LongType:             longScalar,
		JSONType:             jsonScalar,
		AnnotationSink:       annotations,
		NonNullRequiredLists: g.emptyNilSlices,
		Directives:           []*graphql.Directive{exportDirective},
	})
	if err != nil {
		return fmt.Errorf("gat: build schema: %w", err)
	}
	g.schema = schema
	g.annotations = annotations
	// The cache is keyed on (schema, query, operationName), so it is
	// safe to build once here — the schema is immutable after build.
	// Defaults mirror gw/'s: gat has no knobs to expose them through.
	g.planCache = graphql.NewPlanCache(graphql.PlanCacheOptions{
		MaxEntries: planCacheEntries,
		Rules:      g.validationRules(),
	})
	g.built = true
	return nil
}

// Handler returns an http.Handler that serves GraphQL queries and
// mutations (POST /graphql with JSON body; GET with ?query= param) and
// subscriptions registered with Subscribe (a WebSocket upgrade on the
// same path, graphql-transport-ws subprotocol).
//
// Stability: stable
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "upgrade not supported", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && isWebSocketUpgrade(r) {
			g.serveGraphQLWS(w, r)
			return
		}

		var req gqlRequest
		if r.Method == http.MethodPost {
			body := r.Body
			if g.maxBody > 0 {
				// A declared length is checked up front for free; only a
				// chunked body pays for the limiting reader.
				if r.ContentLength > g.maxBody {
					writeError(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				if r.ContentLength < 0 {
					body = http.MaxBytesReader(w, r.Body, g.maxBody)
				}
			}
			// Peek at the first byte to tell a batch (array) from a single
			// request (object) and decode straight into the right type.
			br := peekReaderPool.Get().(*bufio.Reader)
			br.Reset(body)
			isBatch := firstNonSpace(br) == '['
			var (
				batch []gqlRequest
				err   error
			)
			if isBatch {
				// Its own function: &batch here would put the slice
				// header on the heap for every request, batch or not.
				batch, err = decodeBatch(br)
			} else {
				err = json.NewDecoder(br).Decode(&req)
			}
			br.Reset(nil)
			peekReaderPool.Put(br)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeError(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				writeError(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if isBatch {
				g.serveBatch(w, r, batch)
				return
			}
		} else {
			params := r.URL.Query()
			req.Query = params.Get("query")
			req.OperationName = params.Get("operationName")
			req.DocumentID = params.Get("documentId")
			// GET carries variables and extensions as JSON in the query
			// string (GraphQL over HTTP); persisted documents over GET —
			// the CDN-cacheable form — depend on it.
			var err error
			if req.Variables, err = jsonParam(params, "variables"); err != nil {
				writeError(w, "invalid variables JSON", http.StatusBadRequest)
				return
			}
			if req.Extensions, err = jsonParam(params, "extensions"); err != nil {
				writeError(w, "invalid extensions JSON", http.StatusBadRequest)
				return
			}
		}

		p := g.planRequest(&req)
		if p.status != 0 {
			writeCodedError(w, p.msg, p.code, p.status)
			return
		}

		// GraphQL over HTTP forbids mutations on GET. Running one would
		// let any page trigger it with the visitor's cookies (a link or an
		// <img src>; SameSite=Lax still sends cookies on top-level GETs).
		if r.Method == http.MethodGet && p.pr.Plan != nil && p.pr.Plan.IsMutation() {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, "mutations are not allowed over GET; use POST", http.StatusMethodNotAllowed)
			return
		}

		ctx, scope := newScope(r.Context(), r, true)

		// Status is 200 whatever the query did, matching gw/ and the
		// GraphQL-over-HTTP spec: a query that resolved to errors is a
		// successful transport exchange carrying an errors envelope, not a
		// failed request. The 400s above are transport-level (the body
		// wasn't JSON, or carried no query at all) and stay 400.
		// Build the ENTIRE body before touching the response header. A
		// handler's Set-Cookie only reaches the sink once its resolver has
		// run (dispatch feeds the sink), so emitting before execution
		// writes an EMPTY sink and the cookie is dropped — silently, since
		// the op itself still succeeds. That is how login/logout stopped
		// setting a session over GraphQL while the REST twin of the same
		// handler kept working.
		//
		// Nothing here streams — ExecutePlanAppend assembles the bytes in
		// memory — so deferring the header costs nothing.
		res := g.execute(ctx, p, req.OperationName, req.Variables)

		// Execution is done, so the sink now holds whatever the resolvers
		// produced. Emit before WriteHeader — after it the header map is
		// already on the wire.
		scope.cookies.emit(w)
		etag := scope.cache.apply(w.Header(), p.query, req.Variables)
		if r.Method == http.MethodGet && etagMatches(r.Header.Get("If-None-Match"), etag) {
			// The client holds this response already. The work ran (the
			// ETag comes from the operations), but the body stays home.
			w.WriteHeader(http.StatusNotModified)
			res.recycle()
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.payload)
		res.recycle()
	})
}

// gqlRequest is one GraphQL request: an operation as query text, or by
// trusted document id.
type gqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
	DocumentID    string         `json:"documentId"`
	Extensions    map[string]any `json:"extensions"`
}

// peekReaderPool holds the readers the handler peeks a POST body
// through (an array is a batch, export.go); pooled so telling the two
// apart costs no allocation.
var peekReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 512) }}

func decodeBatch(r io.Reader) ([]gqlRequest, error) {
	var batch []gqlRequest
	err := json.NewDecoder(r).Decode(&batch)
	return batch, err
}

// firstNonSpace returns the body's first non-whitespace byte without
// consuming it (0 when there is none).
func firstNonSpace(br *bufio.Reader) byte {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return 0
		}
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			_, _ = br.Discard(1)
		default:
			return b[0]
		}
	}
}

// plannedRequest is a request ready to execute, or the reason it was
// refused before execution (status != 0).
type plannedRequest struct {
	pr    graphql.PlanResult
	query string // the document text, resolved from a trusted id if one was sent

	status    int
	msg, code string
}

// planRequest resolves a request's document (query text or trusted
// document id) and plans it.
func (g *Gateway) planRequest(req *gqlRequest) plannedRequest {
	p := plannedRequest{query: req.Query}
	id := persistedID(req.DocumentID, req.Extensions)
	switch {
	case id != "" && g.trusted == nil:
		p.status, p.msg, p.code = http.StatusBadRequest, "persisted documents are not enabled", "PERSISTED_QUERY_NOT_SUPPORTED"
	case id != "":
		key, doc, ok := g.trusted.lookup(id)
		switch {
		case ok:
			p.query = doc
			p.pr = g.trusted.plan(g, key, doc, req.OperationName)
		case req.Query != "" && !g.trusted.enforce:
			// APQ sends the hash with the query on retry; without
			// enforcement the query alone is enough.
			p.pr = g.planCache.Get(g.schema, req.Query, req.OperationName)
		default:
			p.status, p.msg, p.code = http.StatusOK, "PersistedQueryNotFound", "PERSISTED_QUERY_NOT_FOUND"
		}
	case req.Query == "":
		p.status, p.msg, p.code = http.StatusBadRequest, "missing query", "BAD_REQUEST"
	case g.trusted != nil && g.trusted.enforce:
		p.status, p.msg, p.code = http.StatusBadRequest, "only trusted documents are accepted", "TRUSTED_DOCUMENT_REQUIRED"
	default:
		p.pr = g.planCache.Get(g.schema, req.Query, req.OperationName)
	}
	return p
}

// execResult is an executed request's response JSON. The payload may
// live in a pooled buffer: call recycle once it has been written or
// copied. A struct, not a returned closure, so it stays off the heap.
type execResult struct {
	payload []byte
	buf     *[]byte // pooled buffer the payload may alias; nil when not pooled
	body    []byte  // the (possibly grown) backing array to return to it
}

func (r execResult) recycle() {
	// Keep a one-off large response from pinning a fat allocation for
	// the process lifetime.
	if r.buf != nil && cap(r.body) <= graphqlBufPoolMax {
		*r.buf = r.body[:0]
		graphqlBufPool.Put(r.buf)
	}
}

// execute runs a planned request and returns its response JSON.
func (g *Gateway) execute(ctx context.Context, p plannedRequest, operationName string, variables map[string]any) (res execResult) {
	pr := p.pr
	switch {
	case len(pr.Errors) > 0:
		// Parse / validate failed, so there is no plan to run. Rare
		// path with a small payload — the encoder is fine here.
		res.payload, _ = json.Marshal(&graphql.Result{Errors: pr.Errors})
	case pr.Plan != nil:
		args := variables
		if len(pr.SynthArgs) > 0 {
			merged := make(map[string]any, len(args)+len(pr.SynthArgs))
			for k, v := range args {
				merged[k] = v
			}
			for k, v := range pr.SynthArgs {
				merged[k] = v
			}
			args = merged
		}
		// Hot path: walk the cached plan and emit response JSON
		// straight into a pooled buffer, skipping the
		// map[string]any result tree and the final Encode.
		buf := graphqlBufPool.Get().(*[]byte)
		body, errs := graphql.ExecutePlanAppend(pr.Plan, graphql.ExecuteParams{
			Schema:        *g.schema,
			OperationName: operationName,
			Args:          args,
			Context:       ctx,
		}, (*buf)[:0])
		if len(errs) > 0 {
			// Spec-level failures (variable coercion and friends) that
			// happened before any data was assembled. Emit a clean
			// errors envelope rather than the partial bytes.
			res.payload, _ = json.Marshal(&graphql.Result{Errors: errs})
		} else {
			// Field-level errors are already inside body's `errors`
			// array — ExecutePlanAppend writes them there.
			res.payload = body
		}
		// Recycled by the caller after the write: payload may alias body.
		res.buf, res.body = buf, body
	default:
		// No plan and no errors shouldn't happen, but fall back to
		// the full parse-and-execute path rather than serving an
		// empty body.
		res.payload, _ = json.Marshal(graphql.Do(graphql.Params{
			Schema:         *g.schema,
			RequestString:  p.query,
			VariableValues: variables,
			OperationName:  operationName,
			Context:        ctx,
		}))
	}
	return res
}

// serveBatch runs a batched request: the operations in order, sharing
// one request scope (so MaxOperations, cookies and the cache-header
// merge span the batch), each @export feeding the operations after it.
// The response is the array of their results; an operation refused
// before execution (unknown document id, query text under enforced
// trusted documents) gets an errors object in its slot without failing
// the others.
func (g *Gateway) serveBatch(w http.ResponseWriter, r *http.Request, batch []gqlRequest) {
	if len(batch) == 0 {
		writeError(w, "empty batch", http.StatusBadRequest)
		return
	}
	if g.maxOps > 0 && len(batch) > g.maxOps {
		writeError(w, fmt.Sprintf("batch exceeds %d operations", g.maxOps), http.StatusBadRequest)
		return
	}
	ctx, scope := newScope(r.Context(), r, true)
	exported := map[string]any{}
	docs := make([]string, 0, len(batch))
	out := append(make([]byte, 0, 1024), '[')
	for i := range batch {
		if i > 0 {
			out = append(out, ',')
		}
		req := &batch[i]
		p := g.planRequest(req)
		if p.status != 0 {
			out = appendCodedError(out, p.msg, p.code)
			continue
		}
		docs = append(docs, p.query)
		vars := req.Variables
		if len(exported) > 0 {
			// Exports fill the variables the client left unset.
			merged := make(map[string]any, len(vars)+len(exported))
			for k, v := range exported {
				merged[k] = v
			}
			for k, v := range vars {
				merged[k] = v
			}
			vars = merged
		}
		res := g.execute(ctx, p, req.OperationName, vars)
		out = append(out, res.payload...)
		if paths := g.exports.exportsFor(p.query, req.OperationName); paths != nil {
			collectExports(paths, res.payload, exported)
		}
		res.recycle()
	}
	out = append(out, ']')
	scope.cookies.emit(w)
	scope.cache.apply(w.Header(), strings.Join(docs, "\x00"), nil)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// appendCodedError appends a GraphQL errors object carrying
// extensions.code.
func appendCodedError(dst []byte, msg, code string) []byte {
	b, _ := json.Marshal(map[string]any{
		"errors": []map[string]any{{"message": msg, "extensions": map[string]any{"code": code}}},
	})
	return append(dst, b...)
}

// Schema returns the assembled graphql.Schema for introspection.
//
// Stability: experimental
func (g *Gateway) Schema() *graphql.Schema {
	return g.schema
}

// GraphQLSDL renders the gateway's schema as SDL WITH its annotation index (@derived,
// @deprecated, etc.). Prefer this over ir.PrintSchemaSDL(g.Schema()), which omits the
// annotations — the SDL/graphql HTTP view uses this same path.
func (g *Gateway) GraphQLSDL() string {
	return ir.PrintSchemaSDL(g.schema, g.annotations)
}

// Services returns the registered IR services.
//
// Stability: experimental
func (g *Gateway) Services() []*ir.Service {
	return g.services
}

// PubSub returns the gateway's publish/subscribe handle. It is always
// available — single-node (in-process) out of the box. Call
// EnablePeerMesh to add best-effort cross-node fanout to a set of
// peer gat instances.
//
// Stability: experimental
func (g *Gateway) PubSub() *PubSub {
	return &PubSub{g: g}
}

// Close releases gateway resources: it stops the peer-mesh fanout
// goroutines (if EnablePeerMesh was called) and cancels every active
// pubsub subscription. A gateway is single-use after Close.
//
// Stability: experimental
func (g *Gateway) Close() {
	if g.mesh != nil {
		g.mesh.stop()
	}
	g.pubsub.closeAll()
}

// PubSub is the publish/subscribe handle returned by Gateway.PubSub.
// Publish delivers to local subscribers and, when a peer mesh is
// configured, fans out best-effort to peer gat instances; Subscribe
// is always local-only.
//
// Stability: experimental
type PubSub struct {
	g *Gateway
}

// Publish delivers payload to every local subscriber whose pattern
// matches channel.
//
// Stability: experimental
func (p *PubSub) Publish(channel string, payload []byte) {
	p.g.publish(channel, payload)
}

// Subscribe registers a subscriber for channels matching pattern
// (NATS-style: `*` one segment, `>` the rest). Returns an event
// channel and a cancel func. Subscriptions are local to this gateway
// — peers deliver into it via the mesh receive path, not by
// subscribing remotely.
//
// Stability: experimental
func (p *PubSub) Subscribe(pattern string) (<-chan Event, func()) {
	return p.g.pubsub.Subscribe(pattern)
}

// publish is the gateway-internal publish entry point behind the
// PubSub facade: local fanout, plus best-effort cross-node fanout
// when a peer mesh is configured.
func (g *Gateway) publish(channel string, payload []byte) {
	g.pubsub.publishLocal(channel, payload)
	if g.mesh != nil {
		g.mesh.fanout(channel, payload)
	}
}

// defaultMaxOperations bounds the operations one GraphQL request may
// run: enough for any page a client fuses, low enough that a body of
// a thousand aliases cannot turn into a thousand handler calls.
const defaultMaxOperations = 64

// defaultMaxRequestBytes matches huma's default request body limit
// (huma.Config MaxBodyBytes), so an operation accepts the same body
// size over GraphQL as over REST.
const defaultMaxRequestBytes = 1 << 20

// planCacheEntries bounds the parsed-query cache. gat fronts one
// binary's own operations, so the realistic working set is the query
// set that binary's clients ship — small. Matches gw/'s default.
const planCacheEntries = 1000

// graphqlBufPool reuses []byte buffers across GraphQL responses to
// keep allocs off the hot path. New buffers start at 4 KB — enough
// for most responses; ExecutePlanAppend grows the slice as needed.
// graphqlBufPoolMax caps what we hand back so a one-off megabyte
// response doesn't pin a fat allocation.
const graphqlBufPoolMax = 64 * 1024

var graphqlBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// jsonParam decodes a JSON object query parameter; nil when absent.
func jsonParam(params url.Values, name string) (map[string]any, error) {
	raw := params.Get(name)
	if raw == "" {
		return nil, nil
	}
	var m map[string]any
	err := json.Unmarshal([]byte(raw), &m)
	return m, err
}

func writeError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"message": msg}},
	})
}

// contextKey for attaching *http.Request to context.
type contextKey int

const httpRequestKey contextKey = iota

// WithHTTPRequest attaches the inbound *http.Request to ctx.
//
// Stability: experimental
func WithHTTPRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, httpRequestKey, r)
}

// HTTPRequestFromContext extracts the *http.Request from ctx.
//
// Stability: experimental
func HTTPRequestFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(httpRequestKey).(*http.Request)
	return r
}
