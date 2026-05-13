# Distributed tracing (OpenTelemetry)

gwag emits OpenTelemetry spans for every ingress request and every
upstream dispatch when `WithTracer` is set. Inbound `traceparent`
headers join the caller's trace; outbound calls propagate
`traceparent` on HTTP headers and gRPC metadata so downstream services
see one continuous trace from the user agent through every hop.

```
[client] ──traceparent──▶ [gwag]
                            ├─ gateway.graphql / .http / .grpc                  (server span)
                            │   └─ gateway.dispatch.proto / .openapi / .graphql (client span)
                            │       └─ ──traceparent──▶ upstream service
                            └─ ...
```

When `WithTracer` is unset, the gateway wires a no-op tracer; spans are
not created, attributes are not allocated, and per-request cost stays
near zero. There is no separate "disable tracing" knob — omit the
option.

## Wiring

```go
import (
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    "go.opentelemetry.io/otel/sdk/resource"
    semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

    gateway "github.com/iodesystems/gwag/gw"
)

exp, err := otlptracegrpc.New(ctx,
    otlptracegrpc.WithEndpoint("otel-collector:4317"),
    otlptracegrpc.WithInsecure(),
)
if err != nil { panic(err) }

tp := sdktrace.NewTracerProvider(
    sdktrace.WithBatcher(exp),
    sdktrace.WithResource(resource.NewWithAttributes(
        semconv.SchemaURL,
        semconv.ServiceName("gwag"),
    )),
)

gw := gateway.New(
    gateway.WithTracer(tp),
    // ...other options
)
```

Any `trace.TracerProvider` works — the SDK at
`go.opentelemetry.io/otel/sdk` is the canonical choice. Common collectors
and backends are wired the same way:

| Backend | Exporter import |
|---|---|
| Jaeger | `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` (Jaeger speaks OTLP since 1.35) |
| Honeycomb | `otlptracegrpc` with endpoint `api.honeycomb.io:443`, header `x-honeycomb-team: <key>` |
| OTel Collector | `otlptracegrpc` or `otlptracehttp` against the collector's receiver address |
| stdout (dev) | `go.opentelemetry.io/otel/exporters/stdout/stdouttrace` |

Tracer shutdown belongs to the operator — flush + close `tp` on
graceful exit so buffered spans reach the exporter:

```go
defer func() {
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = tp.Shutdown(shutdownCtx)
}()
```

## Span reference

### Server-kind (ingress)

One per request. Lifetime spans the full ingress handler — from the
moment the gateway extracts inbound headers to the moment the response
is written. WebSocket subscriptions are excluded (the per-request
histograms also skip them); the subscription's open-side dispatch
still emits its own span under the request that initiated it.

| Span name | Ingress shape |
|---|---|
| `gateway.graphql` | GraphQL POST + GraphiQL — `gw.Handler()` |
| `gateway.http` | HTTP/JSON proto-style + OpenAPI passthrough — `gw.IngressHandler()` |
| `gateway.http.subscription` | SSE subscription over the HTTP ingress |
| `gateway.grpc` | gRPC unary unknown-service handler — `gw.GRPCUnknownHandler()` |
| `gateway.grpc.subscription` | gRPC server-streaming unknown-service handler |

Attributes:

| Key | Value |
|---|---|
| `gateway.ingress` | `graphql` / `http` / `grpc` |
| `gateway.namespace` | namespace of the operation (HTTP / gRPC only — GraphQL doesn't know at request start) |
| `gateway.version` | version string (`unstable` / `v1` / etc.) |
| `gateway.method` | flat operation name |
| `http.method` | `POST` / `GET` (HTTP + GraphQL) |
| `http.target` | request URL path (HTTP + GraphQL) |
| `http.route` | template path with `{placeholders}` (HTTP only) |
| `rpc.system` | `grpc` (gRPC only) |
| `rpc.method` | full gRPC method path (gRPC only) |

### Client-kind (dispatch)

One per upstream call, nested under the ingress span. Lifetime spans
the dispatcher entry — including backpressure wait, replica pick,
header injection, and the upstream call — until the response is
decoded or an error is returned.

| Span name | Dispatcher |
|---|---|
| `gateway.dispatch.proto` | Proto unary (gRPC outbound) |
| `gateway.dispatch.openapi` | OpenAPI passthrough (HTTP outbound) |
| `gateway.dispatch.graphql` | Downstream GraphQL forwarder (HTTP outbound) |
| `gateway.dispatch.internal` | In-process internal-proto handlers (no outbound) |
| `gateway.dispatch.proto.subscription` | Proto server-streaming open-side (gRPC outbound) |

Attributes:

| Key | Value |
|---|---|
| `gateway.namespace`, `gateway.version`, `gateway.method` | same shape as ingress |
| `http.method`, `http.route` | OpenAPI only |
| `rpc.system`, `rpc.method` | proto + proto subscription |

### Propagation

`traceparent` (W3C TraceContext) is the only propagator wired —
gwag does not currently support B3 / Jaeger native / OT propagation
formats. The propagator is symmetric: every inbound shape extracts,
every outbound shape injects.

| Direction | Carrier | Where |
|---|---|---|
| Inbound HTTP / GraphQL | request headers | `r.Header` |
| Inbound gRPC | incoming metadata | `metadata.FromIncomingContext` |
| Outbound HTTP (OpenAPI) | request headers | injected after `forwardOpenAPIHeaders` |
| Outbound HTTP (GraphQL forwarder) | request headers | same point in `dispatchGraphQL` |
| Outbound gRPC (proto unary) | outgoing metadata | injected before `r.conn.Invoke` |
| Outbound gRPC (proto subscription) | outgoing metadata | injected before `r.conn.NewStream` |

A client that already started a trace upstream of the gateway sees one
continuous trace. A client that did not is the trace's root; the
gateway opens a fresh root span.

## What gwag does NOT instrument

- **Background reconcilers / cluster watch loops / NATS housekeeping.**
  Background work uses `noopTracer` so trace volume tracks request
  volume rather than cluster size. The plan calls out
  cluster-reconciler spans as a followup if "I can't tell where a
  registration is stuck" surfaces from an operator.
- **Per-event subscription frames.** A subscription's open-side gets a
  dispatch span; per-frame spans would balloon trace cardinality
  without payoff. Operators who need per-frame visibility add their
  own instrumentation in the downstream service.
- **Internal admin operations (huma router).** Admin endpoints
  dogfood the OpenAPI ingest path — they go through
  `gateway.dispatch.internal` like any in-process handler. There's no
  separate "admin span" namespace.
- **Plan-cache / schema-rebuild internals.** These are gateway-state
  transitions, not per-request work; they don't take a span.

## Honest perf disclosure

When `WithTracer` is unset, the gateway wires a no-op tracer at
construction and the per-request hot path stays branch-free
(`tr.enabled` short-circuits the propagator extract + inject calls).
The cost is one extra interface call per ingress + per dispatch — too
small to measure on the existing `BenchmarkProtoSchemaExec`. With
`WithTracer` set, the cost depends on the chosen sampler + exporter;
see `docs/perf.md` for the overhead row.

## Common questions

**"Can I plug an existing OpenTelemetry interceptor in instead?"**
Yes — the gateway opens its own spans, but it doesn't fight an
upstream interceptor. If you wrap `gw.GRPCUnknownHandler()` in
`grpc.UnaryServerInterceptor(otelgrpc.UnaryServerInterceptor())`, both
spans land in the same trace; the interceptor's span is the parent of
the gateway's `gateway.grpc` span because extraction runs against the
metadata the interceptor has already populated.

**"Why is `gateway.namespace` empty on `gateway.graphql`?"** Because
the GraphQL parser hasn't run yet when the ingress span opens — a
single query can touch multiple namespaces. The per-dispatch span has
the namespace.

**"Can I add custom attributes to the per-request span?"** Not via a
gwag-side hook today. Fish the span out of the request context with
`trace.SpanFromContext(ctx)` from any middleware mounted around
`gw.Handler()`; that span is the ingress span.

**"Does the gateway sample?"** No — sampling is the `TracerProvider`'s
job. Use `sdktrace.WithSampler(sdktrace.TraceIDRatioBased(0.01))` on
the provider you pass to `WithTracer` for production rates.
