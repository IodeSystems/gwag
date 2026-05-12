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

- **[`README.md`](./README.md)** — adopter-facing pitch.
- **This file** — maintainer rules + conventions + how to build/run.
- **[`docs/architecture.md`](./docs/architecture.md)** — file-by-file
  layout, design notes, HTTP surface. Read once when you join.
- **[`docs/plan.md`](./docs/plan.md)** if it exists locally — maintainer
  working doc (gitignored). Read at session start; surface conflicts
  before changing code.

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

## Layout (one-line)

Top-level: `gw/` (library) · `ui/` (React admin) · `bench/` (self-perf) · `perf/` (competitor matrix) · `examples/` · `docs/` · `bin/`.

Full file-by-file layout, test inventory, and per-package roles:
[`docs/architecture.md`](./docs/architecture.md). Read it once on
session start if you're new to the codebase; it has every load-bearing
file named with its responsibility.

Importers use `gateway "github.com/iodesystems/gwag/gw"` for the
library and `github.com/iodesystems/gwag/gw/proto/...` for the
generated bindings.

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
  (`AddProto`, `AddProtoBytes`, `AddProtoFS`, `AddOpenAPI`,
  `AddOpenAPIBytes`, `AddGraphQL`). Available options: `To`, `As`,
  `Version` (`unstable` or `vN` per plan §4 — empty defaults to
  `v1`), `AsInternal`, `ForwardHeaders` (HTTP header allowlist),
  `OpenAPIClient` (per-source `*http.Client`), `ProtoImports`
  (multi-file proto import map).
- **Proto ingest is raw-source only.** Both `AddProtoBytes` (in-memory
  bytes) and `AddProto` / `AddProtoFS` (disk / fs.FS) drive the same
  `protocompile` pipeline with `SourceInfoStandard`, so leading /
  trailing comments survive into the GraphQL SDL and the MCP search
  corpus. The control plane wire ships raw `.proto` bytes
  (`proto_source` + `proto_imports` map) — same shape as
  `openapi_spec`. The earlier "ship a compiled FileDescriptorSet"
  path was retired in favor of symmetry with OpenAPI ingest. Adopters
  using `controlclient.SelfRegister` set `ProtoSource []byte` (or
  `ProtoFS fs.FS` + `ProtoEntry string` for multi-file).

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

Routes table + auth model + library-constraint-vs-example-wiring
distinction: [`docs/architecture.md#http-routing-surface`](./docs/architecture.md#http-routing-surface).

## When in doubt

- Adopter / user question → [`README.md`](./README.md) for the pitch;
  [`docs/`](./docs) for the deep dive (lifecycle, operations, auth,
  caller-id, middleware, cluster, comparison).
- Maintainer question → [`docs/architecture.md`](./docs/architecture.md)
  for layout + design notes + HTTP surface;
  [`docs/plan.md`](./docs/plan.md) for the roadmap + decisions log
  (gitignored — may or may not be present locally; surface conflicts
  before changing code).
