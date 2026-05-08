package gateway

import (
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// openAPIServicesAsIRLocked distills the OpenAPI sources matching
// `filter` into ir.Services. Sibling of protoServicesAsIRLocked —
// caller already holds g.mu, transforms (HideInternal + schema
// rewrites) run in the same order as gatewayServicesAsIR, and
// SchemaIDs are stamped last so RenderGraphQLRuntime can resolve
// dispatchers by op.SchemaID at request time.
//
// Caller holds g.mu.
func (g *Gateway) openAPIServicesAsIRLocked(filter schemaFilter) ([]*ir.Service, error) {
	out := []*ir.Service{}
	for k, src := range g.openAPISources {
		if !filter.matchPool(k) {
			continue
		}
		svc := ir.IngestOpenAPI(src.doc)
		svc.Namespace = k.namespace
		svc.Version = k.version
		svc.Internal = g.isInternal(k.namespace)
		out = append(out, svc)
	}
	out = ir.HideInternal(out)
	g.applySchemaRewrites(out)
	for _, svc := range out {
		ir.PopulateSchemaIDs(svc)
	}
	return out, nil
}

// registerOpenAPIDispatchersLocked walks the IR services produced
// by openAPIServicesAsIRLocked and registers one
// backpressure-wrapped openAPIDispatcher in g.dispatchers per
// Operation, keyed by op.SchemaID. RenderGraphQLRuntime's resolvers
// look up dispatchers via the same SchemaID, so the two sides have
// to agree on the key shape — keeping registration paired with the
// same IR walk that feeds the renderer is the simplest way to keep
// them in sync as ops are added / hidden / filtered.
//
// Each Operation carries `*openapi3.Operation` on op.Origin (set by
// IngestOpenAPI) so the dispatcher's parameter encoding keeps using
// kin-openapi's shape. The IR is the type contract; the OpenAPI doc
// stays the wire contract.
//
// Caller holds g.mu.
func (g *Gateway) registerOpenAPIDispatchersLocked(svcs []*ir.Service) error {
	metrics := g.cfg.metrics
	bp := g.cfg.backpressure
	for _, svc := range svcs {
		if svc.OriginKind != ir.KindOpenAPI {
			continue
		}
		src := g.openAPISources[poolKey{namespace: svc.Namespace, version: svc.Version}]
		if src == nil {
			continue
		}
		for _, op := range svc.Operations {
			openAPIOp, _ := op.Origin.(*openapi3.Operation)
			if openAPIOp == nil {
				return fmt.Errorf("openapi: ingest dropped op origin for %s/%s/%s", svc.Namespace, svc.Version, op.Name)
			}
			core := newOpenAPIDispatcher(src, openAPIOp, op.HTTPMethod, op.HTTPPath, metrics, bp)
			dispatcher := BackpressureMiddleware(openAPIBackpressureConfig(src, core.label, metrics, bp))(core)
			g.dispatchers.Set(op.SchemaID, dispatcher)
		}
	}
	return nil
}
