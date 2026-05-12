package gateway

import (
	"github.com/iodesystems/gwag/gw/ir"
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
	headers := g.headerInjectorSnapshot()
	metrics := g.cfg.metrics
	bp := g.cfg.backpressure
	for _, slot := range g.slots {
		switch slot.kind {
		case slotKindProto:
			p := slot.proto
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
						g.dispatchers.Set(sid, newProtoDirectSubscriptionDispatcher(g, p, sd, md))
						continue
					}
					label := methodLabel(sd, md)
					core := newProtoDispatcher(p, sd, md, chain, headers, metrics, bp)
					dispatcher := BackpressureMiddleware(poolBackpressureConfig(p, label, metrics, bp))(core)
					dispatcher = g.quotaMiddleware(p.key.namespace, p.key.version)(dispatcher)
					dispatcher = g.callerIDEnforceMiddleware()(dispatcher)
					g.dispatchers.Set(sid, dispatcher)
				}
			}
		case slotKindInternalProto:
			src := slot.internalProto
			if !filter.matchPool(slot.key) {
				continue
			}
			if g.isInternal(slot.key.namespace) {
				continue
			}
			services := src.file.Services()
			for i := 0; i < services.Len(); i++ {
				sd := services.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					md := methods.Get(j)
					if md.IsStreamingClient() {
						continue
					}
					sid := ir.MakeSchemaID(slot.key.namespace, slot.key.version, string(md.Name()))
					if md.IsStreamingServer() {
						// Server-streaming on the internal-proto kind
						// is the Subscription path (e.g. PubSub.Sub).
						// The slot's subscriptionHandlers map carries
						// the in-process broker function; if the
						// method has no handler entry, the dispatcher
						// is omitted and the field resolves to a
						// "no dispatcher" reject — same posture as a
						// missing unary handler.
						if src.subscriptionHandlers[string(md.Name())] == nil {
							continue
						}
						g.dispatchers.Set(sid, newInternalProtoSubscriptionDispatcher(src, md))
						continue
					}
					dispatcher := newInternalProtoDispatcher(src, sd, md, chain, metrics)
					var d ir.Dispatcher = dispatcher
					d = g.quotaMiddleware(slot.key.namespace, slot.key.version)(d)
					d = g.callerIDEnforceMiddleware()(d)
					g.dispatchers.Set(sid, d)
				}
			}
		}
	}
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

// protoSubscriptionAuthDoc is the canonical HMAC-channel-auth contract
// appended to every proto subscription op's Description. Single
// source of truth so SDL, /api/schema/graphql, and the MCP search
// corpus all carry the same auth-contract sentence. Surfaced
// adopter-facing — keep adopter-readable.
const protoSubscriptionAuthDoc = "Authenticated via HMAC channel token: subscribers obtain a token " +
	"(`gwag sign --channel <subject>` or `/api/admin/sign`) and pass it as " +
	"`hmac` + `timestamp` (+ optional `kid` for key rotation). " +
	"Subject pattern: `events.<namespace>.<Method>.<arg0>.<arg1>…`; " +
	"absent args wildcard to `*`."

// injectProtoSubscriptionAuthDoc appends the HMAC channel-auth
// contract to every proto subscription op's Description. Idempotent —
// rebakes won't double-append because bakeSlotIRLocked re-ingests
// from the source descriptor each time, restoring the original Doc.
func injectProtoSubscriptionAuthDoc(svc *ir.Service) {
	for _, op := range svc.Operations {
		if op.Kind != ir.OpSubscription {
			continue
		}
		if op.Description == "" {
			op.Description = protoSubscriptionAuthDoc
			continue
		}
		op.Description = op.Description + "\n\n" + protoSubscriptionAuthDoc
	}
}
