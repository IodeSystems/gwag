# gwag

A Go library + binary that fronts three kinds of upstream services
under a single typed GraphQL surface:

- **gRPC services** described by `.proto`, registered via
  `AddProto[Descriptor]` or the gRPC control plane.
- **OpenAPI 3.x services** described by a JSON/YAML spec, registered
  via `AddOpenAPI[Bytes]` or the same control plane (proto field
  `ServiceBinding.openapi_spec`).
- **Existing GraphQL services** stitched in via `AddGraphQL`
  (boot-time introspection + namespace-prefixed type mirror).

Multi-gateway clustering via embedded NATS; subscriptions via NATS
pub/sub with HMAC channel auth.

## Read first

- **`docs/plan.md`** — authoritative roadmap, decisions log, and
  recently-shipped tail. Read it at session start. If a choice
  contradicts what's there, surface it before changing code.
- **`README.md`** — user-facing overview of the library API.
- **This file** — how the codebase is laid out and what's load-bearing.

## Architecture in five lines

1. Services register at runtime via control plane gRPC (proto **or**
   OpenAPI bindings) or boot-time via `AddProto` / `AddOpenAPI` /
   `AddGraphQL`.
2. Every registration occupies one *slot* per `(namespace, version)`
   in `g.slots`, kind-tagged (proto / openapi / graphql). The slot
   owns the per-kind dispatch handle (`slot.proto` is a `pool` with
   replicas, sems, FileDescriptor; `slot.openapi` is an
   `openAPISource`; `slot.graphql` is a `graphQLSource`) and the
   request-ready IR (`slot.ir`) baked at registration time. Multi-
   replica adds against the slot; cross-kind collision rejects;
   `unstable` swaps; `vN` is locked. `registerSlotLocked` is the
   single tier-policy decision site.
3. The schema is rebuilt on every slot create/destroy. Schema
   rebuild is one iteration over `g.slots` reading `slot.ir`
   (already post-transform, post-internal-filter, schema-IDs
   populated); the per-kind ingest happens once at registration
   only. SDL + introspection + transformed FDS exposed at
   `/schema/{graphql,proto,openapi}`.
4. Multi-gateway clusters share the registry via JetStream KV;
   reconcilers on every node sync local slot state from a KV
   watch (proto, OpenAPI, and GraphQL bindings ride in the same
   value shape; `slot.kind` picks the install / remove path).
5. Server-streaming RPCs become GraphQL subscription fields backed
   by NATS pub/sub. HMAC verify on subscribe; sign-side gate is the
   admin/boot token plus optional `WithSignerSecret` (gRPC peer calls
   only — in-process bypasses). The gateway also publishes its own
   service-change events on `admin_events_watchServices`.

## Layout

Top-level repo:

```
README.md / CLAUDE.md / LICENSE
go.mod / go.sum
gw/                  Gateway library (package gateway) + subpkgs
ui/                  React + MUI + TanStack Router admin UI; consumes
                     GraphQL only via graphql-codegen-typed SDK
bench/               Local benchmark + demo stack (compose, scripts,
                     cmd/traffic). bin/bench dispatches everything.
examples/            Example services that register against the gateway
docs/                plan.md (roadmap + decisions log) + design notes
bin/                 Top-level shell scripts (build, bench)
.bin/                Pinned tooling (protoc-gen-go-grpc etc.)
```

Inside `gw/` (package `gateway` unless noted):

