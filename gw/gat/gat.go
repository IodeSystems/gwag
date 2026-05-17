// Package gat (GraphQL API Translator) is gwag's embedded sibling: a
// single-server, in-process translator that turns OpenAPI / proto /
// GraphQL specs into a GraphQL surface mountable on the adopter's
// existing huma router. No NATS, no cluster, no admin endpoints, no
// MCP — just spec-to-GraphQL with HTTP dispatch.
package gat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/IodeSystems/graphql-go"

	"github.com/iodesystems/gwag/gw/ir"
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
// Stability: experimental
type Gateway struct {
	schema   *graphql.Schema
	registry *ir.DispatchRegistry
	services []*ir.Service

	// captured holds ops registered via gat.Register for paired huma
	// dispatch. Empty when only the BYO-IR path is used.
	captured []*capturedOp

	// built is true once the schema has been assembled (either by
	// New(regs...) or by RegisterHuma). Subsequent registrations are
	// rejected.
	built bool

	// pubsub is gat's in-process publish/subscribe primitive, always
	// available via PubSub(). mesh is the optional best-effort
	// cross-node fanout layer — nil until EnablePeerMesh is called.
	pubsub *pubSub
	mesh   *peerMesh
}

// ServiceRegistration pairs an IR service with its dispatch config.
//
// Stability: experimental
type ServiceRegistration struct {
	Service   *ir.Service
	BaseURL   string // upstream HTTP base URL for OpenAPI dispatch
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
// Stability: experimental
func New(regs ...ServiceRegistration) (*Gateway, error) {
	g := &Gateway{
		registry: ir.NewDispatchRegistry(),
		pubsub:   newPubSub(),
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
	longScalar, jsonScalar := ir.StandardScalars()
	schema, err := ir.RenderGraphQLRuntime(g.services, g.registry, ir.RuntimeOptions{
		LongType: longScalar,
		JSONType: jsonScalar,
	})
	if err != nil {
		return fmt.Errorf("gat: build schema: %w", err)
	}
	g.schema = schema
	g.built = true
	return nil
}

// Handler returns an http.Handler that serves GraphQL queries and
// mutations. POST /graphql with JSON body; GET with ?query= param.
// WebSocket subscriptions are not supported in gat mode.
//
// Stability: experimental
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "upgrade not supported", http.StatusNotFound)
			return
		}

		var (
			query         string
			variables     map[string]any
			operationName string
		)

		if r.Method == http.MethodPost {
			var req struct {
				Query         string         `json:"query"`
				Variables     map[string]any `json:"variables"`
				OperationName string         `json:"operationName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			query = req.Query
			variables = req.Variables
			operationName = req.OperationName
		} else {
			query = r.URL.Query().Get("query")
			operationName = r.URL.Query().Get("operationName")
		}

		if query == "" {
			writeError(w, "missing query", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		result := graphql.Do(graphql.Params{
			Schema:         *g.schema,
			RequestString:  query,
			VariableValues: variables,
			OperationName:  operationName,
			Context:        ctx,
		})

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if len(result.Errors) > 0 {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(result)
	})
}

// Schema returns the assembled graphql.Schema for introspection.
//
// Stability: experimental
func (g *Gateway) Schema() *graphql.Schema {
	return g.schema
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
