# Changelog

Notable changes to gwag, conventional-commits-derived but human-edited.
One section per release; `Unreleased` at the top while main moves.

The version contract is in [`docs/stability.md`](./docs/stability.md):
SemVer over the symbols marked `// Stability: stable`, additive
changes on MINOR, drops on MAJOR.

## Unreleased

### Added
- Downstream MCP server ingestion — a fourth ingest kind alongside
  proto / OpenAPI / GraphQL. `gw.AddMCP(transport, target, opts...)`
  connects to an MCP server, runs `tools/list`, and re-exposes each
  tool as a GraphQL Mutation; `tools/call` dispatches at request
  time. Transports: `MCPStdio` (subprocess), `MCPHTTP` (Streamable
  HTTP), `MCPSSE`. CLI: `gwag --mcp-upstream NS:TRANSPORT:TARGET`
  (repeatable, also on `gwag up`). New public symbols `gw.AddMCP`,
  `gw.MCPTransport` + `MCPStdio` / `MCPHTTP` / `MCPSSE`, `ir.IngestMCP`,
  `ir.KindMCP`, `ir.MCPToolOrigin`. Tools-only — resources and
  prompts are not ingested. See [`docs/mcp.md`](./docs/mcp.md).
- `gwag --mcp` mounts the outbound MCP server at `/mcp` on the bare
  `gwag` command (previously only `gwag serve` / `gwag up`).
- Proto `bytes` field → `Upload` arg binding. Mark a `bytes` field
  with `[(gwag.upload.v1.upload) = true]` (extension declared at
  `gwag/upload/v1/options.proto`) and the gateway exposes it as a
  GraphQL `Upload` arg; the proto dispatcher reads the upload body
  (inline graphql-multipart-spec or tus-staged) into the field,
  capped by `WithUploadLimit`. Closes the proto-side gap in the
  upload story — OpenAPI `multipart/form-data` already mapped to
  `Upload`. New IR helper `ir.IsUploadField`; new public symbol
  `gw/proto/upload/v1.E_Upload`. Worked end-to-end in
  `gw/upload_proto_test.go`.
- `ir.AppendDispatcher` (gw/ir/dispatch.go) — optional capability
  interface for dispatchers that can emit their result as JSON bytes
  directly. The runtime renderer prefers `DispatchAppend` over
  `Dispatch` when a registered dispatcher implements it, routing
  through the fork's `ExecutePlanAppend` walker's `ResolveAppend`
  fast path (no per-field resolver tree, no full-tree map
  allocation, no leaf-emitter coercion round-trip). The built-in
  `protoDispatcher`, `openAPIDispatcher`, `graphQLDispatcher` (flat-
  upstream forwarder), and `graphqlGroupDispatcher` (nested-
  namespace forwarder) all implement it; the middleware chain
  (`backpressureMiddleware`, `quotaMiddleware`,
  `callerIDEnforceMiddleware`) passes the capability through. Adopters
  with BYO dispatchers (`gat.ServiceRegistration.Dispatchers`) opt in
  by implementing the method; plain `Dispatcher` continues to work
  unchanged.

  Measured impact (Ryzen 9 3900X, loopback, -benchtime=2s, single
  greeter / pets-style schema):

  | Bench | Before (Dispatch) | After (DispatchAppend) | Speedup |
  |---|---|---|---|
  | `BenchmarkProtoSchemaExec*`   | 436 µs / 68 KB / 937 allocs | 201 µs / 11.5 KB / 187 allocs | 2.2× / 5.9× / 5.0× |
  | `BenchmarkOpenAPISchemaExec*` | 391 µs / 68 KB / 937 allocs | 151 µs / 8.9 KB / 102 allocs | 2.6× / 7.6× / 9.2× |

  Fields whose return type contains a Union or Interface take the
  append path on graphql-origin (upstream resolves `__typename`,
  `prefixResponseTypenames` rewrites to the local prefix) and on
  openAPI-origin (the walker carries IR Service + type prefix to
  discriminate variants via `DiscriminatorProperty` / `Mapping`, then
  a required-fields heuristic, mirroring `IRTypeBuilder.unionFor`'s
  `ResolveType`). Proto-origin abstract returns stay on the Dispatch
  path until proto ingest synthesizes `TypeUnion` and the
  dispatcher carries the discriminator closure.

  New helper files: `gw/proto_append.go` (proto-message → JSON walker
  with selection projection + snake_case→camelCase renaming),
  `gw/openapi_append.go` (JSON-byte projector walking the local
  selection AST, type-aware for Union dispatch).
