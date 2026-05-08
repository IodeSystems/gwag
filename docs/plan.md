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

**Todo.**
- [ ] **IR-driven type building.** Replace the three per-format `typeBuilder` paths (`gw/types.go` proto, `openAPITypeBuilder` in `gw/openapi.go`, `graphQLMirror` type code) with a single `ir.Type` → `graphql.{Object,Input,Enum,Union}` builder. Each format's ingester already populates `ir.Type` entries; the new builder walks them. Lets `buildSchemaLocked` synthesize types from `gatewayServicesAsIR()` instead of separately walking each registry. ~2 days.
- [ ] **Multi-version render-side fold + `RenderGraphQLRuntime(svcs, registry)`.** Walks IR + registry, composes same-namespace services into a single synthesized Group tree (latest at top, older as `vN` sub-groups), builds `*graphql.Schema` with resolvers that look up Dispatchers via SchemaID. Cuts over `buildSchemaLocked`; deletes the old converters. ~1-2 days. **Highest risk** — parity for hide-and-inject middleware and subscription field collisions.
- [ ] **UI rewrite.** Nested-everywhere means `admin_listPeers` → `admin.listPeers`, `admin_forgetPeer` (Mutation) → `admin.forgetPeer`. Multi-version OpenAPI sources change too: `pets_v1_getPet` → `pets.v1.getPet`. UI admin pages + any typed query consumer need migration (graphql-codegen regenerates from the new SDL). ~0.5-1 day.
- [ ] **Test churn.** ~70 tests assert flat field names (`admin_listPeers` etc.); most need rewriting. ~1-2 days.
- [ ] **Dispatch benchmark harness.** Before/after numbers per dispatch path (proto / OpenAPI / graphql-mirror) under `bench/` — RPS, p50/p99, allocs/op, B/op. Establishes the parity baseline so the optimization work below has a target to beat. ~1 day.
- [ ] **Allocation + cache-friendliness pass on dispatchers.** With Dispatcher extracted, per-call hot path is small enough to profile. Look for: arg unmarshal allocations, response-shape map building, dynamicpb churn, `graphql.ResolveParams` field walks. Goal is to close the gap to hand-written handlers; spike a per-schema codegen path (`go:generate` from IR) only if reflection-based is leaving meaningful headroom. ~2-3 days, scope-dependent.

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
