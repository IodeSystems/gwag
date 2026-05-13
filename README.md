# gwag

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

GraphQL as it was meant to be — multi-dispatch API composition,
fewer client round trips, less egress.

Register a service once in the protocol it already speaks. Get
typed clients in proto, OpenAPI, and GraphQL off the same registry,
hot-reloading as services redeploy.

`.proto` / OpenAPI 3.x / downstream GraphQL in. One consolidated
GraphQL surface for browser and mobile; the same registry re-emitted
as proto FileDescriptorSet / OpenAPI 3.x / GraphQL SDL for typed
service-to-service clients. Same auth, backpressure, metrics across
all three.

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

## Why

- **One service, three typed-client surfaces.** TS uses
  graphql-codegen, Go uses `buf`, Python uses `openapi-generator` —
  all off one live registry.
- **Compose across services in one round trip.** Query auth + billing
  + inventory in a single GraphQL document; the gateway dispatches
  each part to the right backend and shapes one response.
- **Hot reload, no restart.** Services self-register over the
  control plane; the schema rebuilds. The next codegen run picks
  up the change.
- **Same auth, backpressure, and metrics across every protocol.**
  One declaration, applies to proto + OpenAPI + GraphQL traffic.

## How to use it

Two shapes, same wire surface:

- **Standalone gwag** — one binary fronting many services. Runtime
  registration over the gRPC control plane; HA via embedded NATS;
  admin UI; subscriptions. Start here if you have a fleet. Jump to
  [Quickstart](#unified-cross-format-apis) below.
- **Embedded gat** — one Go binary serving its own
  [huma](https://huma.rocks/) operations as REST + GraphQL + gRPC
  on one port. No NATS, no cluster, no admin endpoints. Start here
  if you have one binary and want typed clients without standing
  up a separate gateway process. Jump to [Embedded mode (`gat`)](#embedded-mode-gat).

## Unified cross-format APIs

One registration, three typed-client surfaces, off one live registry.
Clone and run the multi-service example:

```bash
git clone https://github.com/iodesystems/gwag && cd gwag
cd examples/multi && ./run.sh        # gateway + greeter + library (both proto)
```

One consolidated GraphQL surface for the browser:

```bash
curl -s -X POST localhost:8080/api/graphql \
  -d '{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}'
# → {"data":{"greeter":{"hello":{"greeting":"Hello, world!"}}}}
```

Same `greeter` service, registered once as a `.proto`, re-emitted
as three typed-client surfaces. Pick whichever codegen tool your
client team already uses:

```bash
# GraphQL SDL → graphql-codegen (TS / React / Apollo Client / urql / …)
curl 'localhost:8080/api/schema/graphql?service=greeter' > greeter.graphql

# proto FileDescriptorSet → buf / ts-proto / grpc-python / grpc-go / grpc-java / …
curl 'localhost:8080/api/schema/proto?service=greeter' > greeter.fds
buf generate greeter.fds

# OpenAPI 3.x JSON → openapi-generator (40+ language targets).
# Synthesized for proto-origin services: the gateway round-trips the
# IR through all three formats, so OpenAPI consumers get a valid spec
# even when the upstream speaks gRPC.
curl 'localhost:8080/api/schema/openapi?service=greeter' > greeter.json
openapi-generator-cli generate -i greeter.json -g typescript-axios -o ./gen
```

One registration; three client ecosystems; no duplicated schema.
Edit or add a service — the gateway updates over the control plane
without a restart. The next `pnpm run gen` / `buf generate` picks it
up.

Worked walk-through (three services, three client languages,
edit-redeploy-codegen cycle): [`docs/walkthrough.md`](./docs/walkthrough.md).

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

Services can also self-register over the gRPC control plane — no
gateway restart, no static config edit. Same `/api/schema/*`
endpoints reflect it immediately.

## What else you get

- **Tier-based versioning** — `unstable` / `stable` / `vN`; older `vN` auto-`@deprecated`; CI gate on schema diff.
- **HA out of the box** — embedded NATS + JetStream KV; any node dispatches to any service registered with any peer.
- **Pub/Sub primitive + honest streaming** — gateway-provided `ps.pub` / `ps.sub` for multi-listener channels (HMAC-gated, typed channel registry); service-declared `stream Resp` methods pass through as per-subscriber gRPC streams.
- **Backpressure that respects ownership** — per-pool + per-replica caps; slow service X can't gate calls to service Y.
- **Per-caller identity + quotas** — one extractor seam (Public / HMAC / Delegated / mTLS-via-proxy) feeds per-caller metrics; opt into block-permit quotas via a delegate.
- **Auth / logging as middleware** — one declaration applies across every protocol. `InjectType[T]` / `InjectPath` / `InjectHeader` for the "fill from context, hide from external schema" pattern.
- **MCP for LLM agents** — `gw.MCPHandler()` exposes four tools (`schema_list` / `schema_search` / `schema_expand` / `query`) over MCP Streamable HTTP. Mark which operations agents see with `WithMCPInclude("greeter.**", "library.**")`.
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
Head-to-head vs graphql-mesh + Apollo Router on the same backends:
[`perf/comparison.md`](./perf/comparison.md) (harness + reproduce
recipe: [`perf/`](./perf)).

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

## MCP — agents query the gateway directly

LLM agents speak Model Context Protocol. `gw.MountMCP(mux)` exposes
the gateway as four tools (`schema_list` / `schema_search` /
`schema_expand` / `query`) on `/mcp`, bearer-gated, backed by the
same in-process executor every other ingress hits. Seed which
operations agents see at construction:

```go
gw := gateway.New(gateway.WithMCPInclude("greeter.**", "library.**"))
gw.MountMCP(mux)
```

Full surface — tool shapes, allowlist semantics, cluster behavior,
runtime control: [`docs/mcp.md`](./docs/mcp.md). Worked example
with a client driver: [`examples/multi/cmd/mcp-demo`](./examples/multi/cmd/mcp-demo/main.go).

## Compared to similar tools

- **Apollo Federation** — entity-merging across services that share
  entity identity. gwag stitches by namespace; if you actually need
  entity-merging, use Federation. See [`docs/federation.md`](./docs/federation.md).
- **Hasura** — wraps databases. gwag wraps services. Same shape,
  opposite end of the stack.
- **Kong / Envoy / service meshes** — route bytes; don't read
  schemas or emit clients.
- **graphql-mesh / Apollo Router (single-subgraph mode)** — closest
  peers on multi-format ingest. Head-to-head numbers:
  [`perf/comparison.md`](./perf/comparison.md).

Deeper breakdown: [`docs/comparison.md`](./docs/comparison.md).

## Performance

Throughput ceiling is the GraphQL executor itself. The gateway uses
a [graphql-go fork](https://github.com/iodesystems/graphql) with
plan-cache and subscription primitives; an append-mode executor that
emits JSON straight to a buffer is the next perf lever (in flight,
not gating any release).

Self-measurement: [`docs/perf.md`](./docs/perf.md). Head-to-head vs
peers: [`perf/comparison.md`](./perf/comparison.md) (harness:
[`perf/`](./perf)).

## Documentation

**Operations** — wiring the gateway into a real deployment:

- [`docs/lifecycle.md`](./docs/lifecycle.md) — register / version / deprecate / retire; tier model; CI gate
- [`docs/operations.md`](./docs/operations.md) — health, drain, backpressure, metrics
- [`docs/tracing.md`](./docs/tracing.md) — OpenTelemetry wiring + span reference
- [`docs/admin-auth.md`](./docs/admin-auth.md) — admin boot token + AdminAuthorizer delegate + outbound HTTP transport
- [`docs/caller-identity.md`](./docs/caller-identity.md) — per-caller ID extractor + quota
- [`docs/cluster.md`](./docs/cluster.md) — embedded NATS + JetStream KV
- [`docs/middleware.md`](./docs/middleware.md) — `Transform` / `InjectType` / `InjectPath` / `InjectHeader`
- [`docs/mcp.md`](./docs/mcp.md) — exposing the gateway to LLM agents (tools, allowlist, mounting, cluster semantics)

**Performance**:

- [`docs/perf.md`](./docs/perf.md) — throughput sweep on your hardware
- [`docs/comparison.md`](./docs/comparison.md) — gwag vs service discovery / mesh / Kong / Federation
- [`perf/comparison.md`](./perf/comparison.md) — head-to-head numbers vs graphql-mesh + Apollo Router
- [`perf/`](./perf) — competitor matrix harness (Dockerfile + orchestrator)

**Pub/Sub & subscriptions** — `ps.pub` / `ps.sub` primitives with
per-pattern auth (`ChannelAuthOpen` / `HMAC` / `Delegate`) and a
channel→type binding registry. Service-declared `stream Resp`
methods stay as per-subscriber gRPC streams. See
[`docs/pubsub.md`](./docs/pubsub.md).

**Stability + release**:

- [`docs/stability.md`](./docs/stability.md) — SemVer contract
- [`CHANGELOG.md`](./CHANGELOG.md) — public-surface delta
- [`RELEASE.md`](./RELEASE.md) — cut a release

**Maintainer**:

- [`docs/architecture.md`](./docs/architecture.md) — codebase layout, design notes, HTTP routing surface

## Roadmap

v1 surface is locked in. SemVer contract:
[`docs/stability.md`](./docs/stability.md). Public-surface delta:
[`CHANGELOG.md`](./CHANGELOG.md).

Before the 1.0 tag:

- **Wire-level identifier rename.** Prometheus metrics still prefixed
  `go_api_gateway_*`; proto packages still `gateway.*` (except
  `gwag.ps.v1`). Renaming is a SemVer break post-1.0, so it happens
  before the tag.

After 1.0:

- **Append-mode executor wiring.** The [graphql-go fork](https://github.com/iodesystems/graphql)
  exposes `ExecutePlanAppend`; gateway-side swap is a single-function
  change projected at ~3-4× end-to-end on the hot path.
- **Static codegen + plugin supervisor.** Opt-in native-speed
  dispatch on top of reflection. Layers on; default stays reflection.

Open to: WSDL / SOAP ingest; service-account / OAuth-JWT outbound
auth helpers; static `--openapi` / `--graphql` flags on the full
gateway (gat already covers single-upstream via `gwag serve`).

Not planned: Apollo Federation entity-merging (stitching covers
the common case); AsyncAPI export.

## License

MIT. See [LICENSE](./LICENSE).