- `gwag serve --graphql URL [--mcp] [--mcp-include GLOB]`
  (gw/cmd/gwag/serve.go) fronts one downstream GraphQL service with
  the full gateway — metrics, backpressure, subscription proxy, and
  an optional `/mcp` mount. `--openapi` and `--proto` keep using the
  lighter gat translator. README "How to use it" lists all four
  shapes (standalone, gat, MCP exposure, CLI shortcut).
- `gw.WithMCPInclude(globs...)` / `gw.WithMCPExclude(globs...)` /
  `gw.WithMCPAutoInclude()` (gw/gateway.go) seed the MCP allowlist at
  construction. Operators declare which operations agents can call
  before any runtime admin edit, so the `/mcp` endpoint serves a
  meaningful surface from the first request without an out-of-band
  admin POST. Globs are dot-segmented (`*` matches one segment, `**`
  matches any number). Internal `_*` namespaces remain filtered first;
  neither Include nor AutoInclude can override that. Runtime
  `/api/admin/mcp/*` edits still override the seed.
- `gw.MountMCP(mux, opts...)` and `gateway.MCPPath(path)`
  (gw/mcp_server.go) wrap `MCPHandler()` in `AdminMiddleware` and
  register it on a `*http.ServeMux` in one call. Default path
  `/mcp`; override with `MCPPath`. Used by `gwag up` and the multi
  example so the standard gwag binary exposes MCP out of the box.
- `mcpIncludeRemove` and `mcpExcludeRemove` admin huma operations
  (`POST /api/admin/mcp/include/remove`, `POST /api/admin/mcp/exclude/remove`)
  — explicit removal sibling to the existing include / exclude
  endpoints. Needed so the UI can manage entries without overwriting
  the full list.
- `docs/mcp.md` — full MCP surface reference (tools, allowlist
  semantics, mounting, cluster behavior, runtime control). Linked
  from the README and from `docs/admin-auth.md`.
- `gat.RegisterHTTP(mux, g, prefix)` (gw/gat/http_register.go) — the
  huma-free counterpart of `RegisterHuma`. Mounts gat's four HTTP
  endpoints (`/graphql` + three schema views) on any `HandleMux` after
  `gat.New(regs...)` has built the gateway. `gwag serve` uses this
  path internally.
- `gwag serve` subcommand (gw/cmd/gwag/serve.go) boots the embedded
  gat translator against one upstream service described by an OpenAPI
  spec or `.proto` file. `gwag serve --openapi spec.yaml --to
  http://localhost:8081` or `gwag serve --proto greeter.proto --to
  localhost:50051`. No NATS, no admin, no cluster — for those use
  `gwag up`. `--addr`, `--prefix`, `--namespace`, `--version`
  overrides. Mounts `/graphql` + `/schema/{graphql,proto,openapi}`
  on a plain `http.ServeMux`. Doc: `docs/gat.md` "gwag serve — CLI
  shortcut".
- `gat.RegisterHTTP(mux, g, prefix)` (gw/gat/http_register.go) — the
  huma-free counterpart of `RegisterHuma`. Mounts gat's four HTTP
  endpoints (`/graphql` + three schema views) on any `HandleMux` after
  `gat.New(regs...)` has built the gateway. `gwag serve` uses this
  path internally.
