# gwag

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

A schema gateway for Go services. Ingest gRPC, OpenAPI, or GraphQL;
emit all three as typed client surfaces. Tier-versioned with a CI
gate on breaking changes; client teams keep their existing codegen
tools.

Two configurations:

- **`gat`** — embed in a Go binary. Your huma OpenAPI service serves
  REST as before; gat adds GraphQL and gRPC client surfaces from the
  same handlers. No second schema, no second process.
- **`gwag`** — out-of-process gateway. The seam when a monolith
  outgrows its singleton: register multiple services together,
  clustered via embedded NATS, while pieces migrate out at their own
  pace.

Start on gat. Graduate to gwag when a piece of the monolith needs
to split.

## What it gives you

Beyond the lead — the pieces that need their own mechanics paragraph:

- **Tier-based versioning with a CI gate.** `unstable` / `stable` /
  `vN`; older `vN` is auto-`@deprecated`; CI fails the build on a
  breaking schema diff. The live registry rebuilds on every register
  / deregister, so codegen picks up changes without a server restart.
  The same diff applies to the proto FDS and OpenAPI exports — one
  breaking change fails the build for every client team that consumes
  any of the three. See [`docs/lifecycle.md`](./docs/lifecycle.md).

- **Transforms and middleware.** Auth, header injection, quota
  enforcement, field reshaping — one declaration applied across every
  protocol edge. ~15–20 µs per active rule on the hot path.
  `InjectType[T]` / `InjectPath` / `InjectHeader` for the "fill from
  context, hide from external schema" pattern.

- **Typed pub/sub on the embedded NATS.** The cluster already runs
  NATS + JetStream; `ps.pub` / `ps.sub` expose typed multi-listener
  channels with HMAC auth. No separate broker to run.

## How to use it

Four shapes, same wire surface — pick the one that matches your
deployment:

- **Standalone gwag — API gateway, aggregator, translator.** One
  binary fronting many services. Runtime registration over the gRPC
  control plane; HA via embedded NATS; admin UI; subscriptions.
  Start here if you have a fleet. Jump to
  [Quickstart](#unified-cross-format-apis).
- **Embedded gat — in-process GraphQL API translator.** One Go
  binary serving its own [huma](https://huma.rocks/) operations as
  REST + GraphQL + gRPC on one port. No NATS, no cluster, no admin.
  Start here if you have one binary and want typed clients without
  standing up a separate gateway process. Jump to
  [Embedded mode (`gat`)](#embedded-mode-gat).
- **MCP exposure — let LLM agents query the gateway directly.**
  `gw.MountMCP(mux)` wraps the gateway as four MCP tools
  (`schema_list` / `schema_search` / `schema_expand` / `query`) on
  `/mcp`. Operator-curated allowlist; AdminMiddleware-gated. See
  [`docs/mcp.md`](./docs/mcp.md).
- **CLI shortcut — typed surfaces + metrics over one upstream.**
  `gwag serve --openapi spec.yaml --to URL`,
  `gwag serve --proto file.proto --to HOST:PORT`, or
  `gwag serve --graphql URL` exposes any single upstream as all
  three typed surfaces. `--graphql` routes through the full
  gateway (metrics, backpressure, subscription proxy, optional
  `--mcp`); `--openapi` / `--proto` use the lite gat path.

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

**`gat`** (GraphQL API Translator) is gwag's in-process variant
for the single-binary case. GraphQL + gRPC typed surfaces on top
of a [huma](https://huma.rocks/) service, no NATS, no cluster.
REST + GraphQL + gRPC on one port:

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
- **[gqlgen](https://github.com/99designs/gqlgen)** — Go GraphQL
  server framework. You write SDL, gqlgen generates resolver stubs,
  you implement them. Different layer: gqlgen builds one Go service's
  GraphQL surface; gwag composes existing services and runs on top
  of graphql-go. Directive support is narrow — `@deprecated` only;
  the runtime side of cross-cutting concerns lives in transforms and
  providers. See [`docs/directives.md`](./docs/directives.md).

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
- [`docs/directives.md`](./docs/directives.md) — `@deprecated` rendering; rationale for not carrying custom directives
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

Open to (pulled in by a real use case):

- **AddMCP ingest** — register a downstream MCP server as a fourth
  ingest kind. Each upstream tool becomes an IR operation, surfacing
  through GraphQL / proto / OpenAPI clients alongside everything
  else. Two frictions to size first: most MCP tools don't ship an
  `outputSchema` (falls back to a `JSON` scalar passthrough), and
  `tools/listChanged` notifications need a refresh story.
- **WSDL / SOAP ingest.** Corporate legacy services that can't be
  rewritten.
- **Service-account / OAuth-JWT outbound auth helpers.** Composable
  today; first-class when an adopter pulls.

Not planned: Apollo Federation entity-merging (stitching covers
the common case); AsyncAPI export.

## License

MIT. See [LICENSE](./LICENSE).
