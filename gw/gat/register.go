package gat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// capturedOp is one paired Register entry. It records the huma
// Operation alongside type-erased input/output reflection plumbing
// and an invoker that types-asserts the input back to *I before
// calling the original handler.
//
// irOp is populated by ingestHuma once the IR projection is built —
// it lets the dispatch and gRPC layers share one binding path.
type capturedOp struct {
	op         huma.Operation
	inputType  reflect.Type // I (the value type behind *I)
	outputType reflect.Type // O (the value type behind *O)
	// invoke runs the middlewares, the input checks, then the handler.
	// args are the transport args in was bound from (see inputCheck).
	invoke func(ctx context.Context, in any, args map[string]any) (any, error)
	// check is nil when the op has nothing to check; transports skip
	// building args for it then.
	check *inputCheck
	// needsRequest is true when invoke runs a huma middleware chain or
	// input checks; transports without an *http.Request (gRPC ingress)
	// build one only then.
	needsRequest bool
	// hdr locates the output type's response-header fields; nil when it
	// has none.
	hdr          *outputHeaderPlan
	irOp         *ir.Operation // assigned during ingestHuma; nil until then
	instrumented bool          // invoke already wrapped by instrument
	// doc is the OpenAPI document of the API the op was registered on
	// (a huma.Group shares its parent's), so RegisterHumaVersion can
	// pick the ops of one API. claimed is set once that document was
	// ingested.
	doc     *huma.OpenAPI
	claimed bool
}

// Register is the paired drop-in for huma.Register. It registers the
// operation with huma exactly as huma.Register would, and also
// captures the handler ref on g so RegisterHuma can wire an
// in-process dispatcher (no loopback HTTP) for the GraphQL and gRPC
// surfaces gat builds on top of huma.
//
// Use this in place of huma.Register for any operation you want
// surfaced via gat. Operations registered with plain huma.Register
// are unaffected — they continue to serve REST/OpenAPI as before, but
// gat has no view into them.
//
// All Register calls must happen before RegisterHuma. Calling
// Register after the schema is built returns silently (huma
// registration still succeeds; the capture is dropped) — a hard
// panic was rejected because tests sometimes register more after
// inspecting the schema. Order matters in production code.
//
// Stability: stable
func Register[I, O any](api huma.API, g *Gateway, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	huma.Register(api, op, handler)
	if g.built {
		return
	}
	// Same chain, same order, captured at the same moment huma.Register
	// captures it: API stack first, then the op's own.
	var chain huma.Middlewares
	chain = append(chain, api.Middlewares()...)
	chain = append(chain, op.Middlewares...)
	opRef := op
	inputType := reflect.TypeOf((*I)(nil)).Elem()
	check := newInputCheck(api, op, inputType)
	g.captured = append(g.captured, &capturedOp{
		op:           op,
		inputType:    inputType,
		outputType:   reflect.TypeOf((*O)(nil)).Elem(),
		hdr:          newOutputHeaderPlan(reflect.TypeOf((*O)(nil)).Elem()),
		doc:          api.OpenAPI(),
		check:        check,
		needsRequest: len(chain) > 0 || check != nil,
		invoke: func(ctx context.Context, in any, args map[string]any) (any, error) {
			typed, ok := in.(*I)
			if !ok {
				return nil, fmt.Errorf("gat: input type mismatch for %s: got %T", op.OperationID, in)
			}
			call := func(ctx context.Context) (any, error) {
				if err := check.apply(ctx, &opRef, typed, args); err != nil {
					return nil, err
				}
				return handler(ctx, typed)
			}
			if len(chain) == 0 {
				return call(ctx)
			}
			return runHumaMiddlewares(ctx, chain, &opRef, call)
		},
	})
}