- `gat.ProtoFile(path, target)` and `gat.ProtoSource(entry, body, imports, target)`
  (gw/gat/proto.go) compile a `.proto` via `protocompile`, ingest via
  `ir.IngestProto`, dial the target gRPC server, and return
  `[]ServiceRegistration` ready for `gat.New(...)`. The returned
  dispatchers do `grpc.ClientConn.Invoke` per call with `dynamicpb`
  marshal/unmarshal — no pool, no replicas, no backpressure (gat is
  the simple-start variant; pull full gwag for those). Default
  transport is insecure; mTLS or custom dials route through the
  existing `ServiceRegistration.Dispatchers` BYO path. Namespace and
  version derive from the proto package: `greeter.v1` →
  `greeter`/`v1` (trailing `vN` is the version), `pets` →
  `pets`/`v1`. Unary-only; server-streaming is not in gat's scope.
  Doc: `docs/gat.md` "Front a remote gRPC service from a .proto".
- `gw.WithWSLimit(gw.WSLimitOptions{MaxPerIP, RatePerSec, Burst, TrustedIPs})`
  (gw/ws_limit.go) caps the graphql-transport-ws Upgrade path per
  peer IP — a concurrent-connection semaphore plus a token-bucket
  rate limit on the handshake itself. Operators running gwag at the
  edge (no upstream reverse proxy / CDN terminating WebSockets) get
  the minimum DoS guard; an unset option leaves the upgrade path
  uncapped, preserving the pre-v1 behaviour. Rejected upgrades
  return HTTP 429 and increment
  `go_api_gateway_ws_rejected_total{reason}` with reason
  `max_per_ip` or `rate_limit`. Peer IP comes from `r.RemoteAddr`
  (port stripped); spoofable headers like `X-Forwarded-For` are
  not honoured — operators behind a trusted proxy pin its address
  in `TrustedIPs` and rely on the proxy for per-origin limits.
  Doc: `docs/operations.md` "WebSocket upgrade caps".
- `gw.WithTracer(trace.TracerProvider)` (gw/gateway.go) installs an
  OpenTelemetry TracerProvider for distributed tracing. Per-request
  server-kind spans land at every ingress shape: GraphQL
  (`gateway.graphql`), HTTP/JSON (`gateway.http`), gRPC
  (`gateway.grpc`), plus `.subscription` variants for SSE and gRPC
  server-streaming. Each upstream call opens a nested client-kind
  span (`gateway.dispatch.proto`, `gateway.dispatch.openapi`,
  `gateway.dispatch.graphql`, `gateway.dispatch.internal`,
  `gateway.dispatch.proto.subscription`) and injects W3C TraceContext
  on the outbound side — `traceparent` rides HTTP headers
  (OpenAPI / downstream GraphQL) and gRPC metadata (proto unary +
  server-streaming) so downstream services join the same trace. Spans
  carry the canonical `gateway.ingress` / `gateway.namespace` /
  `gateway.method` / `gateway.version` attribute set plus
  ingress-specific `http.*` / `rpc.*` keys from the OpenTelemetry
  semantic conventions. Inbound `traceparent` on the request is
  honoured so the gateway joins the caller's trace. When the option is
  unset, a noop tracer is wired and per-request cost stays near zero.
  Default builds depend on `go.opentelemetry.io/otel` directly.
- `Upload` GraphQL scalar (`gw.UploadScalar`, `*gw.Upload`) — exposed
  in every assembled schema so clients can declare `mutation
  ($f: Upload!)` against upload-capable fields. End-to-end multipart
  uploads via two complementary wire shapes: the
  graphql-multipart-request-spec parser handles inline single-request
  files at `POST /api/graphql`; tus.io v1.0 chunked uploads land at
  `gw.UploadsTusHandler()` (`/api/uploads/tus`) and the upload-id
  rides in GraphQL variables. The `Upload` scalar accepts both forms
  at `ParseValue`; the dispatcher opens the body lazily via
  `(*Upload).Open(ctx, store)` regardless of which wire shape
  delivered it.
