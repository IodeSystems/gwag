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
peers.go                 Peers KV bucket + monotonic R bump
reconciler.go            Watches registry KV, syncs local pool state
broker.go                Sub-fanout: shared NATS subs across N WebSockets
subscriptions.go         graphql-ws WS lifecycle + schema-time wiring
auth_subscribe.go        HMAC verify + SubscribeAuthCode
auth_delegate.go         Calls _events_auth/v1 at SignSubscriptionToken
metrics.go               Prometheus dispatch + dwell + backoff + queue
                         + stream + auth gauges/histograms/counters
health.go                /health endpoint + Drain method
sdl.go                   Runtime graphql.Schema → SDL printer
proto_export.go          /schema/proto + /schema/openapi
                         (transformed FDS + ingested OpenAPI re-emit)
openapi.go               OpenAPI ingestion → GraphQL fields, HTTP dispatch
cluster.go               Embedded NATS server + JetStream
hide_inject.go           HideAndInject middleware Pair
convert.go               Proto descriptor → GraphQL type builder
loader.go                Proto file parsing
controlclient/           Service-side: SelfRegister + heartbeat goroutine
cmd/go-api-gateway/      Binary: gateway runner + peer/services/schema/sign
                         subcommands + diff.go (SDL diff)
examples/multi/          greeter + library + run.sh + run-cluster.sh
docs/plan.md             Authoritative roadmap & decisions
ui/                      React/MUI/TanStack-Router admin UI
```

## Conventions

- `go vet ./...` after every edit.
- **Zero automated tests today.** Tier-1 followup in plan.md. Manual
  verification via the example binaries.
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
- `ServiceOption` (`To`, `As`, `AsInternal`) applies to both
  `AddProto` and `AddOpenAPI`.

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

| Path | What |
|---|---|
| `/graphql` | GraphQL (POST queries; WebSocket upgrade for subscriptions) |
| `/schema`, `/schema/graphql` | SDL (or `?format=json` for introspection) |
| `/schema/proto?service=ns:ver,...` | FileDescriptorSet (transformed) |
| `/schema/openapi?service=ns,...` | Re-emit ingested OpenAPI specs |
| `/health` | JSON status; 200 normally, 503 when `Drain()` is in flight |
| `/metrics` | Prometheus scrape |

## When in doubt

Read `docs/plan.md`. The decisions log explains *why* things are the
way they are. If a request seems to contradict that, name the decision
and ask before reshaping it.