// RegisterHuma finalizes a paired-mode gateway: ingests huma's
// OpenAPI document into IR, wires in-process dispatchers for every
// captured operation, builds the GraphQL schema, then mounts the
// GraphQL endpoint and schema-view endpoints onto the adopter's huma
// router under prefix.
//
// Routes registered (prefix prepended):
//
//	POST {prefix}/graphql              — GraphQL queries / mutations
//	GET  {prefix}/graphql              — ?query= requests; graphql-transport-ws
//	                                     upgrade for Subscribe fields
//	GET  {prefix}/schema/graphql       — SDL (default) or introspection JSON
//	GET  {prefix}/schema/proto         — FileDescriptorSet (binary)
//	GET  {prefix}/schema/openapi       — re-emitted OpenAPI document
//
// Pass an empty prefix to mount at root. The prefix should not have
// a trailing slash; "/api" → "/api/graphql".
//
// After RegisterHumaVersion calls, RegisterHuma does not ingest api
// again: it builds the schema from the versions and mounts on api.
//
// Stability: stable
func RegisterHuma(api huma.API, g *Gateway, prefix string) error {
	if g.built {
		return fmt.Errorf("gat: gateway already finalized")
	}
	prefix = strings.TrimRight(prefix, "/")

	if len(g.versions) > 0 {
		// RegisterHumaVersion ingested the APIs already: build, mount.
		if err := g.buildVersions(); err != nil {
			return err
		}
	} else {
		if err := g.preflight(g.captured); err != nil {
			return err
		}
		if len(g.captured) > 0 || len(g.subs) > 0 {
			if err := g.ingestHuma(api); err != nil {
				return err
			}
			g.humaAPI = api
		}
		if err := g.build(); err != nil {
			return err
		}
	}

	if !g.noIntrospection {
		mountSchemaEndpoints(api, g, prefix)
	}
	mountGraphQLEndpoint(api, g, prefix)
	mountSubscribe(api, g, prefix)
	if g.mesh != nil {
		mountPeerPublish(api, g, prefix)
	}
	return nil
}

// preflight runs the mount-time checks on captured ops: parameters huma
// cannot see (an error unless AllowEmbeddedParams), and a note for
// OpenAPI response links.
func (g *Gateway) preflight(captured []*capturedOp) error {
	// huma skips unexported fields during parameter discovery, so a parameter on
	// an unexported field — or inside an embedded unexported type — is absent
	// from the schema and never bound. Report before building, not after
	// shipping.
	if found := hiddenParams(captured); len(found) > 0 {
		if !g.allowEmbeddedParams {
			return hiddenParamError(found)
		}
		for _, e := range found {
			log.Printf("gat: warning: %s", e)
		}
	}

	// OpenAPI response links are fine as documentation, but gat does not
	// resolve them into nested fields — on purpose. Point whoever
	// declared one at the pattern gat supports instead.
	for _, c := range captured {
		for code, resp := range c.op.Responses {
			if resp != nil && len(resp.Links) > 0 {
				log.Printf("gat: note: %s declares OpenAPI response links (%s); gat does not resolve links "+
					"into nested fields. To fetch dependent data in one request, batch the operations "+
					"and pass values along with @export — see \"Dependent operations\" in docs/gat.md.",
					c.op.OperationID, code)
				break
			}
		}
	}
	return nil
}

// ingestHuma reads the huma API's accumulated OpenAPI doc, ingests it
// into a fresh IR service, and wires an in-process dispatcher for each
// captured operation (matched by OperationID).
func (g *Gateway) ingestHuma(api huma.API) error {
	svc, err := g.ingestHumaService(g.mergedOpenAPI(api), api, g.captured, g.subs, "", "v1")
	if err != nil {
		return err
	}
	g.services = append(g.services, svc)
	return nil
}

// ingestHumaService ingests doc as the IR service (namespace, version)
// and wires an in-process dispatcher for each of captured and subs
// (matched by OperationID). An empty namespace derives from api's
// OpenAPI title.
func (g *Gateway) ingestHumaService(doc *huma.OpenAPI, api huma.API, captured []*capturedOp, subs []*streamOp, namespace, version string) (*ir.Service, error) {
	specBytes, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("gat: marshal huma OpenAPI: %w", err)
	}

	loader := openapi3.NewLoader()
	odoc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return nil, fmt.Errorf("gat: parse huma OpenAPI: %w", err)
	}

	svc := ir.IngestOpenAPI(odoc)
	if namespace != "" {
		svc.Namespace = namespace
	}
	if svc.Namespace == "" {
		svc.Namespace = humaNamespace(api)
	}
	if svc.Version == "" {
		svc.Version = version
	}
	ir.PopulateSchemaIDs(svc)

	byID := make(map[string]*capturedOp, len(captured))
	for _, c := range captured {
		byID[c.op.OperationID] = c
		c.claimed = true
	}

	for _, op := range svc.FlatOperations() {
		c, ok := byID[op.Name]
		if !ok {
			continue
		}
		c.irOp = op
		if c.check != nil {
			c.check.bodyArg = bodyArgOf(op)
		}
		// @derived: declare the codegen provenance of this generated SDL field — it
		// derives from the Go huma operation with this OperationID. Lets a consumer
		// (poly-lsp-mcp) follow the real edge to the source instead of guessing the
		// field↔OperationID mapping by replicating gat's naming rule.
		op.Annotations = append(op.Annotations, ir.Annotation{
			Name: "derived",
			Args: []ir.AnnotationArg{{Name: "operationId", Kind: ir.AnnString, Value: op.Name}},
		})
		g.instrument(c)
		g.registry.Set(op.SchemaID, newInprocDispatcher(c, op, g.emptyNilSlices))
	}

	if err := g.bindStreams(svc, subs); err != nil {
		return nil, err
	}
	return svc, nil
}

