package gateway

import (
	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// gatewayServicesAsIR walks the gateway's three source registries —
// proto pools, OpenAPI sources, GraphQL ingest sources — and
// distills each into an ir.Service. Selectors filter the result;
// internal namespaces are dropped; Pair.Hides is applied to strip
// hidden fields. Every entry's Origin is the source descriptor /
// spec, so a same-kind render reproduces the original verbatim.
//
// Caller passes the ParseSelectors output (or nil for "all"). Hides
// is read from g.pairs to match the existing runtime middleware
// surface.
func (g *Gateway) gatewayServicesAsIR(selectors []ir.Selector) ([]*ir.Service, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := []*ir.Service{}

	// Proto pools.
	for _, p := range g.pools {
		svcs := ir.IngestProto(p.file)
		for _, svc := range svcs {
			svc.Namespace = p.key.namespace
			svc.Version = p.key.version
			svc.Internal = g.isInternal(p.key.namespace)
			out = append(out, svc)
		}
	}

	// OpenAPI sources.
	for k, src := range g.openAPISources {
		svc := ir.IngestOpenAPI(src.doc)
		svc.Namespace = k.namespace
		svc.Version = k.version
		svc.Internal = g.isInternal(k.namespace)
		out = append(out, svc)
	}

	// Downstream-GraphQL ingest sources. The introspection lives
	// as raw JSON on the source — feed it straight into the IR.
	for k, src := range g.graphQLSources {
		svc, err := ir.IngestGraphQL(src.rawIntrospection)
		if err != nil {
			continue
		}
		svc.Namespace = k.namespace
		svc.Version = k.version
		svc.Internal = g.isInternal(k.namespace)
		out = append(out, svc)
	}

	// Apply transforms. Order matters: Hides shouldn't drop fields
	// from internal-only services we'll filter out anyway, but it
	// also doesn't hurt; HideInternal first cuts the working set.
	out = ir.HideInternal(out)
	out = ir.Filter(out, selectors)
	if hide := g.hidesSet(); len(hide) > 0 {
		ir.Hides(out, hide)
	}

	// Stamp SchemaIDs once Namespace/Version are set and the working
	// set is final. Renderers that build runtime resolvers look up
	// Dispatchers by SchemaID; keeping the stamp here means every
	// IR consumer sees populated ids without reaching into the
	// per-source ingest paths.
	for _, svc := range out {
		ir.PopulateSchemaIDs(svc)
	}
	return out, nil
}

// hidesSet collects every Pair.Hides entry into a string-keyed set
// the IR's Hides transform consumes. Caller holds g.mu.
func (g *Gateway) hidesSet() map[string]bool {
	out := map[string]bool{}
	for _, p := range g.pairs {
		for _, t := range p.Hides {
			out[string(t)] = true
		}
	}
	return out
}

// irSelectorsFromSchema converts a parsed []serviceSelector (the
// existing parseProtoSelectors output) into the IR's []Selector
// shape. Trivially structural — kept as a tiny adapter so handler
// code reads cleanly.
func irSelectorsFromSchema(s []serviceSelector) []ir.Selector {
	out := make([]ir.Selector, 0, len(s))
	for _, sel := range s {
		out = append(out, ir.Selector{Namespace: sel.namespace, Version: sel.version})
	}
	return out
}