- OpenAPI ingest recognises `multipart/form-data` request bodies and
  flattens `type: string, format: binary` schema properties into
  top-level `Upload!` (or `[Upload!]!`) GraphQL args. The
  `openAPIDispatcher` forwards as `multipart/form-data` upstream
  preserving the captured `Filename` and `Content-Type`. The
  gateway's re-exposed REST surface (`gw.IngressHandler`) accepts
  multipart inbound too. Proto-side binding deferred.
- `gw.UploadStore` interface (`Create` / `Append` / `Info` / `Open`
  / `Delete`) backing both wire shapes; default
  `gw.FilesystemUploadStore` impl with 24-hour TTL eviction.
- `gw.WithUploadStore(UploadStore)` plugs a custom store;
  `gw.WithUploadDataDir(string)` shorthand installs the default
  filesystem store (gateway owns lifecycle, `Close` shuts it down);
  `gw.WithUploadLimit(int64)` caps per-upload total bytes at both
  parsers.
- `docs/uploads.md` — adopter-facing docs covering both wire shapes,
  configuration knobs, error shapes, and outbound dispatch
  semantics.
- `docs/stability.md` — SemVer contract for 1.x. Defines what's
  locked, what's allowed to change, and what's experimental.
- `// Stability: stable` and `// Stability: experimental` godoc
  markers on every exported symbol in `gw/`, `gw/ir`, `gw/gat`,
  `gw/controlclient`. 187 stable + 14 experimental.
- `docs/api-audit.md` — pre-v1 export classification with per-symbol
  rationale.
- `CHANGELOG.md` (this file) and `RELEASE.md` (release process).
- `ir.OperationGroup.OriginKind` and `ir.OperationGroup.SchemaID`
  (gw/ir/ir.go) — group-level metadata so the runtime renderer can
  distinguish graphql-origin groups (which now install a single
  forwarding resolver at the top of the chain) from passthroughs.
  Group SchemaIDs use a `_group_<path>` suffix to avoid colliding
  with leaf Operation SchemaIDs. `PopulateSchemaIDs` populates them.

