# Comparison with adjacent tools

Different layers, different scope. You'll typically run more than
one of these.

## Service discovery (k8s, Consul, etcd)

Service discovery routes *bytes* — `auth-svc.cluster.local:50051`
resolves to a pod IP. It doesn't know what the service offers, what
its schema is, or whether your client is on an out-of-date version.
You hand-write or hand-wire the client.

gwag sits above discovery. Discovery answers "where is the auth
service?"; gwag answers "what does it expose, in what shape, and
how do I codegen a typed client?" Run both.

## Service mesh (Istio, Linkerd, Consul Connect)

Meshes own L7 wire concerns: mTLS, retries, traffic shifting,
circuit-breaking. Observability is wire-shaped — bytes/sec, status
codes.

gwag owns *schema* concerns: typed dispatch, versioning, deprecation
propagation through codegen, schema diff in CI, hide-and-inject
middleware, per-method backpressure. Observability is schema-shaped
— `dispatch_duration_seconds{namespace,version,method}`, recent
callers per deprecated method.

Mesh for the wire, gateway for the contract.

## Edge API gateways (Kong, APIGee, AWS API Gateway)

Most edge gateways are HTTP-routing-shaped: path rewrite, auth-header
injection, rate-limit, forward. Single inbound protocol, single
outbound, no schema unification across formats.

## Apollo Federation

Closest peer for the unified-GraphQL story, but Federation only
ingests GraphQL — `.proto` and OpenAPI need a bridge. Different
problem too: Federation does entity-merging across services sharing
entity identity. See [`federation.md`](./federation.md).

## Axes

| Concern | Edge gateways | Apollo Federation | gwag |
|---|---|---|---|
| **Ingest formats** | HTTP/REST (some gRPC plugin) | GraphQL only | `.proto`, OpenAPI 3.x, GraphQL stitching |
| **Codegen targets** | None | GraphQL SDL only | Proto FDS + OpenAPI + GraphQL SDL, simultaneously |
| **Versioning** | Per-route, manual | Per-subgraph, manual | Tier model with auto-deprecation + dep-negotiation gate |
| **Metrics shape** | Wire (status, RPS) | GraphQL (operation, type) | Schema-aware: per-pool, per-replica, per-method, per-tier |
| **Transforms** | Plugins, often language-locked | Schema directives + custom resolvers | Go middleware `Pair` — one declaration across all protocols |
| **HA** | External LB | Stateless; depends on subgraphs | Embedded NATS + JetStream KV |

## What gwag doesn't replace

- **Service discovery.** Bridge into the gateway via the gRPC
  control plane.
- **Service mesh, mTLS, traffic shifting at the wire.** Sibling
  layer.
- **Observability backends.** Metrics export Prometheus; pick the
  scraper, alerting, dashboards.
- **CI/CD.** Control plane lets you wire register/deregister into
  the pipeline; the pipeline itself is yours.

## Benchmarks

Measured numbers vs graphql-mesh and Apollo Router on the same
hardware: [`perf/comparison.md`](../perf/comparison.md). Harness:
[`perf/`](../perf).
