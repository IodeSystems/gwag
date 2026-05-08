package gateway

import (
	"sort"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// graphQLServicesAsIRLocked distills the downstream-GraphQL ingest
// sources matching `filter` into ir.Services. Sibling of
// protoServicesAsIRLocked / openAPIServicesAsIRLocked.
//
// IngestGraphQL parses the source's cached introspection JSON; failed
// parses are skipped silently (matches the pre-cutover posture in
// gatewayServicesAsIR — a malformed introspection is logged at boot
// and shouldn't take down schema rebuilds).
//
// Caller holds g.mu.
func (g *Gateway) graphQLServicesAsIRLocked(filter schemaFilter) ([]*ir.Service, error) {
	out := []*ir.Service{}
	for k, src := range g.graphQLSources {
		if !filter.matchPool(k) {
			continue
		}
		svc, err := ir.IngestGraphQL(src.rawIntrospection)
		if err != nil {
			continue
		}
		svc.Namespace = k.namespace
		svc.Version = k.version
		svc.Internal = g.isInternal(k.namespace)
		out = append(out, svc)
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

// registerGraphQLDispatchersLocked walks the IR services produced by
// graphQLServicesAsIRLocked and registers one backpressure-wrapped
// graphQLDispatcher per Operation, keyed by op.SchemaID.
//
// The dispatcher captures a *graphQLMirror so it can reuse
// rewriteFieldForRemote / unprefixTypeName for AST forwarding —
// graphql→graphql forwarding can't reconstruct an upstream query from
// canonical args alone (it needs the local selection-set), so the
// mirror's AST helpers stay alive even though its build() / type
// construction path is replaced by RenderGraphQLRuntime. mirror.isLatest
// is set per-source so unprefixTypeName picks the right `<ns>_` /
// `<ns>_<vN>_` prefix when stripping local type names off inline
// fragments before forwarding.
//
// Caller holds g.mu.
func (g *Gateway) registerGraphQLDispatchersLocked(svcs []*ir.Service) error {
	metrics := g.cfg.metrics
	bp := g.cfg.backpressure

	// Compute the latest version per namespace so the mirror's
	// unprefixTypeName uses the matching prefix shape. Mirrors the
	// versionN sort the renderer applies.
	latestByNS := map[string]int{}
	for _, svc := range svcs {
		if svc.OriginKind != ir.KindGraphQL {
			continue
		}
		src := g.graphQLSources[poolKey{namespace: svc.Namespace, version: svc.Version}]
		if src == nil {
			continue
		}
		if v, ok := latestByNS[svc.Namespace]; !ok || src.versionN > v {
			latestByNS[svc.Namespace] = src.versionN
		}
	}

	// Stable order for reproducibility — register in the same order the
	// renderer walks namespaces and operations.
	sorted := append([]*ir.Service(nil), svcs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Version < sorted[j].Version
	})

	for _, svc := range sorted {
		if svc.OriginKind != ir.KindGraphQL {
			continue
		}
		src := g.graphQLSources[poolKey{namespace: svc.Namespace, version: svc.Version}]
		if src == nil {
			continue
		}
		mirror := newGraphQLMirror(src)
		mirror.isLatest = src.versionN == latestByNS[svc.Namespace]
		registerGraphQLOps(g.dispatchers, mirror, src, svc.Operations, metrics, bp)
		for _, grp := range svc.Groups {
			registerGraphQLGroupOps(g.dispatchers, mirror, src, grp, metrics, bp)
		}
	}
	return nil
}

func registerGraphQLOps(registry *ir.DispatchRegistry, mirror *graphQLMirror, src *graphQLSource, ops []*ir.Operation, metrics Metrics, bp BackpressureOptions) {
	for _, op := range ops {
		opLabel := graphQLOpLabel(op.Kind)
		core := newGraphQLDispatcher(mirror, op.Name, opLabel, metrics)
		// Subscriptions skip BackpressureMiddleware: src.sem is the
		// per-source unary slot count, not a stream lifetime gauge,
		// and the pre-cutover subscribingResolver bypassed it for the
		// same reason. Stream rate-control rides on the subscription
		// broker / per-replica inflight instead.
		var dispatcher ir.Dispatcher = core
		if op.Kind != ir.OpSubscription {
			dispatcher = BackpressureMiddleware(graphQLBackpressureConfig(src, core.label, metrics, bp))(core)
		}
		registry.Set(op.SchemaID, dispatcher)
	}
}

func registerGraphQLGroupOps(registry *ir.DispatchRegistry, mirror *graphQLMirror, src *graphQLSource, grp *ir.OperationGroup, metrics Metrics, bp BackpressureOptions) {
	registerGraphQLOps(registry, mirror, src, grp.Operations, metrics, bp)
	for _, sub := range grp.Groups {
		registerGraphQLGroupOps(registry, mirror, src, sub, metrics, bp)
	}
}

func graphQLOpLabel(kind ir.OpKind) string {
	switch kind {
	case ir.OpMutation:
		return "mutation"
	case ir.OpSubscription:
		return "subscription"
	default:
		return "query"
	}
}