```
gw/
  gateway.go               Top-level Gateway, Options, http handlers
  slot.go                  Per-(ns,ver) slot index + tier policy:
                           registerSlotLocked, evictSlotLocked,
                           bakeSlotIRLocked, protoSlot/openAPISlot/
                           graphQLSlot accessors, collectSlotIRLocked
  pools.go                 Pool, replica, descriptor hashing (canonical
                           — slot.proto holds an instance per slot)
  schema.go                GraphQL assembly: rootFields, query/subscription
  control.go               gRPC control plane: Register/Heartbeat/Deregister
                           + Sign/List/Forget admin RPCs
  admin_huma.go            huma admin routes (peers, services, forget,
                           sign, channels, drain, openapi.json);
                           self-ingested via OpenAPI to surface as
                           admin_* GraphQL fields (dogfood)
  peers.go                 Peers KV bucket + monotonic R bump
  reconciler.go            Watches registry KV, syncs local slot state
  broker.go                Sub-fanout: shared NATS subs across N WebSockets
  subscriptions.go         graphql-ws WS lifecycle + schema-time wiring
  auth_subscribe.go        HMAC verify + SubscribeAuthCode
  auth_signer.go           Bearer gate on cp.SignSubscriptionToken
  auth_delegate_deprecation.go  One-time-per-(ns,ver) deprecation log
                           when a service registers under _events_auth
  auth_admin.go            Boot-token gen/persist + AdminMiddleware
  auth_admin_delegate.go   Calls _admin_auth/v1 from AdminMiddleware
  admin_events.go          AddAdminEvents() + publishServiceChange (NATS)
  ui_handler.go            UIHandler: fs.FS → http.Handler with SPA fallback
  metrics.go               Prometheus dispatch + dwell + backoff + queue
                           + stream + auth gauges/histograms/counters
  health.go                /health endpoint + Drain method
  sdl.go                   Runtime graphql.Schema → SDL printer
  proto_export.go          /schema/proto + /schema/openapi
                           (transformed FDS + ingested OpenAPI re-emit)
  openapi.go               OpenAPI ingestion (file/URL + AddOpenAPIBytes)
                           → GraphQL fields, HTTP dispatch
  graphql_ingest.go        Downstream GraphQL ingest API (AddGraphQL)
  graphql_introspect.go    Canonical introspection query + parser
  graphql_mirror.go        Type mirror with namespace prefix +
                           forwarding resolver
  cluster.go               Embedded NATS server + JetStream
  inject.go                InjectType / InjectPath / InjectHeader →
                           Transform (schema + runtime + headers)
  inject_inventory.go      InjectPath dormant↔active state machine
  convert.go               Proto descriptor → GraphQL type builder
  loader.go                Proto file parsing
  proto/                   Generated proto bindings:
    controlplane/v1/       Control plane proto + generated bindings
    eventsauth/v1/         SubscriptionAuthorizer delegate proto
                           (parked — runtime path removed plan §2.3,
                           generated code kept one release for
                           importers)
    adminauth/v1/          AdminAuthorizer delegate proto
    adminevents/v1/        AdminEvents (service-change stream) proto
  controlclient/           Service-side: SelfRegister + heartbeat goroutine
  cmd/gwag/      Binary: gateway runner + peer/services/schema/sign
                           subcommands + diff.go (SDL diff)

  # Tests live next to their code (~70 cases; ~22s wall clock for the
  # cluster-heavy ones).
  admin_events_test.go        admin_events publish path (NATS subscriber)
  admin_huma_test.go          channels / drain / openapi.json routes
  auth_admin_test.go          AdminMiddleware + token store + metrics
  auth_admin_delegate_test.go AdminAuthorizer delegate fall-through
  cluster_dispatch_test.go    Two-node cluster cross-gateway dispatch
  dynamic_openapi_test.go     ServiceBinding.openapi_spec path
                              (standalone + cluster + multi-replica)
  forget_peer_test.go         peers KV manipulation; alive vs expired
  graphql_ingest_test.go      AddGraphQL: prefix mirror + forwarding
  grpc_dispatch_test.go       In-process grpc.Server + GraphQL → gRPC
  openapi_test.go             Httptest backend + GraphQL → HTTP/JSON
  schema_rebuild_test.go      Slot create/destroy + hash collision
                              + unstable swap + cross-kind reject
  slot_test.go                registerSlotLocked policy table:
                              fresh / idempotent / unstable swap /
                              vN reject (kind, hash, caps)
  subscriptions_test.go       Embedded NATS + WebSocket round-trip
```

Importers use `gateway "github.com/iodesystems/go-api-gateway/gw"`
for the library and `github.com/iodesystems/go-api-gateway/gw/proto/...`
for the generated bindings.

## Conventions

- `go vet ./...` after every edit. `go test ./.` after touching
  load-bearing code (cluster tests run ~10s each, suite ~22s).
- **Tests follow a fixture pattern.** Httptest for OpenAPI/GraphQL
  forwarding; in-process `grpc.Server` for gRPC; `StartCluster`
  with `127.0.0.1:0` for cluster/subscription tests; in-process
  `grpc.ClientConnInterface` fakes for delegate testing. Lifetime
  gotcha: `startClusterTracking` captures its ctx as the parent of
  the watch + reconciler goroutines — pass `context.Background()`
  in test helpers, not a `WithTimeout`, or the goroutines die mid-
  test. (See `cluster_dispatch_test.go` comment.)
- Generated code: `gw/proto/controlplane/v1/control.pb.go`,
  `gw/proto/eventsauth/v1/eventsauth.pb.go`, `gw/proto/adminauth/v1/adminauth.pb.go`,
  `gw/proto/adminevents/v1/adminevents.pb.go`, `examples/multi/gen/**`. Never
  edit; regenerate with protoc (`PATH=".bin:$PATH" protoc --go_out=...`).
- **Per-pool isolation is sacred.** Anything that introduces a
  gateway-wide cap on unary dispatches (beyond per-pool `MaxInflight`)
  is wrong — see decisions log.
- **Subscriptions = NATS pub/sub.** Server-streaming gRPC dispatch
  intentionally not implemented; services publish to NATS subjects.
