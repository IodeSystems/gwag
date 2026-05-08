package gateway

import (
	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// registerProtoDispatchersLocked walks every proto pool matching
// `filter`, builds a backpressure-wrapped protoDispatcher per unary
// RPC (and a protoSubscriptionDispatcher per server-streaming RPC),
// and registers each one in g.dispatchers under
// MakeSchemaID(ns, ver, methodName). The methodName is the wire-level
// PascalCase name (e.g. "Hello", "Greetings") to match what IR's
// IngestProto + PopulateSchemaIDs stamp on op.SchemaID —
// RenderGraphQLRuntime's resolvers look up dispatchers by op.SchemaID
// at call time, so the two sides have to agree on the key shape.
//
// Client-streaming / bidi RPCs are filtered out at registration
// time (control.go) so they never reach a pool — no dispatcher is
// emitted for them here.
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
				if md.IsStreamingClient() {
					continue
				}
				sid := ir.MakeSchemaID(p.key.namespace, p.key.version, string(md.Name()))
				if md.IsStreamingServer() {
					g.dispatchers.Set(sid, newProtoSubscriptionDispatcher(g, p.key.namespace, p.key.version, string(md.Name()), md.Output()))
					continue
				}
				label := methodLabel(sd, md)
				core := newProtoDispatcher(p, sd, md, chain, metrics, bp)
				dispatcher := BackpressureMiddleware(poolBackpressureConfig(p, label, metrics, bp))(core)
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
		injectProtoSubscriptionAuthArgs(svc)
		ir.PopulateSchemaIDs(svc)
	}
	return out, nil
}

// injectProtoSubscriptionAuthArgs appends the gateway's HMAC-auth
// arguments (hmac, timestamp, kid) to every server-streaming proto
// op so the rendered SDL surfaces them and the WS auth verifier sees
// them in args. The legacy buildSubscriptionField stamped the same
// triple inline; doing it here keeps the IR canonical for the
// renderer.
//
// hmac + timestamp are required (NonNull); kid is optional and used
// only when the gateway runs SubscriptionAuthOptions.Secrets for key
// rotation.
func injectProtoSubscriptionAuthArgs(svc *ir.Service) {
	for _, op := range svc.Operations {
		if op.Kind != ir.OpSubscription {
			continue
		}
		op.Args = append(op.Args,
			&ir.Arg{Name: "hmac", Type: ir.TypeRef{Builtin: ir.ScalarString}, Required: true},
			&ir.Arg{Name: "timestamp", Type: ir.TypeRef{Builtin: ir.ScalarInt32}, Required: true},
			&ir.Arg{Name: "kid", Type: ir.TypeRef{Builtin: ir.ScalarString}},
		)
	}
}
