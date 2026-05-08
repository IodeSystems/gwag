# go-api-gateway: roadmap & decisions

## How to use this file

Source of truth for in-flight work, priority order, and the decisions log. Read at session start.

**What "make progress on plan.md" means:**
1. If Tier 1 has anything, work the top item.
2. Otherwise pick the top open todo in Tier 2 (priority order, top → bottom).
3. Read enough Done lines to understand current state.
4. Do the work.
5. Check the box (`- [ ]` → `- [x]`) and add a one-line Done entry. Verbose history goes in commit messages, not here — keep this file scannable.
6. Commit so the tree is clean. One item = one commit (or commit set if the work decomposed). Leave the working tree clean before context-switching.
7. Don't rearrange tiers without surfacing the decision in chat first.

**Item shape:** **The push** (one paragraph: why and where it sits) → **Done** (one-line entries with commit hashes) → **Todo** (commit-sized chunks with rough effort) → **Followups** (mid-flight discoveries that don't block).

**Tier meaning:** Tier 1 = production-blocking. Tier 2 = design-completing, ordered by priority. Tier 3 = polish. Known limitations = called out intentionally; not planned to fix.

## Product priorities (settled)

Phase 1 ships best-of: **utility, availability, ox/dx**. Performance and stability are something we move *toward*, not gate v1 on. A panacea-that-is-slow beats a fast tool that's hard to set up. Perf paths (codegen, plugins, etc.) layer on as opt-ins; the default code path stays reflection-based and always-works.

Architectural test: every decision should keep the default path working for any reasonable input, even if slower. Platform / toolchain / build-step constraints are fine as opt-ins, fatal as defaults.

---

## Tier 1 — production-blocking

Empty. File new items here when something real breaks.

---

## Tier 2 — design-completing (priority order)

### 1. Multi-protocol ingress

**The push.** Match the egress matrix on the ingress side. The gateway dispatches *out* in three protocols (gRPC, HTTP/JSON, GraphQL) but only accepts *in* over GraphQL. Java/C#/Rust/Python have first-class proto tooling and middling GraphQL tooling — GraphQL-only ingress strands real users. With `ir.Dispatcher` settled, accepting HTTP/JSON or gRPC is "wire-bytes → canonical args → `Dispatch()` → wire-bytes back". `/api/schema/proto` already emits the typed contract; ingress just needs to serve requests against it.

**Constraint.** Unary + server-streaming only. Bidi / client-streaming stays out per the decisions log; we filter with a warning and offer NATS pub/sub.

**Todo.**
- [ ] **HTTP/JSON ingress.** `/<package>.<Service>/<method>` (and OpenAPI's HTTPMethod/HTTPPath when available) accepting JSON bodies, dispatching via canonical args. ~3-4 days.
- [ ] **gRPC ingress for arbitrary services.** Dynamic `grpc.UnknownServiceHandler` proxy catching every `/<svc>/<method>`. Unary only — server-streaming still publishes to NATS. ~4-5 days.
- [ ] **Subscription transport-agnosticism.** NATS broker / HMAC / channel naming live next to the GraphQL transport today; ingress work above needs the same subscribe story over HTTP/SSE or gRPC server-streaming. ~3 days.

### 2. Allocation pass on reflection dispatchers

**The push.** Reflection is the default forever (see priorities). Lifting its floor lifts perf for everyone, no opt-in required. Baselines: proto 174 allocs/op @ ~185µs, OpenAPI 86 allocs/op @ ~128µs (loopback). gRPC client/server internals dominate (~110/op proto); our wrapper overhead is small but tractable.

**Done.**
- [x] Per-descriptor field-name cache (`gw/convert.go`); 178 → 174 allocs/op proto. Cache keyed by descriptor identity, not FullName — same .proto + two gateways = identical FullName but distinct descriptors that dynamicpb refuses to cross-pollinate. (03851e7)
- [x] `BenchmarkProto/OpenAPISchemaExec` for full graphql.Do path. IR runtime overhead vs pre-cutover (8fd7de8): 0% proto, +16% OpenAPI from flat→nested namespace shape (cross-format consistency cost, not IR machinery). (ea73748)

**Todo.**
- [ ] **dynamicpb pooling.** sync.Pool of cleared messages per dispatcher; `proto.Reset` is `m.ProtoReflect().Clear()`, safe to reuse after grpc has marshalled wire bytes. ~4 allocs/call savings. ~0.5 day.
- [ ] **Per-schema codegen spike (informational).** Sketch what reflection-free dispatch would look like; don't ship. Surfaces the perf gap concretely and informs the static codegen workstream below. ~1-2 days.

### 3. Static codegen — RegisterCodegen surface

**The push.** Operators who know their service set at build time should be able to opt into native-speed dispatch with one extra `go generate`. Generated code lives in their binary, linked normally — no plugin machinery, no platform constraints. The registry checks for a generated entry first and falls back to reflection per-service, so it's not all-or-nothing.

**Surface contract.** Codegen output is a self-contained Go package exporting `func Dispatchers(deps SDK) map[ir.SchemaID]ir.Dispatcher`. Operator imports + calls `gw.RegisterCodegen(generated.Dispatchers(...))` alongside `AddProto`. The plugin supervisor (workstream 4) reuses this same artifact, just runtime-compiled.

**Todo.**
- [ ] **Plugin SDK (`gw/sdk` subpackage).** Stable, minimal interfaces (`PoolDispatcher`, `OpenAPIDispatcher` etc.) the codegen consumes. Caps the API surface vs the full `gw` package; bounded by versioning. ~1 day.
- [ ] **Codegen template + driver.** `go-api-gateway codegen --schema=foo.graphql --out=./dispatchers` walks IR, emits typed dispatchers (no reflection, no dynamicpb). ~3-4 days.
- [ ] **`gw.RegisterCodegen` registration point.** Slots into the same `DispatchRegistry`; precedence: codegen entry > reflection entry. ~0.5 day.
- [ ] **Worked example in `examples/multi`.** Operator template + measured perf vs reflection. ~0.5 day.
- [ ] **Telemetry.** `/metrics` per-dispatcher mode (reflection / codegen / plugin) + per-mode latency histograms. Operators see the upgrade path without anyone telling them. ~0.5 day.

### 4. Plugin supervisor for dynamic-static dispatch

**The push.** Operators who want both fast *and* dynamic (control-plane registrations + codegen perf) get a supervisor that runs the codegen toolchain at runtime, builds a `.so`, and rolls it through the cluster via drain-and-restart. Each gateway loads the plugin once per process lifetime — Go plugins can't unload, but the cluster's drain primitive sidesteps that (process dies, plugin dies with it). Same artifact as workstream 3, just runtime-compiled.

**Blocked on workstream 3** (the codegen output is the supervisor's input).

**Todo.**
- [ ] **Compile coordinator.** Leader compiles via the toolchain; publishes `.so` to JetStream object store; peers fetch + load. Compile once per cluster, eliminates version-skew structurally. ~3 days.
- [ ] **Settle window + debounce.** Bursty registrations (5-20/sec on deploys) coalesce into one schema rev before triggering rebuild. ~30s window, tunable. ~1 day.
- [ ] **Rolling drain controller.** Uses existing `Drain()` + `/health` 503; sequenced node drain → fetch → load → up; readiness gate on "≥1 successful dispatch per pool" before draining the next node. Cold-start dwell (empty pools, no HTTP keep-alives) is real — the gate exists to avoid cascading everyone into cold-start at once. ~3 days.
- [ ] **Compile-fail fallback.** `.so` load failure → keep reflection path; alert + retry. Compile/load problems must never take the gateway down. ~1 day.
- [ ] **Toolchain placement decision.** Sidecar / init container vs in-image; security tradeoff (gateway image gains the ability to run `go build` on IR-derived source). Document in plan + README. ~0.5 day.

### 5. Generalized parameter injection

**The push.** `HideAndInject[T proto.Message]` is the only first-class injector and it's narrow: proto-message-typed fields only, keyed by message FullName. The mechanism should generalize across formats (proto / OpenAPI / GraphQL ingest) and target shapes (any arg by name; outbound HTTP headers; conditional vs always-override). Motivating example: `greeter.hello(name)` should be able to declare "inject source IP as `name` if the caller didn't pass one" via `Inject("name", resolveSourceIP, IfMissing)` — no greeter-side change.

**Todo.**
- [ ] **Field-level injection by arg name.** `InjectArg("name", resolve, Mode)` Pair shape; Mode ∈ {Always, IfMissing}. Cross-format because injection lands on canonical args. SDL marks the arg optional when Mode=IfMissing. ~1-2 days.
- [ ] **Header-level injection (outbound).** `InjectHeader("X-Source-IP", resolve, Mode)`. Stamps onto HTTP request (OpenAPI dispatch) or gRPC metadata (proto dispatch). `ForwardHeaders` allowlist still gates upstream visibility. ~1 day.
- [ ] **Re-base `HideAndInject`.** Express in terms of the new primitive; backwards-compatible facade. ~0.5 day.
- [ ] **Worked example in `examples/multi`.** Both flavours documented. ~0.5 day.

### 6. Existing tier-2 tail (parked behind real use cases)

- [ ] **Service-account token outbound auth.** Built-in helper wrapping a RoundTripper. Composable today; first-class when wanted.
- [ ] **OAuth/JWT translation outbound auth.** Inbound token → service-specific token via configurable issuer. Composable today.
- [ ] **Destructive read opt-in.** AdminMiddleware lets every GET through; gate destructive reads via per-route flag when first one shows up.
- [ ] **UI rotate-key panel.** Token rotation done; panel ships when an operator asks.
- [ ] **Interface / Union typed-mirror polish + richer oneOf/anyOf mapping.** Base cases shipped; richer projections wait for use case.

---

## Tier 3 — operational polish

- [ ] Connection-rate limiting / per-IP caps on WebSocket terminator.
- [ ] k8s + docker-compose example deployments for `examples/multi`.
- [ ] NATS server log noise control (`Logger`/`LogLevel` on `ClusterOptions`).
- [ ] Metrics / tracing example middleware.
- [ ] `Cluster.Close` vs `Gateway.Close` lifecycle docs.
- [ ] Heartbeat-to-wrong-gateway smoothing (registry KV check before forcing re-register).
- [ ] Sub-fanout drop policy configurable (per-consumer watermark + behaviour knob).

---

## Known limitations (won't fix unless driven by use case)

- **`SchemaMiddleware` half of `Pair` is stubbed.** Runtime half of `HideAndInject` only — schema-rewrite middleware hasn't found a use case beyond what `Hides` does declaratively.
- **No Apollo Federation.** Stitching covers the common case; federation's entity-merging is overkill for most teams.
- **No AsyncAPI export.** GraphQL SDL with Subscription types covers TS codegen; AsyncAPI's TS tooling is patchier with little payoff.
- **One Register call = one address contributing to N pools, not N independent bindings.** Bindings share lifetime; heartbeat + deregister are atomic across all of them. Run two binaries (or two control-plane connections) for independent lifecycles.

---

## Decisions log

Settled. Reading these prevents re-litigating in future sessions.

| Decision | Rationale |
|---|---|
| **Reflection is the default dispatch path forever** | Always works; no platform/toolchain constraints; lowest setup friction. Codegen + plugin are opt-ins. (See Product priorities.) |
| **Proto/gRPC is canonical service-to-service** | GraphQL client codegen is excellent in TS/JS, fair in Go, weak elsewhere. `.proto` is the multilingual contract; SDL is *derived*. OpenAPI + downstream-GraphQL ingest are bridges. |
| **Per-pool backpressure, not gateway-wide unary cap** | Slow service X shouldn't gate dispatches to service Y. Pool is the isolation primitive. |
| **Hybrid stream caps** (per-pool + gateway-wide) | Per-pool throttles fine-grained; gateway-wide caps the actual scarce resource (FDs, RAM). Defaults: 10k per pool, 100k total. |
| **Subscriptions = NATS pub/sub, not gRPC streams** | NATS handles fan-out natively. gRPC streams require long-lived per-client gateway-to-service connections; doesn't compose at scale. |
| **HMAC verify on subscribe; delegate at sign time** | Verify is hot-path crypto-fast. Sign is the privileged path where business authz lives. |
| **Stitching for downstream GraphQL, not federation** | Federation solves entity-merging that most teams don't have. |
| **Proto stays canonical for events** | One source of truth; AsyncAPI would be a derived view, dropped. |
| **`--environment` becomes part of NATS cluster name** | Hard isolation between dev/staging/prod at the broker level. |
| **Schema diff via SDL, hash parity via canonical descriptors** | Two views of compatibility: semantic + structural. |
| **Server-streaming gRPC filtered with warning, not implemented at egress** | Subscription path is NATS-backed; lifting actual gRPC streams adds a transport story we'd rather not maintain. |
| **`AdminMiddleware` gates writes, lets reads through** | UI's services/peers views must work unauthenticated for the operator to find the token in the first place. |
| **OpenAPI dispatch forwards `Authorization` by default; `ForwardHeaders` overrides** | Default makes admin\_\* end-to-end work with one bearer. |
| **AdminAuthorizer fall-through: delegate → boot token** | Boot token is the always-works emergency hatch. UNAVAILABLE / transport / NOT_CONFIGURED falls through; only explicit DENIED short-circuits. |
| **GraphQL renders nested; proto / OpenAPI flatten via `FlatOperations`** | IR carries the structure; each format honors it as far as the format permits. |

---

## Reference

### Recently shipped: IR runtime cutover

`gw/ir` is canonical for ingest/render across proto / OpenAPI / GraphQL. Runtime dispatchers go through `ir.Dispatcher` registered in a per-build `*ir.DispatchRegistry`; resolvers look up by `SchemaID`. `RenderGraphQLRuntime` (`gw/render_graphql_runtime.go`) emits the runtime schema from IR; per-format builders are gone. Net runtime overhead vs pre-cutover: 0% proto, +16% OpenAPI from flat→nested namespace shape (intentional UX choice).

Conventions (settled): container type names = `<PathPascal><Kind>Namespace`; subscriptions flatten via `<group>_<op>` (graphql-go limitation); a namespace with both queries and mutations emits as two sibling Groups under Service.

### Test fixture patterns

- **Unit-shape:** httptest backend or in-process `grpc.ClientConnInterface` fake; helper-level direct calls. (`auth_admin_test.go`, `auth_admin_delegate_test.go`)
- **HTTP-shape:** httptest backend + `gw.Handler()`; full GraphQL → upstream round-trip. (`openapi_test.go`, `graphql_ingest_test.go`)
- **gRPC-shape:** in-process `grpc.Server` on `127.0.0.1:0` + `AddProtoDescriptor`. (`grpc_dispatch_test.go`)
- **Cluster-shape:** `StartCluster` + ephemeral ports + tempdir. **Lifetime gotcha:** pass `context.Background()` (not `WithTimeout`) as the parent for watch + reconciler goroutines, otherwise they die mid-test. Symptom: registry KV has the key on both nodes but B's reconciler never creates the pool. (`cluster_dispatch_test.go`)

### HTTP routing surface

`/api/*` is the gateway, everything else is the embedded UI bundle. Unmatched `/api/*` returns JSON 404; non-API requests fall back to the SPA's `index.html`. The split is an example wiring choice (`examples/multi`), not a library constraint — `gw.UIHandler(fs.FS)` and per-handler primitives (`gw.Handler()`, `gw.SchemaHandler()`, `gw.AdminMiddleware(...)`) let operators arrange routes however.

| Path | Auth | What |
|---|---|---|
| `/api/graphql` | public for queries/subs, bearer for mutations (transitive) | GraphQL + WS upgrade |
| `/api/schema/{graphql,proto,openapi}` | public | SDL / FDS / re-emitted OpenAPI |
| `/api/admin/*` reads | public | huma reads |
| `/api/admin/*` writes | bearer | huma mutations |
| `/api/health` | public | 503 during `Drain()` |
| `/api/metrics` | public (or behind reverse-proxy auth) | Prometheus |

Bearer = boot token (logged at startup; persisted to `<adminDataDir>/admin-token` if `WithAdminDataDir` is set). Pluggable AdminAuthorizer delegate (registered under `_admin_auth/v1`) consulted first; boot token underneath as the always-works hatch.

### Schema export selectors

`?service=auth:v1,library` — auth at v1 + all versions of library. Missing version → all versions of namespace; missing param → everything (subject to `_*` internal filtering).

### Dogfooding: huma → OpenAPI → GraphQL

Admin operations are defined via huma (`gw/admin_huma.go`), mounted as HTTP at `/admin/*`, and self-ingested via `AddOpenAPIBytes` so SDL gains nested `Query.admin.listPeers` / `Mutation.admin.forgetPeer`. Same path any external huma service takes — use as template for new admin operations.

### UI

React + MUI v6 + TanStack Router admin console at `ui/`. Build flow:
```
cd examples/multi && ./run.sh         # gateway up
cd ui && pnpm install && pnpm run gen # fetch schema + codegen
pnpm run dev                          # http://localhost:5173
pnpm run build                        # → dist/
```
`pnpm run gen` curls `${GATEWAY_URL}/schema/graphql` then runs graphql-codegen → `src/api/gateway.ts`. Pages: Dashboard, Services, Peers (with Forget mutation), Schema viewer.