### Fixed
- `gw.AddGraphQL` against nested-namespace upstreams. Previously the
  per-leaf forwarder dropped the parent group chain: a local
  `{ ns { greeter { hello(...) } } }` produced an upstream
  `{ hello(...) }` that any sane GraphQL server rejects ("Cannot
  query field 'hello' on type 'Query'"). Now a single
  `graphqlGroupDispatcher` registers at each top-level graphql
  group; it captures the local sub-selection, forwards once with
  the container path intact (`{ greeter { hello(...) } }`), and a
  new `graphqlGroupChildResolver` demuxes the response map back
  into per-leaf field positions keyed by alias-or-name. Sibling
  leaves under one group ride a single upstream round trip —
  preserves GraphQL's batching across fields, no longer
  N round-trips. Aliases (including same-field-different-args, the
  case where aliasing is structurally required) round-trip
  correctly. Affected schemas: gwag-fronting-gwag, Hot Chocolate's
  grouped operations, some Apollo subgraph layouts. Pinned by
  `TestGraphQLIngest_NestedNamespaceForwarding`,
  `TestGraphQLIngest_DeepNestedNamespaceForwarding`,
  `TestGraphQLIngest_InlineFragmentInGroup`,
  `TestGraphQLIngest_GroupDispatcherIsolatedAcrossVersions`, and
  `TestGraphQLIngest_AliasInGroupSelection`.

### Changed
- OpenAPI namespace derivation now **preserves the spec title's
  case** — a spec titled `Pets` yields the GraphQL namespace `Pets`,
  not `pets`. Previously the library `AddOpenAPI` path preserved
  case while the `gat` translator and `gwag serve --openapi` lower-
  cased it, so the same spec produced a different schema depending
  on how it was registered. All three paths now route through one
  shared rule, `ir.SanitizeNamespace` (case preserved, spaces /
  dashes → `_`, leading digit guarded). This also fixes a latent
  bug where a digit-leading title produced an invalid GraphQL
  identifier.
- `gw.MCPConfig` and `gw.MCPHandler` promoted from `Stability:
  experimental` to `Stability: stable`. Same for the
  `/api/admin/mcp/*` huma operations. The four MCP tools
  (`schema_list` / `schema_search` / `schema_expand` / `query`) and
  the allowlist shape (AutoInclude / Include / Exclude with
  dot-segmented globs) are part of the 1.x contract.
- `gw.AddAdminEvents()` no longer requires a configured cluster — it
  registers the schema fragment unconditionally so SDL-driven codegen
  consumers pick up the `admin_events_watchServices` subscription
  field. Runtime subscriptions return a clean "subscription broker
  not available" error when no cluster resolves; cluster-mode
  behaviour is unchanged.
- Cluster-mode MCP allowlist: the first runtime mutation now merges
  into the local `WithMCPInclude(...)` seed instead of replacing it.
  Previously `tryMutateMCPConfig` started from `MCPConfig{}` when
  the JetStream KV bucket had no record, so the first
  `/api/admin/mcp/include` POST blew the seed away. The CAS path
  now falls back to the local snapshot on `ErrKeyNotFound`; after
  the first Put, KV stays authoritative as before.
- `gwag serve --mcp` now works with `--openapi` and `--proto`, not
  only `--graphql`. When `--mcp` (or `--mcp-include`) is set, the run
  promotes from the lite gat path onto the full `*gateway.Gateway`
  so `/mcp` shares dispatcher, plan cache, and metrics with every
  other ingress. The `/api/graphql` + `/api/schema/*` URL shape
  matches the existing `--graphql` flow; `--prefix` is ignored when
  `--mcp` is on.
- `examples/multi`: MCP mount moved from `/api/mcp` to `/mcp`, and
  the example seeds the allowlist with `WithMCPInclude("greeter.**",
  "library.**", "admin.**")`. Operators previously needed to POST to
  `/api/admin/mcp/include` before any tool returned useful results;
  the seed makes the demo work end-to-end on first boot.
- `gw/ir` SDL renderer emits one block-string per description (rather
  than one block per source line). Resolves a "consecutive
  triple-quoted strings" parse error from GraphQL lexers (including
  graphql-codegen) when descriptions contained blank or multi-line
  text. The rendered SDL is semantically equivalent.
- Wire-level identifiers renamed `go-api-gateway` → `gwag` across
  JetStream KV bucket names (`gwag-{registry,peers,stable,
  deprecated,mcp-config}`), the default NATS cluster name
  (`gwag`), the MCP server-info string returned by `MCPHandler`,
  and the UI's `localStorage` keys (`gwag:admin-token`,
  `gwag:admin-token-changed`). Pre-1.0 cleanup so the project
  ships with one consistent identifier.
- `github.com/IodeSystems/graphql-go` bumped 0.8.1 → 1.0.0 — first
  tagged release of the fork. `ExecutePlanAppend` walker available;
  gateway-side wiring is a follow-on perf step.

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
  plugin paths are roadmap, not shipped.
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
- `gat.New()` + `gat.Register{,Huma,HTTP,GRPC}` ship a single-server
  in-process translator that turns huma operations into both a
  GraphQL surface and connect-go gRPC endpoints. No NATS, no
  admin, no MCP — the minimum-cost entry for a huma app that wants
  typed multi-format clients.
- Proto ingest via `gat.ProtoFile` / `gat.ProtoSource` and the
  `gwag serve` subcommand for single-upstream embedded boot.
- Package classified `experimental`; surface may shift on a minor
  release.

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
