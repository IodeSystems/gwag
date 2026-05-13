package gateway

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/metadata"
)

const tracerName = "github.com/iodesystems/gwag/gw"

// Canonical span attributes. Three keys are shared across every
// ingress + dispatch span (gateway.ingress / gateway.namespace /
// gateway.method); the http.* / rpc.* keys follow OpenTelemetry
// semantic conventions so existing dashboards can render them.
const (
	attrIngress    = "gateway.ingress"
	attrNamespace  = "gateway.namespace"
	attrMethod     = "gateway.method"
	attrVersion    = "gateway.version"
	attrOperation  = "gateway.graphql.operation"
	attrHTTPMethod = "http.method"
	attrHTTPTarget = "http.target"
	attrHTTPRoute  = "http.route"
	attrRPCSystem  = "rpc.system"
	attrRPCMethod  = "rpc.method"
)

func ingressAttr(kind string) attribute.KeyValue   { return attribute.String(attrIngress, kind) }
func namespaceAttr(ns string) attribute.KeyValue   { return attribute.String(attrNamespace, ns) }
func methodAttr(m string) attribute.KeyValue       { return attribute.String(attrMethod, m) }
func versionAttr(v string) attribute.KeyValue      { return attribute.String(attrVersion, v) }
func httpMethodAttr(m string) attribute.KeyValue   { return attribute.String(attrHTTPMethod, m) }
func httpTargetAttr(t string) attribute.KeyValue   { return attribute.String(attrHTTPTarget, t) }
func httpRouteAttr(r string) attribute.KeyValue    { return attribute.String(attrHTTPRoute, r) }
func rpcMethodAttr(m string) attribute.KeyValue    { return attribute.String(attrRPCMethod, m) }
func grpcSystemAttr() attribute.KeyValue           { return attribute.String(attrRPCSystem, "grpc") }
func operationAttr(name string) attribute.KeyValue { return attribute.String(attrOperation, name) }

// tracer wraps the configured TracerProvider with a W3C TraceContext
// propagator. When WithTracer is unset, the underlying provider is a
// noop so callers can start spans unconditionally without nil checks
// or branch cost beyond an interface call.
type tracer struct {
	t       trace.Tracer
	prop    propagation.TextMapPropagator
	enabled bool
}

func newTracer(tp trace.TracerProvider) *tracer {
	if tp == nil {
		return &tracer{
			t:       noop.NewTracerProvider().Tracer(tracerName),
			prop:    propagation.TraceContext{},
			enabled: false,
		}
	}
	return &tracer{
		t:       tp.Tracer(tracerName),
		prop:    propagation.TraceContext{},
		enabled: true,
	}
}

// extractHTTP folds an inbound request's traceparent into ctx so child
// spans become continuations of the caller's trace.
func (tr *tracer) extractHTTP(ctx context.Context, h http.Header) context.Context {
	if !tr.enabled {
		return ctx
	}
	return tr.prop.Extract(ctx, propagation.HeaderCarrier(h))
}

// extractGRPC mirrors extractHTTP for gRPC incoming metadata.
func (tr *tracer) extractGRPC(ctx context.Context) context.Context {
	if !tr.enabled {
		return ctx
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return tr.prop.Extract(ctx, mdCarrier(md))
}

// injectHTTP writes the current span's traceparent onto outbound
// request headers before dispatch.
func (tr *tracer) injectHTTP(ctx context.Context, h http.Header) {
	if !tr.enabled {
		return
	}
	tr.prop.Inject(ctx, propagation.HeaderCarrier(h))
}

// injectGRPC writes the current span's traceparent onto outbound
// gRPC metadata before dispatch. Returns a context with the injected
// metadata appended to the outgoing pairs.
func (tr *tracer) injectGRPC(ctx context.Context) context.Context {
	if !tr.enabled {
		return ctx
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.MD{}
	} else {
		md = md.Copy()
	}
	tr.prop.Inject(ctx, mdCarrier(md))
	return metadata.NewOutgoingContext(ctx, md)
}

// startIngressSpan opens a server-kind span for an ingress request.
// Returns the new ctx (carrying the span) and the span itself; caller
// must End() it. When tracing is disabled, the noop tracer returns a
// recording-off span with negligible overhead.
func (tr *tracer) startIngressSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tr.t.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	)
}

// startDispatchSpan opens a client-kind span for an outbound dispatch
// (proto/openapi/graphql forwarder).
func (tr *tracer) startDispatchSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tr.t.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
}

// mdCarrier adapts metadata.MD to TextMapCarrier. gRPC metadata keys
// are lowercased on the wire; the W3C TraceContext propagator emits
// "traceparent" / "tracestate" which already conform.
type mdCarrier metadata.MD

func (c mdCarrier) Get(k string) string {
	v := metadata.MD(c).Get(k)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (c mdCarrier) Set(k, v string) { metadata.MD(c).Set(k, v) }

func (c mdCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}