- **AsyncAPI export was considered and dropped.** GraphQL SDL +
  Subscription types is the client-facing schema for events.
- **Proto/gRPC is canonical for service-to-service.** GraphQL is the
  client-facing surface; OpenAPI and downstream-GraphQL ingestion are
  bridges for legacy / external services that don't speak gRPC.
- **Admin auth ≠ service auth.** The boot-token + AdminAuthorizer
  delegate model is *only* for the gateway's own admin endpoints. It
  does not authenticate services calling each other through the
  gateway, and it has nothing to do with outbound auth to upstream
  services (which is `WithOpenAPIClient` / `OpenAPIClient(c)` /
  `ForwardHeaders`). Three separate concerns; keep them separate.
- **Auto-internal `_*` namespaces.** Any namespace starting with `_`
  is hidden from the public schema regardless of whether
  `AsInternal()` was passed. `_events_auth`, `_admin_auth`,
  `_admin_events`, etc. — operators don't have to remember the flag.
- **Dogfood the OpenAPI path.** Admin operations live in
  `admin_huma.go`, defined via huma → OpenAPI → self-ingested by the
  gateway → surfaced as `admin_*` GraphQL fields. Same path any
  external huma service takes. Use this as the template when adding
  new admin operations.
- **`ServiceOption`** applies to every registration entry point
  (`AddProto`, `AddProtoDescriptor`, `AddOpenAPI`, `AddOpenAPIBytes`,
  `AddGraphQL`). Available options: `To`, `As`, `Version` (`unstable`
  or `vN` per plan §4 — empty defaults to `v1`), `AsInternal`,
  `ForwardHeaders` (HTTP header allowlist), `OpenAPIClient`
  (per-source `*http.Client`).

## How to build/run

```bash
go build ./...
go vet ./...

# single-gateway example (greeter + library)
cd examples/multi && ./run.sh

# 3-gateway cluster
cd examples/multi && ./run-cluster.sh

# the binary
gwag --proto path/to/foo.proto=foo-svc:50051 --addr :8080

# operator subcommands
gwag peer list     --gateway gw:50090
gwag peer forget   --gateway gw:50090 NODE_ID
gwag services list --gateway gw:50090
gwag schema fetch  --endpoint https://gw/schema
gwag schema diff   --from URL --to URL --strict
gwag sign          --gateway gw:50090 --channel events.X --ttl 60
```

## HTTP surface

The example gateway splits routes by prefix: `/api/*` is the
gateway, everything else is the embedded UI bundle. Unmatched
`/api/*` returns JSON 404 (so a typo doesn't render the SPA);
non-API requests fall back to the SPA's `index.html` for client-
side routing. Pattern lifted from zdx-go.

| Path | Auth | What |
|---|---|---|
| `/api/graphql` (queries, subscriptions) | public | GraphQL + WebSocket upgrade for subscriptions |
| `/api/graphql` (mutations) | bearer (transitive) | admin\_\* dispatch through /api/admin/\* — operator sends `Authorization: Bearer <token>` to /api/graphql; the gateway forwards it on outbound dispatch |
| `/api/schema`, `/api/schema/graphql` | public | SDL (or `?format=json` for introspection) |
| `/api/schema/proto?service=...` | public | FileDescriptorSet (transformed) |
| `/api/schema/openapi?service=...` | public | Re-emit ingested OpenAPI specs |
| `/api/admin/*` reads (GET) | public | huma reads (peers, services list) |
| `/api/admin/*` writes | bearer | huma mutations (forget, sign) |
| `/api/health` | public | JSON status; 503 when `Drain()` is in flight |
| `/api/metrics` | public (or behind reverse-proxy auth) | Prometheus scrape |
| `/api/...` (unmatched) | n/a | JSON 404 |
| `/`, `/{anything}` | public | Embedded SPA bundle from `ui/dist/`; index.html SPA fallback for unknown paths |

The bearer is the gateway's boot token (logged at startup, persisted
to `<adminDataDir>/admin-token` if `WithAdminDataDir(...)` is set).
The pluggable AdminAuthorizer delegate (registered under
`_admin_auth/v1`) is consulted first; the boot token is the
always-works emergency hatch underneath. Outbound auth to upstream
services is a separate concern: `WithOpenAPIClient` /
`OpenAPIClient(c)` / `ForwardHeaders`.

The `/api/*` split is an example wiring choice, not a library
constraint. `gateway.UIHandler(fs.FS)` and the per-handler primitives
(`gw.Handler()`, `gw.SchemaHandler()`, `gw.AdminMiddleware(...)`)
let operators arrange the routes however they like. The
`gw/cmd/gwag` CLI mounts GraphQL at `/` directly when no UI
is in play.

## When in doubt

Read `docs/plan.md`. The decisions log explains *why* things are the
way they are. If a request seems to contradict that, name the decision
and ask before reshaping it.
