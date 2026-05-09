package gateway

import (
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// registerOpenAPIDispatchersLocked walks IR services from the
// slot-IR collection and registers one backpressure-wrapped
// openAPIDispatcher in g.dispatchers per Operation, keyed by
// op.SchemaID. RenderGraphQLRuntime's resolvers look up dispatchers
// via the same SchemaID; keeping registration paired with the same
// IR walk that feeds the renderer is the simplest way to keep them
// in sync as ops are added / hidden / filtered.
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
	headers := g.headerInjectorSnapshot()
	for _, svc := range svcs {
		if svc.OriginKind != ir.KindOpenAPI {
			continue
		}
		src := g.openAPISlot(poolKey{namespace: svc.Namespace, version: svc.Version})
		if src == nil {
			continue
		}
		for _, op := range svc.Operations {
			openAPIOp, _ := op.Origin.(*openapi3.Operation)
			if openAPIOp == nil {
				return fmt.Errorf("openapi: ingest dropped op origin for %s/%s/%s", svc.Namespace, svc.Version, op.Name)
			}
			core := newOpenAPIDispatcher(src, openAPIOp, op.HTTPMethod, op.HTTPPath, headers, metrics, bp)
			dispatcher := BackpressureMiddleware(openAPIBackpressureConfig(src, core.label, metrics, bp))(core)
			g.dispatchers.Set(op.SchemaID, dispatcher)
		}
	}
	return nil
}
