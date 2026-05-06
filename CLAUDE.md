# go-api-gateway

A Go library + binary fronting gRPC and OpenAPI services with a
GraphQL surface. Drop in a `.proto` or OpenAPI spec, get a typed
GraphQL endpoint. Multi-gateway clustering via embedded NATS;
subscriptions via NATS pub/sub with HMAC channel auth.

## Read first

- **`docs/plan.md`** — authoritative roadmap, decisions log, and
  recently-shipped tail. Read it at session start. If a choice
  contradicts what's there, surface it before changing code.
- **`README.md`** — user-facing overview of the library API.
- **This file** — how the codebase is laid out and what's load-bearing.

## Architecture in five lines

1. Services register `.proto` files (or OpenAPI specs) via control
   plane gRPC at runtime, or boot-time via `AddProto`/`AddOpenAPI`.
2. Each `(namespace, version)` is a *pool*. Multiple replicas can
   join a pool only if proto hashes match (canonicalized SHA-256).
3. The schema is rebuilt on every pool create/destroy. SDL +
   introspection + transformed FDS exposed at
   `/schema/{graphql,proto,openapi}`.
4. Multi-gateway clusters share the registry via JetStream KV;
   reconcilers on every node sync local pool state from KV watch.
5. Server-streaming RPCs become GraphQL subscription fields backed
   by NATS pub/sub. HMAC verify on subscribe; delegate (registered
   under `_events_auth/v1`) at sign time.

## Layout

```
gateway.go               Top-level Gateway, Options, http handlers
pools.go                 Pool, replica, descriptor hashing (canonical)
schema.go                GraphQL assembly: rootFields, query/subscription
control.go               gRPC control plane: Register/Heartbeat/Deregister
                         + Sign/List/Forget admin RPCs
controlplane/v1/         Control plane proto + generated bindings
eventsauth/v1/           SubscriptionAuthorizer delegate proto
admin_huma.go            huma admin routes; self-ingested via OpenAPI
                         to surface as admin_* GraphQL fields (dogfood)
peers.go                 Peers KV bucket + monotonic R bump
reconciler.go            Watches registry KV, syncs local pool state
broker.go                Sub-fanout: shared NATS subs across N WebSockets
subscriptions.go         graphql-ws WS lifecycle + schema-time wiring
auth_subscribe.go        HMAC verify + SubscribeAuthCode
auth_delegate.go         Calls _events_auth/v1 at SignSubscriptionToken
auth_admin.go            Boot-token gen/persist + AdminMiddleware bearer
metrics.go               Prometheus dispatch + dwell + backoff + queue
                         + stream + auth gauges/histograms/counters
health.go                /health endpoint + Drain method
sdl.go                   Runtime graphql.Schema → SDL printer
proto_export.go          /schema/proto + /schema/openapi
                         (transformed FDS + ingested OpenAPI re-emit)
openapi.go               OpenAPI ingestion (file/URL + AddOpenAPIBytes)
                         → GraphQL fields, HTTP dispatch
cluster.go               Embedded NATS server + JetStream
hide_inject.go           HideAndInject middleware Pair
convert.go               Proto descriptor → GraphQL type builder
loader.go                Proto file parsing
controlclient/           Service-side: SelfRegister + heartbeat goroutine
cmd/go-api-gateway/      Binary: gateway runner + peer/services/schema/sign
                         subcommands + diff.go (SDL diff)
examples/multi/          greeter + library + run.sh + run-cluster.sh
docs/plan.md             Authoritative roadmap & decisions
ui/                      React + MUI + TanStack Router admin UI; consumes
                         GraphQL only via graphql-codegen-typed SDK
```

## Conventions

- `go vet ./...` after every edit.
- **Test coverage is a thin seed.** `auth_admin_test.go` covers
  AdminMiddleware + header forwarding; everything else is still manual
  via the example binaries. Growing this is tier-1 in plan.md — when
  adding a feature, add tests in the same shape (httptest, no NATS).
