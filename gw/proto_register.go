package gateway

import (
	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// registerProtoDispatchersLocked walks every proto pool matching
// `filter`, builds a backpressure-wrapped protoDispatcher per unary
// RPC, and registers each one in g.dispatchers under
// MakeSchemaID(ns, ver, methodName). The methodName is the wire-level
// PascalCase name (e.g. "Hello") to match what IR's IngestProto +
// PopulateSchemaIDs stamp on op.SchemaID — RenderGraphQLRuntime's
// resolvers resolve dispatchers by op.SchemaID at call time, so the
// two sides have to agree on the key shape.
//
// Streaming RPCs are skipped: server-streaming methods go through the
// legacy buildSubscriptionFields path until step 6 unifies that
// surface; client-streaming / bidi are filtered out at registration
// time (control.go) so they never reach a pool.
//
// Caller holds g.mu.
func (g *Gateway) registerProtoDispatchersLocked(filter schemaFilter) {
	chain := g.runtimeChain()
	metrics := g.cfg.metrics
	bp := g.cfg.backpressure
	for _, p := range g.pools {
		if !filter.matchPool(p.key) {
			continue
		}
		if g.isInternal(p.key.namespace) {
			continue
		}
		services := p.file.Services()
		for i := 0; i < services.Len(); i++ {
			sd := services.Get(i)
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				if md.IsStreamingClient() || md.IsStreamingServer() {
					continue
				}
				label := methodLabel(sd, md)
				core := newProtoDispatcher(p, sd, md, chain, metrics)
				dispatcher := BackpressureMiddleware(poolBackpressureConfig(p, label, metrics, bp))(core)
				sid := ir.MakeSchemaID(p.key.namespace, p.key.version, string(md.Name()))
				g.dispatchers.Set(sid, dispatcher)
			}
		}
	}
}

// protoServicesAsIRLocked distills the proto pools matching `filter`
// into ir.Services. Mirrors the proto branch of gatewayServicesAsIR
// but doesn't take g.mu (caller holds it) and stays scoped to the
// proto cutover — OpenAPI / GraphQL remain on their own field
// builders until steps 4 & 5. Internal namespaces drop via
// HideInternal; per-Pair Hides strip hidden message fields.
//
// Each returned Service has Namespace / Version / Internal stamped
// before HideInternal + Filter run, and SchemaIDs populated last so
// RenderGraphQLRuntime can resolve dispatchers by op.SchemaID.
//
// Caller holds g.mu.
func (g *Gateway) protoServicesAsIRLocked(filter schemaFilter) ([]*ir.Service, error) {
	out := []*ir.Service{}
	for _, p := range g.pools {
		if !filter.matchPool(p.key) {
			continue
		}
		svcs := ir.IngestProto(p.file)
		for _, svc := range svcs {
			svc.Namespace = p.key.namespace
			svc.Version = p.key.version
			svc.Internal = g.isInternal(p.key.namespace)
			out = append(out, svc)
		}
	}
	out = ir.HideInternal(out)
	if hide := g.hidesSet(); len(hide) > 0 {
		ir.Hides(out, hide)
	}
	for _, svc := range out {
		ir.PopulateSchemaIDs(svc)
	}
	return out, nil
}