// humaNamespace derives a namespace from api's OpenAPI title via the
// shared ir.SanitizeNamespace rule (case preserved); "huma" when there
// is no usable title.
func humaNamespace(api huma.API) string {
	if oapi := api.OpenAPI(); oapi != nil && oapi.Info != nil && oapi.Info.Title != "" {
		if ns := ir.SanitizeNamespace(oapi.Info.Title); ns != "" {
			return ns
		}
	}
	return "huma"
}

// mountGraphQLEndpoint registers POST and GET {prefix}/graphql on api
// as huma adapter handlers that delegate to g.Handler. GET carries
// ?query= requests and the graphql-transport-ws upgrade; the upgrade
// relies on adapterResponseWriter forwarding Hijack.
func mountGraphQLEndpoint(api huma.API, g *Gateway, prefix string) {
	// Adapter-level handler: bypass huma's body codec since GraphQL has
	// its own request shape, and reuse g.Handler which already
	// understands query/mutation envelopes. Built once, not per request.
	h := g.Handler()
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		api.Adapter().Handle(&huma.Operation{
			Method: method,
			Path:   prefix + "/graphql",
		}, func(ctx huma.Context) {
			serveAdapter(h, ctx)
		})
	}
}

// mountSubscribe registers GET {prefix}/_gat/subscribe as a huma
// adapter handler — the WebSocket pubsub stream. Always mounted; the
// in-process pubsub primitive is always available. The WS upgrade
// relies on adapterResponseWriter forwarding Hijack to the
// underlying writer.
func mountSubscribe(api huma.API, g *Gateway, prefix string) {
	h := g.subscribeWSHandler()
	api.Adapter().Handle(&huma.Operation{
		Method: http.MethodGet,
		Path:   prefix + SubscribePath,
	}, func(ctx huma.Context) {
		serveAdapter(h, ctx)
	})
}

// mountPeerPublish registers POST {prefix}/_gat/publish as a huma
// adapter handler — the peer-mesh receive endpoint. Mounted only when
// EnablePeerMesh was called.
func mountPeerPublish(api huma.API, g *Gateway, prefix string) {
	h := g.peerPublishHandler()
	api.Adapter().Handle(&huma.Operation{
		Method: http.MethodPost,
		Path:   prefix + PeerPublishPath,
	}, func(ctx huma.Context) {
		serveAdapter(h, ctx)
	})
}

// mountSchemaEndpoints registers the three /schema/{graphql,proto,openapi}
// views as huma adapter handlers (no body codec; raw byte payloads).
func mountSchemaEndpoints(api huma.API, g *Gateway, prefix string) {
	mount := func(path string, h http.Handler) {
		api.Adapter().Handle(&huma.Operation{
			Method: http.MethodGet,
			Path:   prefix + path,
		}, func(ctx huma.Context) {
			serveAdapter(h, ctx)
		})
	}
	mount("/schema/graphql", schemaGraphQLHandler(g))
	mount("/schema/proto", schemaProtoHandler(g))
	mount("/schema/openapi", schemaOpenAPIHandler(g))

	// Also answer introspection POSTs on /schema/graphql by delegating to the
	// GraphQL handler (same as {prefix}/graphql). graphql-codegen introspects
	// via POST, so pointing it at the documented schema URL then works instead
	// of 405-ing. (The GET op above stays the SDL / ?format=json view.)
	gql := g.Handler()
	api.Adapter().Handle(&huma.Operation{
		Method: http.MethodPost,
		Path:   prefix + "/schema/graphql",
	}, func(ctx huma.Context) {
		serveAdapter(gql, ctx)
	})
}
