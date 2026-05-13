# Changelog

Notable changes to gwag, conventional-commits-derived but human-edited.
One section per release; `Unreleased` at the top while main moves.

The version contract is in [`docs/stability.md`](./docs/stability.md):
SemVer over the symbols marked `// Stability: stable`, additive
changes on MINOR, drops on MAJOR.

## Unreleased

### Added
- `Upload` GraphQL scalar (`gw.UploadScalar`, `*gw.Upload`) — exposed
  in every assembled schema so clients can declare `mutation
  ($f: Upload!)` against upload-capable fields. The
  graphql-multipart-request-spec wire format (operations / map / file
  parts) is parsed at the GraphQL HTTP ingress; file parts substitute
  into the variables tree before plan execution. Field-level binding
  for OpenAPI `format: binary` and proto `bytes` lands with the
  outbound-dispatch follow-on.
- `docs/stability.md` — SemVer contract for 1.x. Defines what's
  locked, what's allowed to change, and what's experimental.
- `// Stability: stable` and `// Stability: experimental` godoc
  markers on every exported symbol in `gw/`, `gw/ir`, `gw/gat`,
  `gw/controlclient`. 187 stable + 14 experimental.
- `docs/api-audit.md` — pre-v1 export classification with per-symbol
  rationale.
- `CHANGELOG.md` (this file) and `RELEASE.md` (release process).

### Changed
- Wire-level identifiers renamed `go-api-gateway` → `gwag` across
  JetStream KV bucket names (`gwag-{registry,peers,stable,
  deprecated,mcp-config}`), the default NATS cluster name
  (`gwag`), the MCP server-info string returned by `MCPHandler`,
  and the UI's `localStorage` keys (`gwag:admin-token`,
  `gwag:admin-token-changed`). Pre-1.0 cleanup so the project
  ships with one consistent identifier.
- `github.com/IodeSystems/graphql-go` bumped 0.8.1 → 1.0.0 — first
  tagged release of the fork. `ExecutePlanAppend` walker available
  (gateway-side wiring is a Tier 2.5 follow-on).

### Removed (pre-1.0 cleanup; no SemVer contract yet)
- `gw/`: unexported every admin-internal helper that escaped:
  - Stats / history wire types (`StatsSnapshot`, `MethodStatsSnapshot`,
    `HistoryBucket`, `ServiceHistory`, `SubjectInfo`) and their
    methods (`Snapshot`, `History`, `ActiveSubjects`).
  - MCP admin wire types (`Schema{Expand,List,Search}*`,
    `MCP{QueryInput,ResponseWithEvents,EventsBundle,ChannelEvent}`)
    and admin-only methods (`MCPAllows`, `MCPConfigSnapshot`,
    `MCPQuery`, `MCPSchema{Expand,List,Search}`, `SetMCPConfig`).
    `MCPHandler` stays public (the canonical wiring uses it).
  - Injector inventory types (`InjectorEntry`, `InjectorFrame`,
    `InjectorLanding`, `InjectorRecord`, `InjectorKind`, `InjectorState`
    + their consts) and the `InjectorInventory()` method.
  - `BackpressureConfig` + `BackpressureMiddleware` (internal
    plumbing; `BackpressureOptions` + `WithBackpressure` remain
    the public surface).
  - `Hide{Type,Path}Rewrite` / `Nullable{Type,Path}Rewrite` —
    concrete `SchemaRewrite` impls type-asserted inside `gw/` only.
  - `HeaderInjector` and `Transform.{Headers,Inventory}` — `Transform`
    is now opaque; construct via `InjectHeader` / `InjectType` /
    `InjectPath`.
  - `InternalProto{,Subscription}Handler` callback types.
  - `With{Admin,HTTP}Auth`, `IsAdminAuth`, `WithHTTPRequest` — set
    on ctx inside the gateway, never called from adopter code.
- `gw/`: dropped IR re-export shims (`RenderGraphQLRuntime`,
  `RenderGraphQLRuntimeFields`, `RuntimeOptions`, `IRTypeBuilder`,
  `IRTypeBuilderOptions`, `IRTypeNaming`, `NewIRTypeBuilder`).
  Callers import `gw/ir` directly.
