# gwag

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**Register every service once, in the protocol it already speaks.
Get typed clients in *all three* — proto, OpenAPI, *and* GraphQL —
simultaneously, hot-reloading as services redeploy.**

`.proto` / OpenAPI 3.x / downstream GraphQL → one consolidated
GraphQL surface for browser & mobile, **plus** the same registry
re-emitted as proto FileDescriptorSet / OpenAPI 3.x / GraphQL SDL
for typed service-to-service clients in any language. Same auth,
same backpressure, same metrics across all three.

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

## The magic moment

One registration → three typed-client surfaces, simultaneously, off
the same live registry. Clone and run the multi-service example:

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

Now the wedge: the **same** `greeter` service — registered once, as
a `.proto` — re-emitted simultaneously as three typed-client surfaces.
Pick whichever codegen tool your client team already uses:

```bash
# GraphQL SDL → graphql-codegen (TS / React / Apollo Client / urql / …)
curl 'localhost:8080/api/schema/graphql?service=greeter' > greeter.graphql

# proto FileDescriptorSet → buf / ts-proto / grpc-python / grpc-go / grpc-java / …
curl 'localhost:8080/api/schema/proto?service=greeter' > greeter.fds
buf generate greeter.fds

# OpenAPI 3.x JSON → openapi-generator (40+ language targets)
# (Synthesized for proto-origin services; the gateway round-trips the IR through
# all three formats, so OpenAPI consumers get a valid spec even when the upstream
# service speaks gRPC.)
curl 'localhost:8080/api/schema/openapi?service=greeter' > greeter.json
openapi-generator-cli generate -i greeter.json -g typescript-axios -o ./gen
```

That's the wedge: **one** upstream registration, **three** client
ecosystems, **zero** schema duplication. Same story for OpenAPI- or
downstream-GraphQL-origin services — register in any of the three
formats, codegen in any (or all) of the three.

Edit a service or add a new one — the gateway's schema updates over
the control plane without a gateway restart. The next `pnpm run gen`
/ `buf generate` picks it up.

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

## Compared to similar tools

Pick the framing that matches what you actually need:

**Apollo Federation** is for entity-merging across services that
share entity identity — `User` defined in two services with
cross-references that resolve as one entity. If you have that
problem, use Federation; gwag doesn't replicate it. If you don't
(and most teams don't), federation's setup cost buys you nothing,
and `.proto` / OpenAPI are first-class ingest formats in gwag, not
something you bridge into a GraphQL-only world.
See [`docs/federation.md`](./docs/federation.md) for an honest
"do you need federation?" walkthrough.

**Hasura** wraps databases — point it at Postgres and get a GraphQL
surface auto-derived from your schema. gwag wraps services — point
it at a `.proto` / OpenAPI / GraphQL service and get a typed
surface over the wire protocol the service already speaks. Same
shape ("auto-generate clients"), opposite end of the stack.

**Kong / Envoy / service meshes** route bytes — they don't read
your schema and they don't generate clients. gwag reads the schema
and re-emits it in three formats. If your problem is "route HTTP
between pods" you want the mesh; if it's "give my browser /
mobile / service-to-service teams typed clients off one registry"
you want gwag.

**graphql-mesh / Apollo Router (single-subgraph mode)** are the
closest peers on the multi-format-ingest dimension. Head-to-head
performance comparison: [`perf/`](./perf).

Deeper breakdown vs service discovery, service meshes, and individual
gateways: [`docs/comparison.md`](./docs/comparison.md).

## Performance & the graphql-go fork

Throughput ceiling is the GraphQL executor itself. The gateway depends
on a [graphql-go fork](https://github.com/iodesystems/graphql) (currently
`v1.0.0`) — a hardened cut of `graphql-go/graphql` with extra plan-cache
+ subscription primitives plus an `ExecutePlanAppend` walker that emits
the response straight to a JSON byte buffer. Projected wedge once the
gateway swaps onto the append walker: **~3-4× end-to-end**
(`~430 µs / ~970 allocs → ~120 µs / ~200 allocs` on
`BenchmarkProtoSchemaExec`). Default gateway code path today calls the
older `ExecutePlan` + JSON-encoder pair; the swap is a follow-on perf
step, not a release gate.

Self-measurement (your hardware): [`docs/perf.md`](./docs/perf.md) —
escalating-target-RPS sweep with knee detection. Head-to-head vs peers
(graphql-mesh, Apollo Router): [`perf/`](./perf) — Docker-hermetic
matrix.

## Documentation

**Operations** — wiring the gateway into a real deployment:

- [`docs/lifecycle.md`](./docs/lifecycle.md) — register / version / deprecate / retire; tier model; CI gate
- [`docs/operations.md`](./docs/operations.md) — health, drain, backpressure, metrics
- [`docs/tracing.md`](./docs/tracing.md) — OpenTelemetry wiring + span reference
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
primitives with per-pattern auth tiers (`ChannelAuthOpen` / `HMAC` /
`Delegate`), a channel→type binding registry (proto-declarative *and*
runtime), and opt-in shape/coverage strictness knobs. Service-declared
`stream Resp` methods stay as honest per-subscriber gRPC streams.
Full surface, migration from the pre-1.0 implicit-channel transform,
and design notes: [`docs/pubsub.md`](./docs/pubsub.md).

## Roadmap

v1 release prep is the active focus. Tier 1 workstreams in priority
order — see [`docs/plan.md`](./docs/plan.md) for the punch list:

1. **The pitch** — this file + worked walkthrough + federation positioning doc.
2. **Public API audit + SemVer commitment** — lock the surface before v1 advertises stability.
3. **Wire-level identifier rename to `gwag-*`** — pre-1.0 freebie.
4. **File uploads** — graphql-multipart-request-spec + tus.io chunked uploads. See [`docs/uploads.md`](./docs/uploads.md).
5. **WebSocket connection-rate / per-IP caps** — DoS surface today.
6. **CHANGELOG + release versioning** — first stable tag.
7. **Competitor performance matrix** — ships *with* v1, doesn't gate it.

Shipped: **OpenTelemetry tracing** ([`docs/tracing.md`](./docs/tracing.md)).

Open to (pulled in by a real use case): WSDL / SOAP ingest as a
fourth kind; static `--openapi` / `--graphql` CLI flags;
service-account / OAuth-JWT outbound auth helpers; opt-in static
codegen + plugin supervisor for native-speed dispatch.

Not planned: Apollo Federation entity-merging (stitching covers the
common case); AsyncAPI export (GraphQL Subscription types already
cover TS codegen).

## License

MIT. See [LICENSE](./LICENSE).
