# go-api-gateway: roadmap & decisions

Source of truth for open work, design decisions, and the rationale
behind both. Read at session start. Update whenever scope shifts.

Each work item follows the same shape: **the push** (one paragraph
on why this exists and where it sits), **done** (commits / facts
that are settled), **todo** (commit-sized chunks), and **followups**
(things noticed mid-flight that don't block the push).

Tier 1 = correctness / production-blocking. Tier 2 = design-completing
features. Tier 3 = operational polish. Known limitations = called out
intentionally; not currently planned to fix.

---

## Tier 1 — load-bearing (production-blocking)

**Empty.** Inbound admin auth, outbound auth v1, and every
load-bearing glue surface (admin / OpenAPI / gRPC unary /
subscriptions / schema rebuild / ForgetPeer / cross-gateway
dispatch) have e2e tests.

If a new production-blocking item shows up, file it here. Daily
work draws from tier 2.

---

## Tier 2 — design-completing

### IR runtime cutover (active workstream)

**The push.** Make IR the default — every code path
(ingest, runtime, export, transforms) goes through `gw/ir` instead
of the per-format converters in `gw/{convert,openapi,graphql_mirror,schema}.go`.
Schema EXPORT already flows through IR (`/api/schema/openapi`,
`/api/schema/proto`, cross-kind synthesis: ingest → transform →
render); the runtime path (`g.schema` + `/api/graphql` dispatch +
`/api/schema/graphql`) and the inline copies of
`Hides` / `HideInternal` / `Filter` in `buildSchemaLocked` are the
remaining holdouts. Once IR is canonical, transformers compose at
one layer instead of two and we can benchmark + optimize the
runtime: dispatchers built from IR should be allocation- and
cache-friendly enough to approach hand-written / code-generated
handlers (open question whether we get there with reflection +
struct-of-handlers, or whether per-schema codegen is worth it).

**Done.**
- [x] IR types — `Service` / `Operation` / `Type` / `Field` as superset
- [x] Proto ingest + render with round-trip via Origin shortcut
- [x] OpenAPI + GraphQL ingest / render with round-trips
- [x] Transforms (`Filter`, `HideInternal`, `Hides`) + cross-kind render tests
- [x] `/api/schema/openapi` + `/api/schema/proto` wired through IR
- [x] Nested namespaces — `Service.Groups` for graphql round-trip
- [x] `SchemaID` on IR + `Dispatcher` interface + `DispatchRegistry` (`gw/ir/dispatch.go`); `gatewayServicesAsIR` stamps SchemaIDs once Namespace/Version are assigned
- [x] `BackpressureMiddleware` (`gw/backpressure.go`) wrapping an `ir.Dispatcher` — slot acquire + dwell + queue depth + backoff metric, returns `Reject(CodeResourceExhausted)` on timeout
- [x] `protoDispatcher` (`gw/proto_dispatcher.go`) — `buildPoolMethodField` resolver is now `dispatcher.Dispatch(rp.Context, rp.Args)`; user runtime middleware chain still wraps the proto-shaped Handler inside the dispatcher
- [x] `openAPIDispatcher` (`gw/openapi_dispatcher.go`) — same shape; OpenAPI field resolver is a one-liner over the wrapped dispatcher
- [x] `graphQLDispatcher` (`gw/graphql_dispatcher.go`) — same shape; ResolveInfo (selection-set + variables) plumbed into the dispatcher via `withGraphQLForwardInfo` context value because canonical args alone can't reconstruct an upstream query. Three inline backpressure copies are now gone.
- [x] `g.dispatchers *ir.DispatchRegistry` plumbed through every field builder; resolver closures look up by SchemaID instead of capturing dispatcher pointers. Registry is rebuilt fresh on every `assembleLocked` (stale entries can't leak across rebuilds). Tests in `dispatch_registry_test.go` pin the contract.
- [x] Dispatch benchmark harness (`gw/dispatch_bench_test.go`) — proto + OpenAPI dispatchers measured directly via the registry; baseline ~186µs/178 allocs (proto) and ~131µs/86 allocs (OpenAPI) on loopback. graphql-mirror omitted because the forwarder needs a full ResolveInfo (selection-set + variables) that isn't stubbable.
- [x] IR type-builder library (`gw/ir_typebuilder.go`) — single `*ir.Service` → `graphql.{Object,InputObject,Enum,Union,Scalar}` builder with a pluggable `IRTypeNaming` policy and `IRTypeBuilderOptions` for int64/uint64/map projection. Covers Object/Input/Enum/Union/Interface/Scalar plus list / non-null / item-required wrapping; recursive refs share the same `*graphql.Object` via `FieldsThunk`. Tests in `gw/ir_typebuilder_test.go` cover scalars, recursion, naming overrides, the OpenAPI Long-scalar override path, OpenAPI oneOf → graphql Union cross-format, and end-to-end `graphql.NewSchema` validation.
- [x] OpenAPI ingest captures top-level `oneOf` / `anyOf` as `TypeUnion` entries with named `Variants`; `RenderOpenAPI` round-trips unions back to `oneOf` schemas under `components.schemas` (cross-kind synthesis path). Discriminator metadata still rides on `Origin` for the same-kind shortcut.
- [x] `ir.Hides` extended to strip `Operation.Args` (top-level + nested under `Groups`). Proto's flatten-input-message-into-Args path was bypassing the hide policy; `HideAndInject` would have leaked the hidden type's args into the public schema once the wiring step lands.
- [x] Canonical `DiscriminatorProperty` + `DiscriminatorMapping` on `ir.Type` (Union kind only). `IngestOpenAPI` populates from `schema.discriminator`; `RenderOpenAPI` round-trips on the synthesis path; `IRTypeBuilder.unionFor` consults the mapping (then identity fallback, then `__typename`) before giving up. Removes the previous "discriminator survives only via Origin" caveat.
- [x] Inline (anonymous) `oneOf` / `anyOf` at OpenAPI field positions: `synthesizeInlineUnion` registers a deterministic `<A>Or<B>`-named `TypeUnion` in `svc.Types` and the field's `TypeRef` points at it. Anonymous variants (no `$ref`) still fall through to the scalar fallback — IR has no name-synthesis story for those yet.
- [x] `IngestProto` walks `fd.Imports()` transitively so cross-file message refs (e.g. `user.proto` returning `auth.v1.Context`) land in the IR Types map. Necessary for the IR-driven type-builder wiring to resolve cross-file refs without falling through to the descriptor graph.
- [x] **Proto path wired through `IRTypeBuilder`.** `gw/proto_typebuilder.go` builds one shared `*IRTypeBuilder` over the merged proto Types from every pool (proto FullNames are globally unique, so a single Types map is collision-free); `buildPoolMethodField` and `buildSubscriptionField` resolve outputs via `protoTB.Output(ir.TypeRef{Named: ...})`. Args still walk `md.Fields()` (the flatten-input-message path) but route type lookup through `protoTB.Input(ir.TypeRef{Named: fd.Message().FullName()}, ...)` so cyclic message refs share a single `*graphql.Object` across Query/Subscription roots. The `tb.hidden(fd)` skip migrated to `protoMessageHidden` in the args walk because `ir.Hides` only filters `Type.Fields` — args bypass that path. Old `typeBuilder` / `policy` deleted from `gw/types.go` (kept `exportedName` / `lowerCamel` helpers).
- [x] **OpenAPI path wired through `IRTypeBuilder`.** `gw/openapi.go` constructs one `*IRTypeBuilder` per source (`newOpenAPISourceTypeBuilder`) with prefix-aware naming closures (`ObjectName`/`InputName`/etc. project `Pet` → `<ns>_Pet` or `<ns>_<vN>_Pet`). Long + JSON scalars are built once per schema build and shared across per-source builders via `IRTypeBuilderOptions{Int64Type, UInt64Type, MapType, JSONType}` because graphql-go forbids two scalars sharing a Name. `buildOpenAPIFields` walks IR Operations from `ir.IngestOpenAPI(src.doc)` instead of the kin-openapi path tree — IR is the type contract, the openapi3.Operation captured on `irOp.Origin` stays the wire contract for `dispatchOpenAPI`. Inline anonymous schemas (body / response / nested fields) are synthesized in IR ingest with deterministic names (`<OperationId>Body`, `<OperationId>Response`, `<ParentName><FieldName>`) so IRTypeBuilder lookups via `TypeRef.Named` resolve. `IRTypeBuilder.unionFor` extended with the legacy "first variant whose required fields are all present" heuristic; `ScalarUnknown` falls back to `Options.JSONType` when set so unmappable schemas (mixed-kind oneOf) still surface as the JSON scalar. Old `openAPITypeBuilder` + helpers (`schemaName`, `validGraphQLName`, `isRequired`, `primaryType`, `integerType`, `unionFromVariants`, etc.) deleted — ~440 lines of duplicated type-construction collapsed into the IR builder.
- [x] **GraphQL-ingest path wired through `IRTypeBuilder`.** `gw/graphql_mirror.go` now holds a per-source `*IRTypeBuilder` (built via `newGraphQLSourceTypeBuilder`) instead of its own `objects` / `inputs` / `enums` / `scalars` / `unions` caches. Type construction (Object/Input/Enum/Scalar/Union/Interface) all goes through the builder; the wrapper-walk in `outputType` / `inputType` and the inline-fragment AST rewriting (`unprefixTypeName`) stay because they're orthogonal to type identity. A new `introspectionToIRService` converter feeds the builder from the parsed `*introspectionSchema` — distinct from `ir.IngestGraphQL` because the runtime mirror needs every named type the introspection declares (no namespace-classification pruning). One JSON scalar is shared across every per-source mirror in a schema build (graphql-go's duplicate-Name rejection). New `IRTypeNaming.EnumValueValue` callback — graphql-ingest passes a closure returning `EnumValue.Name` (string) so upstream "ADMIN" coerces correctly through graphql-go's enum coercion, while proto callers keep the default `EnumValue.Number`.
- [x] **Step 3: Proto path cut over to `RenderGraphQLRuntime`.** `gw/proto_register.go` now hosts `registerProtoDispatchersLocked` (builds backpressure-wrapped protoDispatchers and registers them under `MakeSchemaID(ns, ver, string(md.Name()))` — wire-native PascalCase, matching what `IngestProto` + `PopulateSchemaIDs` stamp) plus `protoServicesAsIRLocked` (proto-only mirror of `gatewayServicesAsIR`'s proto branch, no lock acquisition since the caller holds `g.mu`). `buildSchemaLocked` swapped its proto pool-walk for a call to `RenderGraphQLRuntimeFields` (new wrapper-friendly variant of `RenderGraphQLRuntime` that returns query/mutation/subscription field maps for merging). Subscriptions stay on `buildSubscriptionFields` — step 6 unifies that surface, so the proto cutover discards the subs map from the IR render and only merges the query/mutation outputs. **Behaviour changes:** container type names follow the documented convention (`GreeterQueryNamespace`, `GreeterV1QueryNamespace`) instead of the old `GreeterNamespace` / `Greeter_V1`; dispatcher SchemaIDs use the wire-native rpc name (`greeter/v1/Hello`) instead of the previous lowerCamel'd form (`greeter/v1/hello`) and only register one entry per RPC instead of two aliases (the version sub-object and the flat alias resolve through the same id). Field paths (`greeter.hello`, `greeter.v1.hello`) and SDL field names (`greeter_greetings` for the still-legacy subscription path) are unchanged. New `RuntimeOptions.SharedProtoBuilder` knob lets the renderer reuse one `*IRTypeBuilder` over the merged proto Types map across every proto service in the schema — required when v1 + v2 of the same proto package coexist (the proto FullNames are identical so per-service builders would trip graphql-go's duplicate-named-type rejection). The renderer also gained a per-kind op-name transform (`opNameForRuntime`): proto applies `lowerCamel` to op.Name at field-key emission so wire `Hello` surfaces as graphql `hello`; OpenAPI / GraphQL stay identity since their op names already follow the target convention.

**Todo.**

The remaining tier-2 runtime work is decomposed below. Each step
is committable on its own; steps 1-2 are pure additions (no
existing code path changes); cuts begin at step 3. Once step 7
lands, the cross-format converters in
`gw/{convert,openapi,graphql_mirror,schema}.go` are gone and IR
is canonical for every code path.

- [x] **Step 1: `RenderGraphQLRuntime` — single-version skeleton.**
  New `gw/render_graphql_runtime.go` (lives in `gw` because
  `IRTypeBuilder` already does — moving the typebuilder into
  `gw/ir` would force `gw/ir` to depend on `graphql-go` and is a
  bigger refactor). Function
  `RenderGraphQLRuntime(svcs []*ir.Service, registry *ir.DispatchRegistry, opts RuntimeOptions) (*graphql.Schema, error)`
  walks top-level Operations + Groups (recursive), builds resolvers
  that look up Dispatchers via SchemaID. One IRTypeBuilder per
  Service; naming policy picked from `Service.OriginKind` so
  proto/OpenAPI/GraphQL type-name conventions match the existing
  per-format builders. Subscription fields flatten (graphql-go
  limitation). Tests against IR fixtures (no Gateway): feed
  service + fake dispatcher, build schema, execute query, verify
  canonical args reach `Dispatch()`. Doesn't replace anything —
  runs side-by-side with `buildSchemaLocked`. ~1 day.
- [x] **Step 2: Multi-version fold inside RenderGraphQLRuntime.**
  Group services by namespace, sort by versionN, latest's
  Operations/Groups at top, older wrapped in `vN` sub-Groups.
  Per-service IRTypeBuilders mean version-prefixed type names
  (`<ns>_v1_Pet`) emerge naturally for OpenAPI/GraphQL while
  proto's package-qualified FullNames stay collision-free without
  extra prefixing. Tests against multi-version IR fixtures.
  ~0.5 day.
- [x] **Step 3: Cut over proto path.** Replace the proto half of
  `buildSchemaLocked` with a call to `RenderGraphQLRuntime`,
  filtered to proto-origin services. OpenAPI + GraphQL paths stay
  intact (they merge their own fields into rootFields as today).
  Should be near-zero behavior change for proto callers — the path
  was already nested + multi-version. Test impact: proto-origin
  tests (greeter, schema_rebuild, grpc_dispatch) only.
  ~0.5-1 day.
- [ ] **Step 4: Cut over OpenAPI path.** Pass OpenAPI services
  through RenderGraphQLRuntime. **Behavior change:** field names
  shift from `pets_v1_getPet` → `pets.v1.getPet`,
  `admin_listPeers` → `admin.listPeers`. ~half the
  openapi_test.go / admin_huma_test.go cases need rewriting; UI
  admin pages break (UI rewrite is its own todo below).
  ~0.5-1 day.
- [ ] **Step 5: Cut over GraphQL ingest path.** Same deal for
  graphql-mirror. Inline-fragment AST rewriting stays put
  (orthogonal — internal to `forwardingResolver`). ~0.5-1 day.
- [ ] **Step 6: Subscription unification.** Move
  `buildSubscriptionFields` into RenderGraphQLRuntime. Subs still
  flatten (graphql-go limitation) but the whole render path is
  one walk. Closes the parity-for-subscription-field-collisions
  risk called out as "highest risk" in the original push. ~0.5 day.
- [ ] **Step 7: Delete dead converters.** Remove `gw/convert.go`,
  `buildPoolRPCs`, `buildOpenAPIFields`, `buildGraphQLFields`,
  `graphQLMirror.build`, `newOpenAPISourceTypeBuilder`,
  `newGraphQLSourceTypeBuilder`. Dispatcher internals
  (`dispatchOpenAPI`, `(m *graphQLMirror).forwardingResolver`,
  `subscribeNATS`) stay — orthogonal to render-side. ~0.5 day.
- [ ] **UI rewrite.** Nested-everywhere means `admin_listPeers` →
  `admin.listPeers`, `admin_forgetPeer` (Mutation) →
  `admin.forgetPeer`. Multi-version OpenAPI sources change too:
  `pets_v1_getPet` → `pets.v1.getPet`. UI admin pages + any typed
  query consumer need migration (graphql-codegen regenerates from
  the new SDL). Triggered by step 4. ~0.5-1 day.
- [ ] **Test churn.** ~70 tests assert flat field names; most need
  rewriting. Trickles through steps 3-5; this is the final
  cleanup pass after step 7. ~1-2 days.
- [ ] **Allocation + cache-friendliness pass on dispatchers.**
  Baselines captured (see done list); per-call hot path is small
  enough to profile. Look for: arg unmarshal allocations
  (178 allocs/proto is heavy), response-shape map building,
  dynamicpb churn, `graphql.ResolveParams` field walks. Goal is
  to close the gap to hand-written handlers; spike a per-schema
  codegen path (`go:generate` from IR) only if reflection-based
  is leaving meaningful headroom. Independent of steps 1-7;
  pickable in parallel. ~2-3 days, scope-dependent.

**Conventions** (settled, see `gw/ir/render_graphql.go`):
- GraphQL renders nested everywhere; proto / OpenAPI flatten via `FlatOperations`. IR carries the structure; each format honors it as far as the format permits.
- Container type names: `<PathPascal><Kind>Namespace`. Top-level `greeter` (Query) → `GreeterQueryNamespace`; sub `v1` → `GreeterQueryV1Namespace`. The kind suffix prevents collisions when the same namespace hosts both queries and mutations.
- Subscription groups flatten to `<group>_<op>` because graphql-go doesn't support nested types under Subscription (`gw/schema.go:231`).
- A namespace with both queries and mutations (e.g. `admin`) emits as two sibling Groups under Service — one per kind.
- `MultiVersionPrefix` was deleted (no production callers); the replacement is the render-side fold above.

**Followups discovered.**
- *SDL→introspection round-trip gap.* `TestGraphQLIngest_NestedNamespaces` / `TestGraphQLRender_NestedNamespaces` exercise ingest → render in halves rather than a true wire round-trip. If a real upstream graphql server emitted the gateway's nested-namespace SDL and the gateway introspected it back, the existing logic works (the heuristic recognizes the nested shape) — just untested end-to-end.

---

### Multi-protocol ingress (long-term vision)

**The push.** Match the egress matrix on the ingress side: today
the gateway dispatches *out* in three protocols (gRPC, HTTP/JSON,
GraphQL) but only accepts *in* over GraphQL. With the IR as the
canonical message contract, the same Dispatcher can serve
inbound HTTP/JSON or gRPC calls — operations are unary (request /
response) or server-streaming (subscriptions over NATS). Bidi /
client-streaming stays out of scope per the decisions log; we
filter those with a warning and offer NATS pub/sub as the
streaming story. Each item below layers on top of the Dispatcher
abstraction; don't start before that abstraction is landed and
parity-tested.

**Todo.**
- [ ] **HTTP/JSON ingress.** Gateway exposes `/<package>.<Service>/<method>` (or REST paths from OpenAPI's HTTPMethod/HTTPPath) accepting JSON bodies, dispatching via canonical args. ~3-4 days.
- [ ] **gRPC ingress for arbitrary services.** Dynamic `grpc.UnknownServiceHandler`-based proxy that catches every `/<svc>/<method>` invocation and routes through canonical args. Unary only — server-streaming RPCs at this ingress would still need to publish to NATS, same contract as today. ~4-5 days.
- [ ] **Subscription transport-agnosticism.** NATS broker / HMAC / channel naming live next to the GraphQL transport today; the ingress work above needs the same subscribe story available over HTTP/SSE or gRPC server-streaming so a non-GraphQL client can subscribe. Symmetric refactor to make subs transport-agnostic. ~3 days.

---

### Generalized parameter injection

**The push.** `HideAndInject[T proto.Message]` is the only first-class
injector today and it's narrow: proto-message-typed fields only
(`gw/hide_inject.go:50` early-returns on anything that isn't
`*dynamicpb.Message`), keyed by message FullName so you can't replace
a scalar (`string token`) with a context-resolved value, and there's
no equivalent for outbound HTTP headers (`ForwardHeaders` is
allowlist pass-through, not context-resolved injection). The
mechanism should generalize across formats and target shapes:

- *Field-level* (in input position) — replace any arg by name with
  a context-resolved value, regardless of whether the underlying
  type is a message, scalar, or enum. Applies symmetrically to
  proto, OpenAPI, and downstream-GraphQL dispatch.
- *Header-level* (outbound) — context-resolve a value and stamp it
  into the outgoing HTTP header (or gRPC metadata) on dispatch.
  Per-source allowlist still gates which headers a particular
  upstream sees.
- *Conditional* — "inject if the client didn't provide a value" vs
  "always override". The greeter example below needs the
  conditional flavour; auth-context overlays today are the
  always-override flavour.

Motivating example: in `examples/multi`, the `greeter.hello(name)`
RPC takes a `name` string. We want the gateway to inject the source
IP (e.g. extracted from `X-Forwarded-For` or the request remote
addr) as the `name` arg when the caller doesn't pass one — a
single declarative `Inject("name", resolveSourceIP, IfMissing)`
on the registered Pair, no greeter-side change. Today this is
expressible only by writing a custom Middleware that knows the
proto-arg shape inside-out, and the schema half (the field would
ideally be marked optional in SDL even if the proto says required)
isn't tied to the runtime half.

**Todo.**
- [ ] **Field-level injection by arg name.** New Pair shape:
  `InjectArg("name", resolve, Mode)` where Mode ∈ {Always, IfMissing}.
  Schema half marks the arg optional in SDL when Mode=IfMissing
  (so codegen consumers don't have to lie). Runtime half resolves
  via context (caches per-request like `HideAndInject` already
  does) and stamps onto the dispatcher's canonical args before
  the dispatcher's own marshal step. Cross-format: applies on
  proto / OpenAPI / GraphQL dispatch paths because the injection
  point is the dispatcher's `args map[string]any`, which is
  format-neutral. Greeter example becomes a one-liner. ~1-2 days.
- [ ] **Header-level injection (outbound).** New Pair shape:
  `InjectHeader("X-Source-IP", resolve, Mode)`. Stamps onto the
  outgoing HTTP request (OpenAPI dispatch) or gRPC metadata
  (proto dispatch). Header allowlist (`ForwardHeaders`) still
  applies — injection populates a header, the allowlist gates
  whether the upstream sees it. ~1 day.
- [ ] **Re-base `HideAndInject` on the new primitive.** Express
  `HideAndInject[T]` as `InjectArg(<every-arg-of-type-T>, resolve,
  Always)` with the schema half hiding those args. Backwards-
  compatible facade so existing callers don't break. ~0.5 day.
- [ ] **Worked example in `examples/multi`.** Greeter resolver
  documents both flavours (always-override for an audit
  identifier; if-missing source-IP-as-name) so operators have a
  copyable template. ~0.5 day.

---

### Existing tier-2 tail (parked behind real use cases)

**The push.** None individually — these are first-class versions
of capabilities that are already composable today. Promote to tier
1 if any becomes load-bearing for a real deployment.

**Todo.**
- [ ] **Service-account token outbound auth.** Built-in helper that wraps a RoundTripper and refreshes a token on schedule. Achievable now via custom `*http.Client`; first-class when a deployment wants it.
- [ ] **OAuth/JWT translation outbound auth.** Gateway exchanges the inbound token for a service-specific token via a configurable issuer. Composable today, first-class when needed.
- [ ] **Destructive read opt-in.** `AdminMiddleware` lets every GET through for the UI. When a destructive read shows up (`/admin/peers/{id}/inspect-state`), gate it explicitly via a per-route flag rather than flipping the global GET policy. Parked until a real destructive read needs it.
- [ ] **UI rotate-key panel.** Shows the configured kid set, with a "set active" toggle. Token rotation itself is done; the panel ships when an operator asks.
- [ ] **Interface / Union typed-mirror polish + oneOf/anyOf richer mapping.** Both base cases shipped; richer projections (carrying `graphql.Interface` with shared fields, more elaborate union resolution) wait for a use case.

---

## Tier 3 — operational polish

**The push.** Knobs and ergonomics for operators. Nothing here
blocks shipping; pick up opportunistically.

**Todo.**
- [ ] **Connection-rate limiting / per-IP caps.** Reject excessive new WS connections per IP / per token. Token bucket; configurable knob. Prevents trivial DoS on the WebSocket terminator.
- [ ] **k8s + docker-compose example deployments.** YAML manifests for the 3-gateway cluster from `examples/multi`. Shows how to wire `--nats-peer`, health probe, drain-on-shutdown.
- [ ] **NATS server log noise control.** Routes log everything at INFO; tests pile up server-banner output too. Expose a `Logger`/`LogLevel` field on `ClusterOptions` and a `--nats-log-level` CLI flag.
- [ ] **Metrics / tracing example middleware.** Concrete `Pair` showing OpenTelemetry / Prometheus-app-level integration on top of what the gateway already emits.
- [ ] **Cluster.Close vs Gateway.Close lifecycle docs.** Document the shutdown sequence: `gw.Drain` → `srv.GracefulStop` → `cluster.Close`. Out-of-order calls are OK; the example should show the right sequence.
- [ ] **Heartbeat-to-wrong-gateway smoothing.** When a service heartbeats to a gateway that didn't receive its Register, fall back to checking the registry KV instead of forcing re-register. Smaller dispatch-failure window during gateway failover.
- [ ] **Sub-fanout drop policy configurable.** Today a slow consumer drops events. Operator might want "kick the slow one" instead. Per-consumer watermark + configurable behaviour.

---

## Known limitations (won't fix unless driven by use case)

- **`SchemaMiddleware` half of `Pair` is stubbed.** `HideAndInject`
  uses the runtime half only. Schema-rewrite middleware that needs
  the schema half hasn't been built — no concrete use case has shown
  up beyond what `Hides` already does declaratively.
- **Apollo Federation.** Not planned. Stitching covers the common case;
  federation's entity-merging is overkill for most use cases and pulls
  in a query planner.
- **AsyncAPI export.** Considered, dropped. GraphQL SDL with
  Subscription types covers TS codegen via graphql-codegen; AsyncAPI's
  TS tooling is patchier and adds a parallel codegen path with little
  payoff. Revisit if backend-to-backend integration use cases show up.
- **A `Register` call is one address contributing to N pools, not N
  bindings with independent lifecycles.** One service's `Register`
  payload can carry many `ServiceBinding`s (many namespaces, one
  address); the gateway adds the same address as a replica to each
  pool. They share lifetime: the heartbeat keeps every binding
  alive together, and a deregister drops them all at once. There's
  no way for a single binary to register two namespaces and later
  evict only one. If you need that, run two binaries (or two
  control-plane connections from the same binary). The reconciler
  and `_events_auth` / `_admin_auth` delegates work the same way.

---

## Decisions log

Things that are settled. Reading these prevents re-litigating in
future sessions.

| Decision | Rationale |
|---|---|
| **Proto/gRPC is the canonical service-to-service contract** | GraphQL is a client-facing surface. Typed GraphQL client codegen is excellent in TS/JS, fair in Go, weak everywhere else (Python, Rust, Java, .NET, etc.). Every language has a mature protoc plugin; a `.proto` file is the multilingual contract. The GraphQL SDL is *derived* from proto — emergent, not authoritative. OpenAPI and downstream GraphQL are *bridges* for legacy/external services that don't speak gRPC. New service-to-service work goes through proto. |
| **Per-pool backpressure, not gateway-wide unary cap** | Slow service X shouldn't gate dispatches to service Y. Pool is the isolation primitive. |
| **Hybrid stream caps** (per-pool + gateway-wide) | Per-pool gives fine-grained throttling when wanted; gateway-wide caps the actual scarce resource (FDs, RAM). Defaults: 10k per pool, 100k total. |
| **Subscriptions = NATS pub/sub, not gRPC streams** | NATS handles fan-out natively (10M msg/s, 100k+ subs). gRPC streams require a long-lived per-client gateway-to-service connection; doesn't compose at scale. |
| **HMAC verify on subscribe; delegate at sign time** | Verify is hot-path crypto-fast. Sign is the privileged path where business authz logic belongs. |
| **OpenAPI 3.x via kin-openapi** | Most-used Go OpenAPI parser; supports 3.0 + 3.1. Huma emits 3.1 — works. |
| **Stitching for downstream GraphQL, not federation** | Federation solves entity merging, which most teams don't have. Stitching is ~300 LoC, fits the gateway's "thin proxy + namespace" model. |
| **Proto stays canonical for events** | One source of truth for types; AsyncAPI would be a derived view, dropped for v1. |
| **`--environment` becomes part of NATS cluster name** | Hard isolation between dev/staging/prod at the broker level. Cannot accidentally cross-talk. |
| **Schema diff via SDL, hash parity via canonical descriptors** | Two views of "are these clusters compatible": semantic (SDL diff) and structural (hash equality). |
| **No flat gateway-wide unary queue** | Re-introduces the cross-pool blocking problem we explicitly designed away. |
| **Server-streaming gRPC filtered with warning, not implemented** | Subscription path is NATS-backed. Lifting actual gRPC streams adds a transport story we'd rather not maintain. Files declaring server-streaming RPCs surface in the schema, but services must publish to the resolved NATS subject rather than implementing the gRPC stream method — README documents the subject derivation. |
| **`AdminMiddleware` gates writes, lets reads through** | UI's services/peers views must work unauthenticated for the operator to find the token in the first place. Destructive reads will need explicit opt-in once any exist. Dispatch path forwards `Authorization` so a token presented at /graphql reaches /admin/\* automatically — the bearer middleware is the single gating point. |
| **OpenAPI dispatch forwards `Authorization` by default; `ForwardHeaders(...)` overrides per source** | The default makes admin\_\* GraphQL mutations work end-to-end with one bearer (the dogfood path). Per-source allowlist replaces it when a backend wants a different header set or no forwarding at all. Tier-2 alternatives (service-account, mTLS, OAuth translation, `WithOpenAPIClient`) layer on top of this rather than replacing it. |
| **AdminAuthorizer fall-through priority: delegate → boot token** | Boot token is the always-works emergency hatch. A delegate that crashes / DOS's / mis-deploys cannot lock operators out: UNAVAILABLE / transport error / NOT_CONFIGURED fall through. Only an explicit DENIED short-circuits. Operators can still get in with the on-disk token. |

---

## Reference

Background context for orienting a new session. Not work items.

### Test seed

~70 cases across 12 files. The fixture patterns to crib from:

**Unit-shape (httptest + helper-level):**
- `auth_admin_test.go` — AdminMiddleware read/write split, token
  store + persistence, header-forwarding allowlist, admin auth
  metrics (countingMetrics fixture).
- `auth_admin_delegate_test.go` — AdminAuthorizer delegate
  (no-delegate, OK, DENIED, UNAVAILABLE, transport error, public
  reads bypass, request fields). Uses an in-process
  `grpc.ClientConnInterface` fake — no real gRPC server.
- `schema_rebuild_test.go` — pool create/destroy/hash-mismatch
  flows through `assembleLocked`. Includes auto-internal `_*`
  namespace test.

**HTTP-shape (httptest backend + `gw.Handler()`):**
- `openapi_test.go` — boot-time OpenAPI dispatch + `ForwardHeaders`
  + `WithOpenAPIClient` / `OpenAPIClient(c)` resolution.
- `dynamic_openapi_test.go` — control-plane registration of
  OpenAPI specs (standalone + cluster cross-gateway + multi-replica
  least-in-flight).
- `graphql_ingest_test.go` — `AddGraphQL` mirror, namespace prefix,
  forwarding strips prefix, args pass through, duplicate ns
  rejected. Hand-rolled introspection JSON in the test file.
- `admin_huma_test.go` — channels / drain / openapi.json routes
  through `AdminMiddleware`.

**gRPC-shape (in-process `grpc.Server`):**
- `grpc_dispatch_test.go` — real grpc.Server on `127.0.0.1:0`
  implementing GreeterService.Hello, registered via
  `AddProtoDescriptor`. Happy path, v1 sub-object, backend error,
  drained-pool.

**Cluster-shape (`StartCluster` + ephemeral ports + tempdir):**
- `forget_peer_test.go` — single-node cluster, manual peers-KV
  manipulation; alive vs expired vs never-registered.
- `cluster_dispatch_test.go` — two-node cluster peering on free
  TCP ports; Register on A, dispatch from B via the KV reconciler.
- `subscriptions_test.go` — embedded NATS + WebSocket; greeter
  registered via `AddProtoDescriptor`; covers HMAC codes, NOT_CONFIGURED,
  client-`complete` broker cleanup, admin\_events round-trip
  (also exercises the proto-enum-as-int32 serialisation fix).
- `admin_events_test.go` — admin\_events publisher direct via NATS
  subscriber (no WS). Cluster-required check.

**Pattern:** httptest + `gw.Handler()` for OpenAPI / GraphQL /
subscription / gRPC paths; direct helper calls or
`grpc.ClientConnInterface` fakes for unit shape. Every new feature
should add same-shape coverage.

**Lifetime gotcha** (in `cluster_dispatch_test.go` comments):
`startClusterTracking(ctx)` captures `ctx` as the parent of the
long-running watch + reconciler goroutines. Test helpers must pass
`context.Background()` (not a `WithTimeout`) so those goroutines
outlive the helper return. Cleanup runs through `gw.Close →
tracker.stop`. The symptom of getting this wrong is "registry KV
has the key on both nodes but B's reconciler never creates the
pool".

### Schema export family

Three sibling endpoints under `/schema/*`, each accepting the same
`?service=ns[:ver][,...]` selector grammar:

- `GET /schema/graphql` — SDL (default) or introspection JSON via
  `?format=json`. Derived from registered protos + OpenAPI +
  downstream-GraphQL ingest. Filtered requests build a fresh schema
  per call; unfiltered uses the cached `g.schema`.
- `GET /schema/proto` — FileDescriptorSet (binary
  `application/protobuf`) with **gateway transformations applied**:
  hidden fields stripped via the `Pair.Hides` set; internal
  namespaces excluded. Not the raw registered protos — the contract
  surface as the gateway exposes it.
- `GET /schema/openapi` — JSON object keyed by namespace, re-emitting
  each ingested OpenAPI spec.

`/schema` (without sub-path) stays as a back-compat alias for SDL.

Selector grammar:
- `service=auth:v1,library` → auth at v1 + all versions of library.
- Missing version → all versions of that namespace.
- Missing service param → everything (subject to internal filtering).

### Dogfooding: huma → OpenAPI → GraphQL

The gateway's own admin operations are defined via huma
(`admin_huma.go`), mounted as plain HTTP at `/admin/*`, and
**self-ingested** at boot via `AddOpenAPIBytes`:

```go
adminMux, adminSpec, _ := gw.AdminHumaRouter()
mux.Handle("/admin/", gw.AdminMiddleware(adminMux))
gw.AddOpenAPIBytes(adminSpec,
    gateway.As("admin"),
    gateway.To("http://localhost:18080"))
```

Result: SDL gains flat `Query.admin_listPeers`,
`Query.admin_listServices`, `Mutation.admin_forgetPeer`,
`Mutation.admin_signSubscriptionToken`. Each huma handler delegates
to the existing `controlPlane` gRPC methods in-process (no extra
hop). External clients see one GraphQL surface; the huma OpenAPI
is the contract source.

This is the same path any external huma-defined service takes —
`gw.AddOpenAPI("https://service/openapi.json", gateway.To(...))`.
Dogfooding it for the gateway's own admin keeps the integration
path tested by the gateway itself.

`AddProtoDescriptor` survives for the gRPC-self-registration case
(e.g. expose a service whose proto is compiled into the gateway
binary) but the recommended path is huma + OpenAPI for
admin/operator surfaces.

### UI

React + MUI v6 + TanStack Router admin console at `ui/`.

**Build flow** (the dogfood):
```
1. start gateway        cd examples/multi && ./run.sh
2. fetch + codegen      cd ui && pnpm install && pnpm run gen
3. dev server           pnpm run dev    → http://localhost:5173
4. production           pnpm run build  → dist/
```

`pnpm run gen` is `pnpm run schema && pnpm run codegen`:
- `schema` curls `${GATEWAY_URL:-http://localhost:18080}/schema/graphql`
  into `schema.graphql`.
- `codegen` runs graphql-codegen against the cached SDL, emitting
  `src/api/gateway.ts` with typed query/mutation functions.

Pages: Dashboard, Services, Peers (with Forget mutation), Schema
viewer. Vite proxies `/graphql`, `/schema`, `/health` to the gateway.