- `gw/gat`: dropped six dead exports (`Option`, `config`, `As`,
  `Version`, `ServiceOption`, `ServiceConfig`) — declared but never
  applied anywhere. Per-registration knobs would go on
  `ServiceRegistration` as plain fields if/when needed.

## v0 — pre-1.0 development

Capsule history of what landed before the 1.0 surface lock. Granular
log lives in `git log`; this is the orientation for adopters reading
the changelog cold.

### Ingest pipeline
- **Proto ingest** via `protocompile` with `SourceInfoStandard` so
  comments survive into SDL + the MCP corpus. Three entry points:
  `AddProto(path)`, `AddProtoFS(fs, entry)`, `AddProtoBytes(entry, body)`.
- **OpenAPI 3.x ingest** via `kin-openapi`. Mirror entry points:
  `AddOpenAPI(spec)`, `AddOpenAPIBytes(spec)`.
- **GraphQL stitching** via boot-time introspection with a
  namespace-prefixed type mirror: `AddGraphQL(endpoint)`.
- Symmetric raw-source wire format across the control plane:
  `ServiceBinding.proto_source` + `proto_imports`, `openapi_spec`.
  The compiled-FileDescriptorSet path was retired pre-1.0.

### Schema rebuild + IR
- Format-agnostic intermediate representation (`gw/ir`) that lifts
  proto / OpenAPI / GraphQL into a common shape. Ingest happens once
  at registration; schema rebuild reads `slot.ir` directly.
- `RenderGraphQL{,Runtime,RuntimeFields}` for client-facing SDL,
  `RenderProtoFiles` for FDS export, `RenderOpenAPI` for the
  OpenAPI re-emit. Each format honours `FlatOperations` if the
  upstream format doesn't natively support nesting.
- Auto-internal namespaces: any namespace prefixed `_` is hidden
  from the public schema (used internally for admin / quota /
  pubsub-auth in-process services).
- Tier model: `unstable` (single overwrite slot), `vN` (locked
  once registered; differing schema-hash → `AlreadyExists`),
  `stable` (computed alias to highest-ever `vN`, monotonic; only
  `RetractStable` walks it back). `--allow-tier` per-deploy policy
  replaces the older `--environment` knob.

### Dispatch
- Reflection-based unary dispatch is the canonical path. Codegen +
  plugin paths are roadmap (Tier 2.5).
- Per-pool backpressure (`BackpressureOptions`) with separate
  unary in-flight caps, per-replica caps, and gateway-wide stream
  caps. No gateway-wide unary cap — pools are the isolation primitive.
- Outbound transport pluggability: `WithOpenAPIClient(c)` / per-
  source `OpenAPIClient(c)` for HTTP; `LoadMTLSConfig` + `WithTLS`
  for mTLS on outbound gRPC.

### Pub/sub
- `gwag.ps.v1.PubSub` proto rides NATS for multi-listener fan-out
  with channel-named topics and a typed payload registry.
- Server-streaming gRPC at egress: each subscriber opens its own
  upstream stream. The earlier "auto-transform every `stream Resp`
  into a NATS subscription" path was retired pre-1.0 — the schema
  no longer lies about an upstream stream that didn't open.
- HMAC + per-pattern auth tiers (`WithChannelAuth(pattern, tier)`):
  `ChannelAuthOpen`, `ChannelAuthHMAC`, `ChannelAuthDelegate`.
  Strictest-tier-wins for wildcard subs. Sign-side: bearer-gated
  `SignSubscribeToken{,WithKid}`.

### Caller identity
- Three extractor styles via `WithCallerID{Public,HMAC,Delegated}`
  + the `WithCallerIDExtractor` seam for custom shapes.
- `WithCallerIDMetricsTopK` caps Prometheus label cardinality;
  overflow folds into the `OtherCallerID = "__other__"` bucket.
