# gwag

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**Register every service once, in the protocol it already speaks.
Clients get typed access in any of three.**

`.proto`, OpenAPI 3.x, downstream GraphQL → one consolidated GraphQL
surface for browser/mobile, **plus** typed proto/OpenAPI/GraphQL
clients (codegen-friendly) for service-to-service. Same schema, same
auth middleware, same backpressure, same metrics across all three.

```
TS / mobile ─┐                              ┌── auth-svc      (.proto)
             ├──▶  /api/graphql      ──┐    ├── billing-svc   (OpenAPI)
service A ──▶│  /api/schema/{*} ◀── codegen ├── inventory-svc (.proto)
service B ──▶│  /api/ingress/...      ─┘    ├── legacy-svc    (GraphQL stitch)
             └──┬─ gwag (1 binary) ─┬─┘     ├── …
                │ tiered versioning │       └── any new service: register at runtime
                │ HA via NATS + JS  │
                │ subscriptions out │
                └───────────────────┘
```

## What you get

- **One GraphQL surface for clients** — no juggling N service URLs and M auth schemes; deprecations propagate through to client codegen.
- **Typed clients in all three formats for service-to-service** — proto FDS / OpenAPI / GraphQL SDL all re-emitted, simultaneously, from the same registry.
- **Live-reload schema** — services self-register over the gRPC control plane; new fields land without a gateway redeploy.
- **Tier-based versioning** — `unstable` / `stable` / `vN`; older `vN` auto-`@deprecated`; CI gate on schema diff.
- **HA out of the box** — embedded NATS + JetStream KV; any node dispatches to any service registered with any peer.
- **Pub/Sub primitive + honest streaming** — gateway-provided `ps.pub` / `ps.sub` for multi-listener channels (HMAC-gated, typed channel registry); service-declared `stream Resp` methods pass through as per-subscriber gRPC streams.
- **Backpressure that respects ownership** — per-pool + per-replica caps; slow service X can't gate calls to service Y.
- **Per-caller identity + quotas** — one extractor seam (Public / HMAC / Delegated / mTLS-via-proxy) feeds per-caller metrics; opt into block-permit quotas via a delegate.
- **Auth / logging as middleware** — one declaration applies across every protocol. `InjectType[T]` / `InjectPath` / `InjectHeader` for the "fill from context, hide from external schema" pattern.
- **Health / drain / metrics** — `/api/health` (200/503), `gw.Drain(ctx)` for rolling deploys, `/api/metrics` Prometheus.

## Cost

One Go binary or library import. Cluster is opt-in. Default dispatch
is reflection-based — any registered service works without a build
step; codegen and plugin paths layer on for extra throughput.

Per-request overhead at 1 k rps × 15 s, loopback (gateway adds on top
of a direct dial in the matching wire format):

| Ingress | Source | Δp50 | Δp95 |
|---|---|---|---|
| gRPC | proto upstream | +283 µs | +336 µs |
| HTTP/JSON | OpenAPI upstream | +208 µs | +245 µs |
| GraphQL | GraphQL upstream | +344 µs | +505 µs |

Each active middleware rule on the hot path adds ~15–20 µs at p50.
Full numbers + reproduce recipe: [`docs/perf.md`](./docs/perf.md).
Head-to-head vs alternatives: [`perf/`](./perf).

## Try it in 60 seconds

```bash
git clone https://github.com/iodesystems/gwag && cd gwag
cd examples/multi && ./run.sh        # gateway + greeter + library
```

In another terminal:

```bash
curl -s -X POST http://localhost:8080/api/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}'
# → {"data":{"greeter":{"hello":{"greeting":"Hello, world!"}}}}
```

For the full bench / Prometheus / Grafana stack: `bin/bench up`. For
the operator CLI: `gwag --help`.

## Library equivalent

```go
gw := gateway.New()
gw.AddProto("./protos/auth.proto",   gateway.To("authsvc:50051"))
gw.AddProto("./protos/user.proto",   gateway.To("usersvc:50051"))
gw.AddOpenAPI("./billing-openapi.json",
    gateway.As("billing"),
    gateway.To("https://billing.internal"))
http.ListenAndServe(":8080", gw.Handler())
```

Codegen typed S2S clients for any registered service:

```bash
curl https://gw.internal/api/schema/proto?service=billing > billing.fds
buf generate billing.fds                                  # proto stack

curl https://gw.internal/api/schema/openapi?service=billing > billing.json
openapi-generator-cli generate -i billing.json -g typescript-axios -o ./gen
```

## Embedded mode (`gat`)

