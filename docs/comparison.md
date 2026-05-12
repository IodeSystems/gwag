# Why this vs service discovery, service meshes, or other API gateways

Three reasonable questions any new reader will have. The short
answer in each case is **different scope** — these systems sit at
different layers and you'll typically run more than one.

## "Isn't k8s service discovery / Consul / etcd enough?"

Service discovery routes *bytes* — `auth-svc.cluster.local:50051`
resolves to a pod IP. It doesn't know what the service offers,
whether it's deprecated, what its schema is, or whether your client
is using an out-of-date version of it. You still hand-write or
hand-wire the client.

This gateway sits *above* discovery. Discovery answers *"where is
the auth service?"*; the gateway answers *"what does the auth
service expose, in what shape, and how do I codegen a typed client
for my language?"* Both layers coexist — discovery routes within
the cluster; the gateway is the schema-aware aggregator.

## "Isn't a service mesh enough?"

Meshes (Istio, Linkerd, Consul Connect) own L7 wire concerns: mTLS,
retries, traffic shifting by percentage, circuit-breaking. Their
observability is wire-shaped — bytes/sec, request counts by HTTP
status code.

This gateway owns *schema* concerns: typed dispatch, versioning,
deprecation propagation through to client codegen, schema diff in
CI, hide-and-inject middleware, per-method backpressure. Its
observability is schema-shaped —
`dispatch_duration_seconds{namespace,version,method}`, p95 latency
per replica per version, recent callers per deprecated method.

Run both. Mesh for the wire, gateway for the contract.

## "Isn't Kong / APIGee / AWS API Gateway / Apollo Federation enough?"

Most edge-style API gateways are HTTP-routing-shaped: path rewrite,
auth-header injection, rate-limit, send to a backend. Single
inbound protocol, single outbound protocol, no schema unification
across formats.

Apollo Federation is the closest peer for the unified-GraphQL
story, but federation only ingests GraphQL — you can't drop a
`.proto` or an OpenAPI spec on it.

The axes that differ:

| Concern | Kong / APIGee / etc. | Apollo Federation | gwag |
|---|---|---|---|
| **Ingest formats** | HTTP/REST (some gRPC plugin) | GraphQL only | `.proto`, OpenAPI 3.x, GraphQL stitching |
| **Codegen targets** | None — operators bring their own | GraphQL SDL only | Proto FDS + OpenAPI + GraphQL SDL, simultaneously |
| **Versioning** | Per-route metadata; manual | Per-subgraph; manual | Tier model (`unstable` / `stable` / `vN`); auto-deprecation; dependency-negotiation forcing function |
| **Metrics shape** | Wire-shaped (status, RPS) | GraphQL-shaped (operation, type) | Schema-aware: per-pool, per-replica, per-method, per-tier |
| **Custom transforms** | Plugins, often language-locked | Schema directives + custom resolvers | Go middleware `Pair` — one declaration applies across all protocols |
| **HA story** | External LB | Stateless; depends on subgraphs | Embedded NATS + JetStream KV; any node dispatches to any service |

## Where this is specifically strong

Two points worth re-stating in the comparison frame above:

- **Schema-aware metrics.** Per-pool, per-replica, per-method
  dispatch latency and queue dwell with deprecation labels — wire-level
  metrics can answer *"is the auth service slow?"*, schema-level
  metrics can answer *"who's still calling deprecated `users.v1.list`?"*
- **One transform, every protocol.** A `Transform` (e.g. via
  `InjectType[T]`) hides a field from the public schema *and* fills it
  from request context in one declaration; the same code applies
  whether the underlying service is proto, OpenAPI, or downstream
  GraphQL.

## What it doesn't replace

- **Service discovery.** Use k8s Services / Consul / DNS / whatever
  you already have. Bridge into the gateway via the gRPC control
  plane.
- **Service mesh, mTLS, traffic shifting at the wire.** Sibling
  layer; run a mesh.
- **Observability backends.** Metrics export Prometheus; pick your
  scraper, alerting, and dashboards.
- **CI/CD and deploy automation.** The control plane lets you wire
  register/deregister into your deploy pipeline; the pipeline
  itself is yours.

## Head-to-head benchmarks

For measured comparison numbers against graphql-mesh and Apollo
Router on the same hardware, see [`perf/`](../perf) — Docker-hermetic
benchmark harness that boots each gateway in turn against shared
upstream backends and writes `perf/comparison.md` with the matrix.