- Kid-aware HMAC rotation via `SignCallerIDTokenWithKid`.

### Admin surface
- Operations defined via huma (`gw/admin_huma.go`), mounted at
  `/admin/*`, and self-ingested as OpenAPI so the SDL gains nested
  `Query.admin.*` / `Mutation.admin.*` fields. The same path any
  external huma service takes.
- `AdminMiddleware`: bearer-gated mutations, public reads. Pluggable
  `AdminAuthorizer` delegate registered under `_admin_auth/v1`;
  boot token underneath as the always-works hatch.
- `WithAdminDataDir` persists the boot token under
  `<dir>/admin-token`; pair with `gwag login`/`use`/`context` for
  multi-cluster credential management.

### Multi-gateway clustering
- Embedded NATS via `StartCluster(ClusterOptions)`. JetStream KV
  buckets hold the registry, peer roster, stable map, deprecation
  state, and MCP config.
- Reconcilers on every node sync local slot state from KV watches —
  proto, OpenAPI, and GraphQL bindings ride in the same value
  shape; `slot.kind` picks the install / remove path.
- The gateway publishes its own service-change events on
  `admin_events_watchServices` so UIs can react to pool churn
  without polling.

### Embedded translator (`gw/gat`)
- `gat.New()` + `gat.Register{,Huma,GRPC}` ship a single-server
  in-process translator that turns huma operations into both a
  GraphQL surface and connect-go gRPC endpoints. No NATS, no
  admin, no MCP — the "minimum-cost entry" for a huma app that
  wants typed multi-format clients.
- Tier 2 work outstanding: proto ingest, the `gwag serve`
  subcommand; package classified `experimental` until both land.

### MCP integration
- `MCPHandler()` mounts an MCP-streamable HTTP endpoint with four
  tools: `schema_list`, `schema_search`, `schema_expand`,
  `gateway_query`. Operator-curated allowlist lives in the
  cluster `gwag-mcp-config` KV bucket; `SetMCPConfig` writes
  through.

### Operability
- Prometheus metrics under the `gateway_*` namespace: dispatch
  histograms, queue depth (per-pool + per-replica), backpressure
  rejects, admin-auth outcomes, caller-id label budgets, MCP
  audit counters.
- `WithPprof` mounts the runtime profiler under
  `AdminMiddleware`.
- Health endpoint (`HealthHandler`) flips to 503 during
  `Drain(ctx)`. Graceful drain waits for in-flight unary +
  active subscriptions before close.
- `WithRequestLog(io.Writer)` opt-in JSON-per-request sink with
  total time, self time, and per-dispatch counts.

### Benchmarking
- `bench/` self-perf harness with `bench traffic` (escalating-
  target-RPS sweep) + `bench perf` (knee detection, p95/p99
  reports → `docs/perf.md`).
- `perf/` competitor matrix scaffolding for head-to-head against
  graphql-mesh, Apollo Router, WunderGraph. Hermetic Docker
  build; scenarios declared in `perf/competitors.yaml`.

### Tooling / DX
- `gwag` binary (`gw/cmd/gwag`) with subcommands: `up` (zero-config
  NATS + admin + UI), `peer {list,forget}`, `services list`,
  `schema {fetch,diff}`, `sign`, `login`, `logout`, `context`, `use`.
- Embedded React + MUI v6 admin console at `ui/` with
  TanStack Router. `ui/embed.go` bundles the built SPA into the
  Go binary.
- `bin/build` single-entry build wrapper handling UI codegen +
  Vite build + Go build with fallback seeding so
  `go:embed all:dist` never breaks an unprepared tree.

### Won't fix (called out intentionally)
- **No Apollo Federation.** Stitching is correct for the common
  case; federation's entity-merging is overkill for most teams.
  See `docs/federation.md`.
- **No AsyncAPI export.** GraphQL SDL with Subscription types
  covers TS codegen; AsyncAPI's TS tooling is patchier.
- **One Register call = one address contributing to N pools, not
  N independent bindings.** Bindings share lifetime. Run two
  binaries for independent lifecycles.
