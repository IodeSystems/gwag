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
- **[`AGENTS.md`](./AGENTS.md)** — build, verify, proto generation, UI,
  run examples, binary, architecture essentials, test gotchas, bench,
  and don't-commit rules. See @AGENTS.md.
- **This file** — conventions and design decisions not covered by
  AGENTS.md.
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

See @AGENTS.md for build commands, verify steps, proto generation,
UI workflow, test gotchas, and bench commands.

Additional design decisions not in AGENTS.md:

- **AsyncAPI export was considered and dropped.** GraphQL SDL +
  Subscription types is the client-facing schema for events.
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
