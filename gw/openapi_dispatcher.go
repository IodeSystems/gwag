package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// openAPIDispatcher implements ir.Dispatcher for one OpenAPI source
// + one operation. Inner work mirrors the inline body in
// gw/openapi.go (pre-cutover): pickReplica → dispatchOpenAPI →
// RecordDispatch. BackpressureMiddleware wraps the outside.
//
// One per (source, operation) at schema build time. The captured
// op pointer / method / pathTemplate / forwardHeaders / metrics
// labels live for the lifetime of the source — the dispatcher is
// short-lived only across schema rebuilds.
type openAPIDispatcher struct {
	src            *openAPISource
	op             *openapi3.Operation
	method         string
	pathTemplate   string
	forwardHeaders []string
	metrics        Metrics
	bp             BackpressureOptions
	ns             string
	ver            string
	label          string // "<METHOD> <pathTemplate>" — proto-side metric parity
}

func newOpenAPIDispatcher(src *openAPISource, op *openapi3.Operation, method, pathTemplate string, metrics Metrics, bp BackpressureOptions) *openAPIDispatcher {
	return &openAPIDispatcher{
		src:            src,
		op:             op,
		method:         method,
		pathTemplate:   pathTemplate,
		forwardHeaders: src.forwardHeaders,
		metrics:        metrics,
		bp:             bp,
		ns:             src.namespace,
		ver:            src.version,
		label:          method + " " + pathTemplate,
	}
}

func (d *openAPIDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	start := time.Now()
	r := d.src.pickReplica()
	if r == nil {
		err := Reject(CodeInternal, fmt.Sprintf("openapi: no live replicas for %s/%s", d.ns, d.ver))
		d.metrics.RecordDispatch(d.ns, d.ver, d.label, time.Since(start), err)
		return nil, err
	}
	releaseInstance, err := acquireOpenAPIReplicaSlot(ctx, r, d.src, d.label, d.metrics, d.bp)
	if err != nil {
		d.metrics.RecordDispatch(d.ns, d.ver, d.label, time.Since(start), err)
		return nil, err
	}
	defer releaseInstance()

	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	resp, err := dispatchOpenAPI(ctx, d.method, r.baseURL, d.pathTemplate, d.op, args, d.forwardHeaders, r.httpClient)
	d.metrics.RecordDispatch(d.ns, d.ver, d.label, time.Since(start), err)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// acquireOpenAPIReplicaSlot is the per-instance counterpart to
// acquireBackpressureSlot for OpenAPI sources. Same shape as the
// proto-side acquireReplicaSlot (kept distinct because pool and
// openAPISource have separate sem fields and metric namespaces).
func acquireOpenAPIReplicaSlot(ctx context.Context, r *openAPIReplica, src *openAPISource, label string, metrics Metrics, bp BackpressureOptions) (release func(), err error) {
	if r.sem == nil {
		return func() {}, nil
	}
	cfg := BackpressureConfig{
		Sem:         r.sem,
		Queueing:    &r.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   src.namespace,
		Version:     src.version,
		Label:       label,
		Kind:        "unary_instance",
	}
	return acquireBackpressureSlot(ctx, cfg)
}

// openAPIBackpressureConfig bundles a source's per-dispatch knobs
// for BackpressureMiddleware. Sibling of poolBackpressureConfig in
// gw/proto_dispatcher.go — kept separate because openAPISource and
// pool are separate types with separate sem / queueing fields.
func openAPIBackpressureConfig(src *openAPISource, label string, metrics Metrics, bp BackpressureOptions) BackpressureConfig {
	return BackpressureConfig{
		Sem:         src.sem,
		Queueing:    &src.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   src.namespace,
		Version:     src.version,
		Label:       label,
		Kind:        "unary",
	}
}

var _ ir.Dispatcher = (*openAPIDispatcher)(nil)