- Generated code: `controlplane/v1/control.pb.go`,
  `eventsauth/v1/eventsauth.pb.go`, `examples/multi/gen/**`. Never
  edit; regenerate with protoc (`PATH=".bin:$PATH" protoc --go_out=...`).
- **Per-pool isolation is sacred.** Anything that introduces a
  gateway-wide cap on unary dispatches (beyond per-pool `MaxInflight`)
  is wrong — see decisions log.
- **Subscriptions = NATS pub/sub.** Server-streaming gRPC dispatch
  intentionally not implemented; services publish to NATS subjects.
- **AsyncAPI export was considered and dropped.** GraphQL SDL +
  Subscription types is the client-facing schema for events.
- **Proto/gRPC is canonical for service-to-service.** GraphQL is the
  client-facing surface; OpenAPI ingestion is a bridge for legacy
  HTTP services.
- **Admin auth ≠ service auth.** The boot-token model in plan.md is
  *only* for the gateway's own admin endpoints. It does not
  authenticate services calling each other through the gateway, and
  it has nothing to do with outbound auth to OpenAPI services. Two
  separate concerns; keep them separate.
- **Dogfood the OpenAPI path.** Admin operations live in
  `admin_huma.go`, defined via huma → OpenAPI → self-ingested by the
  gateway → surfaced as `admin_*` GraphQL fields. Same path any
  external huma service takes. Use this as the template when adding
  new admin operations.
- `ServiceOption` (`To`, `As`, `AsInternal`) applies to `AddProto`,
  `AddProtoDescriptor`, `AddOpenAPI`, and `AddOpenAPIBytes`.

## How to build/run

```bash
go build ./...
go vet ./...

# single-gateway example (greeter + library)
cd examples/multi && ./run.sh

# 3-gateway cluster
cd examples/multi && ./run-cluster.sh

# the binary
go-api-gateway --proto path/to/foo.proto=foo-svc:50051 --addr :8080

# operator subcommands
go-api-gateway peer list     --gateway gw:50090
go-api-gateway peer forget   --gateway gw:50090 NODE_ID
go-api-gateway services list --gateway gw:50090
go-api-gateway schema fetch  --endpoint https://gw/schema
go-api-gateway schema diff   --from URL --to URL --strict
go-api-gateway sign          --gateway gw:50090 --channel events.X --ttl 60
```

## HTTP surface

| Path | Auth | What |
|---|---|---|
| `/graphql` (queries, subscriptions) | public | GraphQL queries + WebSocket upgrade for subscriptions |
| `/graphql` (mutations) | bearer (transitive) | admin\_\* dispatch through /admin/\* — operator must send `Authorization: Bearer <token>` to /graphql; the gateway forwards it on outbound dispatch |
| `/schema`, `/schema/graphql` | public | SDL (or `?format=json` for introspection) |
| `/schema/proto?service=...` | public | FileDescriptorSet (transformed) |
| `/schema/openapi?service=...` | public | Re-emit ingested OpenAPI specs |
| `/admin/*` reads (GET) | public | huma reads (peers, services list) |
| `/admin/*` writes | bearer | huma mutations (forget, sign) |
| `/health` | public | JSON status; 503 when `Drain()` is in flight |
| `/metrics` | public (or behind reverse-proxy auth) | Prometheus scrape |

The bearer is the gateway's boot token (logged at startup, persisted
to `<adminDataDir>/admin-token` if `WithAdminDataDir(...)` is set).
Boot-token model is the gateway's own emergency hatch — it does not
authenticate services calling each other through the gateway, and it
has nothing to do with outbound auth to OpenAPI backends (which is a
separate tier-1 in plan.md). The pluggable admin authorizer
delegate is still tier-1; boot token is the always-works fallback.

## When in doubt

Read `docs/plan.md`. The decisions log explains *why* things are the
way they are. If a request seems to contradict that, name the decision
and ask before reshaping it.