Single Go binary, no NATS, no cluster — just want GraphQL + gRPC
typed surfaces on top of a [huma](https://huma.rocks/) service?
Use **`gat`** (GraphQL API Translator), gwag's embedded sibling.
One huma source of truth, three typed surfaces (REST + GraphQL +
gRPC) on one port:

```go
import "github.com/iodesystems/gwag/gw/gat"

g, _ := gat.New()
gat.Register(api, g, huma.Operation{ /* ... */ }, listProjects) // drop-in for huma.Register
gat.Register(api, g, huma.Operation{ /* ... */ }, getProject)
gat.RegisterHuma(api, g, "/api")     // /api/graphql + /api/schema/*
gat.RegisterGRPC(mux, g, "/api/grpc") // connect-go handlers
```

UI consumers use **graphql-codegen** off `/api/schema/graphql`;
service-to-service clients use **buf / ts-proto** off
`/api/schema/proto`; legacy REST integrations stay on huma's own
`/openapi.json`. No second schema, no second server, no extra
ports.

Concept doc: [`docs/gat.md`](./docs/gat.md). Runnable end-to-end
demo with React + Vite + graphql-codegen: [`examples/gat/`](./examples/gat/README.md).

## Compared to…

| | gwag | Apollo Federation | Hasura | Kong / Envoy |
|---|---|---|---|---|
| Single schema across services | ✓ | ✓ (GraphQL only) | ✓ (DB-centric) | ✗ (route-level only) |
| Ingest gRPC `.proto` natively | ✓ | ✗ | ✗ | partial |
| Ingest OpenAPI 3.x natively | ✓ | ✗ | ✗ | partial |
| Stitch downstream GraphQL | ✓ | ✓ (federation) | ✗ | ✗ |
| Re-emit proto / OpenAPI for typed S2S clients | ✓ | ✗ | ✗ | ✗ |
| Subscriptions out of the box | ✓ (NATS) | partial | ✓ (DB triggers) | ✗ |
| Runtime schema reload | ✓ (control plane) | restart | ✓ (DDL) | restart / config push |
| Tiered versioning + deprecation | ✓ (`unstable`/`stable`/`vN`) | manual | manual | manual |
| HA cluster | ✓ (embedded NATS / JetStream) | ✓ | ✓ | ✓ |
| Setup | one binary | gateway + N federated subgraphs | DB + Hasura | mesh + control plane |

Stitching covers the common case; **Apollo Federation**'s entity-merging
across services that share entity identity is a feature gwag doesn't
replicate (use Federation if you need it). **Hasura** wraps databases,
gwag wraps services. **Kong / Envoy** route bytes; gwag understands the
schema and produces typed clients.

Deeper breakdown vs service discovery, service meshes, and individual
gateways: [`docs/comparison.md`](./docs/comparison.md).

## Performance & the graphql-go fork

Throughput ceiling is the GraphQL executor itself. We maintain a
[graphql-go fork](https://github.com/iodesystems/graphql) with an
append-mode plan-cache executor that walks the plan straight to a JSON
byte buffer — projected **~3-4× end-to-end wedge** vs upstream
(`~430 µs / ~970 allocs → ~120 µs / ~200 allocs` on
`BenchmarkProtoSchemaExec`). Default builds use upstream graphql-go;
opt into the fork via a `go.mod` `replace` directive for the perf
upgrade.

Self-measurement (your hardware): [`docs/perf.md`](./docs/perf.md) —
escalating-target-RPS sweep with knee detection. Head-to-head vs peers
(graphql-mesh, Apollo Router): [`perf/`](./perf) — Docker-hermetic
matrix.

## Documentation

**Operations** — wiring the gateway into a real deployment:

- [`docs/lifecycle.md`](./docs/lifecycle.md) — register / version / deprecate / retire; tier model; CI gate
- [`docs/operations.md`](./docs/operations.md) — health, drain, backpressure, metrics
- [`docs/admin-auth.md`](./docs/admin-auth.md) — admin boot token + AdminAuthorizer delegate + outbound HTTP transport
- [`docs/caller-identity.md`](./docs/caller-identity.md) — per-caller ID extractor + quota
- [`docs/cluster.md`](./docs/cluster.md) — embedded NATS + JetStream KV
- [`docs/middleware.md`](./docs/middleware.md) — `Transform` / `InjectType` / `InjectPath` / `InjectHeader`

**Performance**:

- [`docs/perf.md`](./docs/perf.md) — throughput sweep on your hardware
- [`docs/comparison.md`](./docs/comparison.md) — gwag vs service discovery / mesh / Kong / Federation
- [`perf/`](./perf) — competitor matrix harness

**Maintainer**:

- [`docs/architecture.md`](./docs/architecture.md) — codebase layout, design notes, HTTP routing surface
- [`docs/plan.md`](./docs/plan.md) — roadmap + decisions log

**Pub/Sub & subscriptions** — gateway-provided `ps.pub` / `ps.sub`
primitives with HMAC auth tiers and a channel→type binding registry.
[`docs/pubsub.md`](./docs/pubsub.md) ships with the pre-1.0 workstream;
design + status: [`docs/plan.md`](./docs/plan.md) Tier 1.

## Roadmap

Active pre-1.0 workstreams are tracked in
[`docs/plan.md`](./docs/plan.md):

- **Pub/Sub as a first-class gateway primitive** (`ps.pub` / `ps.sub` +
  channel→type binding registry + auth tiers; replaces the implicit-
  channel transform; restores honesty to `stream Resp`).
- **Competitor performance matrix** (gwag vs graphql-mesh / Apollo
  Router; output → `perf/comparison.md`).

Open to (pulled in by a real use case): WSDL / SOAP ingest as a fourth
kind; multipart/form-data passthrough; static `--openapi` / `--graphql`
CLI flags; service-account / OAuth-JWT outbound auth helpers; opt-in
static codegen + plugin supervisor for native-speed dispatch.

Not planned: Apollo Federation entity-merging (stitching covers the
common case); AsyncAPI export (GraphQL Subscription types already cover
TS codegen).

## License

MIT. See [LICENSE](./LICENSE).
