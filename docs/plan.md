# go-api-gateway: roadmap & decisions

Source of truth for open work, design decisions, and the rationale
behind both. Read at session start. Update whenever scope shifts.

Tier 1 = correctness / production-blocking. Tier 2 = design-completing
features. Tier 3 = operational polish. Known limitations = called out
intentionally; not currently planned to fix.

---

## Tier 1 — load-bearing (production-blocking)

**Empty.** Inbound admin auth, outbound auth v1, and every
load-bearing glue surface (admin / OpenAPI / gRPC unary /
subscriptions / schema rebuild / ForgetPeer / cross-gateway
dispatch) have e2e tests. Outbound-auth alternatives moved to
tier 2; the test follow-ups (cosmetic log spam, broader cluster
scenarios) are tracked under tier 3 / tier 2 as appropriate.

If a new production-blocking item shows up, file it here. Daily
work draws from tier 2.

---

## Tier 2 — design-completing

Open items only — completed work is in *Recently Shipped*. Ordered
by current leverage; the top items are realistic next picks for a
fresh session.

**Suggested pickups:**
- *Architecturally interesting:* GraphQL **subscription forwarding**
  (graphql-ws upstream multiplexer). Closes the AddGraphQL story so
  remote Subscription roots aren't skipped at registration time. With
  dispatch metric + classification + backpressure + dynamic
  registration + multi-replica all shipped for queries/mutations,
  subscriptions are the last AddGraphQL gap.
- *Operational:* OpenAPI / GraphQL **multi-version**. Today both
  pin to `v1`; multi-version (matching the proto pool model) needs
  the version axis threaded through `addOpenAPISourceLocked` /
  `addGraphQLSourceLocked` and the schema-build paths. Probably
  larger than it looks because the codepaths assume single-version
  in many places.

### Token rotation (kid in tokens)

Done — see `325aaf4` (verifier + standalone signer) and the
follow-on commit (RPC kid in/out) under Recently Shipped. Future
work would be the UI side: a "rotate key" panel that shows the
configured kid set, with a "set active" toggle. Park until an
operator asks.

### Downstream GraphQL ingestion: subscriptions

The boot-time mirror handles queries + mutations (see commit
`3517273`). To complete the story, forward subscriptions:
- Multiplex one upstream graphql-ws WebSocket per (gateway,
  downstream-service) — same shape as the existing local sub
  fanout in `broker.go`.
- The local Subscription type gains a `<ns>_<remoteSubField>`
  entry per remote subscription field. Resolver dials the upstream
  WS, subscribes, fans incoming `next` frames to the local client.
- Auth: respect `ForwardHeaders` for connection_init and HMAC
  args for subscribes (same convention as queries).

### Downstream GraphQL ingestion: dynamic registration

Done — see Recently Shipped. Future work: GraphQL ingest
multi-replica + load balancing (see suggested pickups).

### Downstream GraphQL ingestion: Interface / Union typed mirror

v1 falls back to a JSON scalar with a registration-time log line.
Add proper Interface / Union mirroring once a real downstream uses
them. Map possibleTypes to `graphql.NewUnion`; resolve `__typename`
on each value to pick the variant.

### Outbound auth pass-through alternatives

`Authorization` is forwarded by default; `ForwardHeaders(...)`
overrides per-source. `WithOpenAPIClient(*http.Client)` (gateway-
wide default) and `OpenAPIClient(c)` (per-source override) let
operators plug in any transport — mTLS, custom RoundTripper, signed
URLs, retry/timeout policy. That covers the common cases.

Open design forks for richer scenarios:
- **Service-account token**: a built-in helper that wraps a
  RoundTripper and refreshes a token on schedule. Today this is
  achievable via a custom `*http.Client`; promote to first-class
  when a real deployment wants it.
- **OAuth/JWT translation**: gateway exchanges the inbound token
  for a service-specific token via a configurable issuer. Heavier;
  composable today, first-class when needed.

### Admin auth follow-ups

Auto-internal `_*` namespaces and admin auth metrics shipped
(commit `01b1a3a`). Remaining:
- *Destructive read opt-in.* `AdminMiddleware` lets every GET
  through for the UI. Once a destructive read shows up
  (`/admin/peers/{id}/inspect-state` etc.), gate it explicitly via
  a per-route flag rather than flipping the global GET policy.
  Parked until a real destructive read needs it.

### OpenAPI oneOf / anyOf → GraphQL Union

Currently falls back to a JSON scalar. GraphQL Union supports the
common case (each variant is an Object with a known name). When all
variants in a `oneOf` resolve to known objects, emit a Union;
otherwise keep the JSON scalar fallback.

Edge cases:
- Discriminator field → resolver picks the variant.
- Inline objects without `$ref` → synthesise type names.

---

## Tier 3 — operational polish

- **Connection-rate limiting / per-IP caps.** Reject excessive new WS
  connections per IP / per token. Prevents trivial DoS on the
  WebSocket terminator. Use a token bucket; configurable knob.
- **k8s + docker-compose example deployments.** YAML manifests for the
  3-gateway cluster from `examples/multi`. Shows how to wire `--nats-peer`,
  health probe, drain-on-shutdown.
- **NATS server log noise control.** Currently routes log everything
  at INFO (tests pile up server-banner output too). Expose a
  `Logger`/`LogLevel` field on `ClusterOptions` and a corresponding
  `--nats-log-level` CLI flag.
- **Metrics / tracing example middleware.** Concrete `Pair` showing
  OpenTelemetry / Prometheus-app-level integration on top of what
  the gateway already emits.
- **Cluster.Close vs Gateway.Close lifecycle docs.** Document the
  shutdown sequence: `gw.Drain` → `srv.GracefulStop` →
  `cluster.Close`. Out-of-order calls are OK but the example should
  show the right sequence.
- **Heartbeat-to-wrong-gateway smoothing.** When a service heartbeats
  to a gateway that didn't receive its Register, fall back to checking
  the registry KV instead of forcing re-register. Smaller window of
  dispatch failure during gateway failover.
- **Sub-fanout drop policy configurable.** Today a slow consumer drops
  events. Operator might want "kick the slow one" instead. Per-consumer
  watermark + configurable behaviour.

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

UI-side tier-2 work is tracked under the Tier 2 heading: token
entry/storage, dist embed.

## Recently shipped

(Last n commits worth knowing about for context. Update on commit; trim
older entries when they get stale.)

- `5f03354` downstream-GraphQL multi-replica + load
  balancing. Mirrors `dfae181` for OpenAPI: one source per
  namespace, N replicas. `graphQLSource` collapses
  `endpoint`+`httpClient`+`owner` into
  `replicas atomic.Pointer[[]*graphQLReplica]` plus a `pickHint`
  for round-robin tiebreaking; `graphQLReplica` carries id,
  endpoint, owner, httpClient, inflight. `pickReplica` returns
  lowest-in-flight (round-robin among ties so serial low-traffic
  dispatch spreads across replicas instead of stacking on the
  first). Same hash → idempotent multi-replica add; mismatched
  hash still rejects. Granular replica delete via
  `removeGraphQLReplicaByIDLocked` (source dies when last replica
  leaves) for the cluster reconciler path; owner-based eviction
  via `removeGraphQLSourcesByOwnerLocked` for standalone
  Deregister + boot rollback. The forwarding resolver in
  `graphql_mirror.go` now picks per call and inflight-bumps with
  defer, the same shape as the OpenAPI resolver.
  control.Register's standalone-rollback path simplified to
  owner-walk every kind of source instead of tracking
  per-namespace add lists. 1 new test
  (`TestDynamicGraphQL_MultiReplica`): two backends, alternating
  dispatch over 10 calls, deregister-A → all 5 follow-ups land
  on B, replicaCount goes 2→1.
- `2afa1d5` downstream-GraphQL dynamic registration over the
  control plane. Mirrors `784430a` for OpenAPI:
  `ServiceBinding.graphql_endpoint` (proto field 6) joins
  `file_descriptor_set` and `openapi_spec` as a third mutually-
  exclusive form. Receiving gateway runs the canonical introspection
  query at Register time, hashes the bytes, and writes both the
  endpoint and the introspection JSON into the registry KV value
  (`registryValue.GraphQLEndpoint` + `GraphQLIntrospection`); other
  peers' reconcilers parse from the cached bytes — no re-fetch.
  `addGraphQLSourceLocked` is idempotent under hash equality (mirror
  of `addOpenAPISourceLocked`). `removeGraphQLSourceLocked` /
  `removeGraphQLSourcesByOwnerLocked` cover Deregister and reconciler
  delete. `controlclient.Service` gains `GraphQLEndpoint string`,
  mutually exclusive with `FileDescriptor` / `OpenAPISpec` (validated
  client-side too). 5 new tests in `dynamic_graphql_test.go`:
  standalone register → query → deregister; hash mismatch on second
  register; three-form set rejection; namespace-required (no fallback
  for GraphQL); cluster cross-gateway dispatch (register on A,
  dispatch from B). Existing `TestGraphQLIngest_DuplicateNamespaceRejected`
  renamed to `_Idempotent` to match the new behavior (same-hash
  re-register is a no-op).
- `45c0cd4` `SignSubscriptionToken` RPC kid in/out. Closes
  the rotation story for centrally-signed tokens (the open
  follow-up after `325aaf4`). `SignSubscriptionTokenRequest` gains
  `string kid = 3`; `SignSubscriptionTokenResponse` gains
  `string kid = 5`. Handler routes through the same
  `lookupSecret(kid)` helper as the verifier — empty kid +
  configured Secret stays on the legacy single-key payload (back-
  compat); non-empty kid signs the rotated payload via
  `computeSubscribeHMAC(secret, kid, channel, ts)`. UNKNOWN_KID is
  surfaced when the gateway has no entry for the requested kid.
  Admin huma route (`signIn` / `signOut` JSON shapes) and the CLI
  `sign` subcommand both gained `--kid` (input) + `kid=` (output);
  CLI's local-sign path now uses `SignSubscribeTokenWithKid`. 3
  new unit tests in `auth_subscribe_test.go` cover RPC happy path
  (rotated kid round-trips through verify), UNKNOWN_KID, and the
  legacy default-kid back-compat path.
- `325aaf4` token rotation (kid in HMAC tokens). Verifier +
  standalone signer half. `SubscriptionAuthOptions.Secrets
  map[string][]byte` joins the legacy `Secret []byte`; verifier
  reads an optional `kid: String` arg from subscribe payloads,
  resolves it via `lookupSecret(kid)` (legacy `Secret` wins for
  kid=""; `Secrets[kid]` otherwise), and emits `UNKNOWN_KID` when
  unmapped. HMAC payload bound to kid: empty kid stays on the
  legacy `<channel>\n<ts>` payload (back-compat); non-empty kid
  signs `<kid>\n<channel>\n<ts>` so a token can't be replayed by
  changing the kid arg on the wire. SDL gains optional `kid: String`
  on every Subscription field. New public helper
  `SignSubscribeTokenWithKid(secret, kid, channel, ttl) (hmacB64,
  kidOut, ts)`; the original `SignSubscribeToken` is preserved as
  a kid="" wrapper. The `SignSubscriptionToken` RPC continues to
  sign with the default secret (kid="") for back-compat — extending
  the RPC proto with `kid` in/out is the documented follow-up.
  7 new unit tests in `auth_subscribe_test.go`: legacy still
  verifies, rotated verifies, unknown kid → UNKNOWN_KID, cross-kid
  replay → SIGNATURE_MISMATCH, legacy + rotated coexist, no-secret
  → NOT_CONFIGURED, kid wrong type → MALFORMED.
- `2c1f58d` downstream-GraphQL backpressure. Closes the
  per-source semaphore parity with the proto and OpenAPI paths.
  `graphQLSource` gains `sem chan struct{}` + `queueing
  atomic.Int32`, sized at registration from
  `BackpressureOptions.MaxInflight` (gateway-wide config — same
  knob OpenAPI / proto pools use). Forwarding resolver acquires
  before any AST work, observes dwell + queue-depth, fast-rejects
  with `Reject(ResourceExhausted)` when `MaxWaitTime` expires
  (back-stamped through the same `record` closure that fires
  `RecordDispatch`). Dwell / queue-depth / backoff metrics fire
  with `kind="unary"`, version `"v1"`. `BackpressureOptions` is
  passed through from `Gateway.buildGraphQLFields` →
  `newGraphQLMirror` so the resolver closure has it without
  reaching back into the gateway. 1 new test
  (`TestGraphQLIngest_BackpressureTimesOutAndRejects`): blocking
  backend + `MaxInflight=1` + `MaxWaitTime=50ms` → second
  concurrent dispatch rejects with RESOURCE_EXHAUSTED. Same shape
  as `TestOpenAPIE2E_BackpressureTimesOutAndRejects`.
- `448d86b` downstream-GraphQL dispatch metric + error
  classification. Mirrors the OpenAPI pair (`e88d158`/`cf115c1`):
  `graphQLMirror` now carries a `Metrics` reference (passed in via
  `g.cfg.metrics`) and `forwardingResolver` records start time +
  calls `metrics.RecordDispatch(ns, "v1", "<query|mutation>
  <remoteFieldName>", elapsed, err)` on every exit (no-AST,
  printer error, dispatch return, remote-errors envelope, decode
  failure). `dispatchGraphQL` and the no-FieldASTs / printer / no-
  data branches now return `Reject(Code, msg)` so the `code` label
  on the dispatch metric (and GraphQL `extensions.code`) reflects
  the failure shape: HTTP statuses go through the same
  `httpStatusToCode` helper added for OpenAPI; remote GraphQL
  errors classify as `INTERNAL` (no portable status in the GraphQL
  error envelope); transport / decode → `INTERNAL`; marshal /
  request-build → `INVALID_ARGUMENT`. Histogram help text +
  `Metrics.RecordDispatch` godoc updated to call out the third
  dispatch path. 2 new tests in `graphql_ingest_test.go`:
  `TestGraphQLIngest_RecordDispatchFires` (label parity) and
  `TestGraphQLIngest_ErrorClassification` (HTTP 401 / 404 / 500 +
  remote-errors envelope).
- `cf115c1` OpenAPI dispatch error classification.
  `dispatchOpenAPI` and the resolver's no-live-replicas branch now
  return `Reject(Code, msg)` instead of plain `fmt.Errorf` so
  `classifyError` (used by both `RecordDispatch`'s `code` label and
  GraphQL `extensions.code`) picks up a meaningful enum: HTTP 400 →
  `INVALID_ARGUMENT`, 401 → `UNAUTHENTICATED`, 403 →
  `PERMISSION_DENIED`, 404 → `NOT_FOUND`, 429 →
  `RESOURCE_EXHAUSTED`, 5xx → `INTERNAL`, transport / decode /
  no-live-replicas → `INTERNAL`, body-marshal / request-build →
  `INVALID_ARGUMENT`. New `httpStatusToCode` helper lives in
  `openapi.go` with a comment pointing at gRPC's HTTP-mapping
  conventions. 2 new tests
  (`TestOpenAPIE2E_ErrorClassification` table-test across 7 status
  codes, `TestOpenAPIE2E_TransportErrorClassifiesAsInternal`).
- `e88d158` OpenAPI dispatch RecordDispatch parity. Resolver
  in `buildOpenAPIField` records `start := time.Now()` and calls
  `metrics.RecordDispatch(ns, "v1", "<METHOD> <pathTemplate>",
  elapsed, err)` on every exit path — backpressure rejection, no-
  live-replicas, and the dispatchOpenAPI return — so
  `go_api_gateway_dispatch_duration_seconds` covers HTTP sources, not
  just gRPC pools. Histogram help text + `Metrics.RecordDispatch`
  godoc updated to reflect dual-transport coverage. 1 new test
  (`TestOpenAPIE2E_RecordDispatchFires`): asserts label parity
  (namespace="test", version="v1", method="GET /things/{id}") and
  err nil/non-nil across happy + 500 paths.
- `340f73b` `/schema/graphql` selector support. The endpoint
  now accepts the same `?service=ns[:ver][,...]` grammar as
  `/schema/proto` and `/schema/openapi`. Refactored
  `assembleLocked` into `buildSchemaLocked(filter schemaFilter)` —
  unfiltered builds populate `g.schema` (cached, atomic swap);
  filtered requests build a fresh schema per call and discard.
  `schemaFilter.matchPool(poolKey)` covers proto pools (ns + ver);
  `schemaFilter.matchNS(ns)` covers OpenAPI / downstream-GraphQL
  sources (no version axis). Filter threaded through
  `buildOpenAPIFields`, `buildGraphQLFields`,
  `buildSubscriptionFields`. 1 new test
  (`TestSchemaHandler_ServiceSelectorFiltersSDL` in
  `schema_rebuild_test.go`): two protos registered, unfiltered SDL
  carries both, `?service=greeter` carries only greeter, malformed
  selector → 400.
- `cc44855` OpenAPI HTTP backpressure. Per-source semaphore +
  queue gauge mirroring the proto pool path: `openAPISource` gains
  `sem chan struct{}` + `queueing atomic.Int32`, sized by
  `BackpressureOptions.MaxInflight` (gateway-wide config — same knob
  as proto pools). Resolver acquires before `pickReplica`,
  fast-rejects with `Reject(ResourceExhausted)` when `MaxWaitTime`
  expires. Dwell + queue-depth + backoff metrics fire under the
  same labels as the proto path (`kind="unary"`, version pinned to
  `v1` since OpenAPI sources have no version axis). Method label is
  `<HTTP_METHOD> <pathTemplate>`. 1 new test
  (`TestOpenAPIE2E_BackpressureTimesOutAndRejects` in
  `openapi_test.go`): blocking backend + `MaxInflight=1` +
  `MaxWaitTime=50ms` → second concurrent dispatch rejects with
  RESOURCE_EXHAUSTED.
- `3517273` downstream GraphQL ingestion (boot-time, queries +
  mutations). `gw.AddGraphQL(endpoint, opts...)` runs the canonical
  introspection query, parses into a typed model, mirrors every
  custom type with a `<ns>_` prefix (built-in scalars stay
  unprefixed), and registers Query/Mutation fields. Forwarding
  resolver rewrites the AST to drop the prefix from the top-level
  field name, prints via graphql-go's printer, and POSTs to the
  remote with the same auth/header pass-through OpenAPI uses.
  Interfaces/Unions fall back to a JSON scalar (logged); subscriptions
  not yet supported. New files: `graphql_ingest.go`,
  `graphql_introspect.go`, `graphql_mirror.go`. 4 new tests
  (`graphql_ingest_test.go`).
- `dfae181` OpenAPI multi-replica + load balancing.
  `openAPISource.baseURL` and `httpClient` collapse into a slice of
  `openAPIReplica`s (atomic.Pointer like proto pools), each with its
  own baseURL + in-flight counter + per-replica client.
  `pickReplica` picks lowest in-flight with a round-robin tiebreaker
  (serial low-traffic dispatch now spreads across replicas instead
  of stacking on the first). Multi-replica registration: identical
  spec hash + different addr appends a new replica; mismatched hash
  still rejects. Granular reconciler delete (per replicaID); source
  dies when last replica leaves. 1 new test
  (`TestDynamicOpenAPI_MultiReplica`): two backends, alternating
  dispatch, deregister-A → all traffic shifts to B.
- `01b1a3a` admin-auth follow-ups pack: (1) auto-internal
  `_*` namespaces — anything starting with underscore is hidden
  from the public schema regardless of `AsInternal()`. Centralised
  via a new `g.isInternal(ns)` helper; the explicit map still
  works. (2) `go_api_gateway_admin_auth_total{method, outcome}`
  counter mirroring the subscribe-auth metric. Outcomes:
  `ok_delegate` / `ok_bearer` / `denied_delegate` / `denied_bearer`
  / `no_token_configured`. Public reads never increment.
  Destructive-read opt-in remains parked. 2 new tests
  (`schema_rebuild_test.go`, `auth_admin_test.go`).
- `784430a` dynamic OpenAPI registration through the
  control plane. New `ServiceBinding.openapi_spec` (proto field 5)
  alongside the existing `file_descriptor_set`; mutually exclusive.
  `controlPlane.Register` routes by which form was sent —
  proto-shaped goes through the existing pool / dial path, OpenAPI
  goes through new `addOpenAPISourceLocked` (idempotent under hash
  equality). Cluster mode: spec bytes ride in the registry KV
  value alongside the FileDescriptorSet field; `reconciler.handlePut`
  detects via the new `registryValue.IsOpenAPI()` and creates the
  source on every peer. `controlclient.Service` gains an
  `OpenAPISpec []byte` field so external services can self-register
  OpenAPI bindings the same way they register proto. 5 new tests
  (`dynamic_openapi_test.go`) covering standalone, hash-mismatch,
  both-set + neither-set rejection, and cluster cross-gateway.
- `cc57458` `WithOpenAPIClient(*http.Client)` gateway option
  + `OpenAPIClient(c)` per-source `ServiceOption`. Per-source beats
  gateway-wide; both fall back to `http.DefaultClient`. Threaded
  through `dispatchOpenAPI`. Closed the cheapest tier-2 outbound-
  auth fork.
- `58b6ff9` admin\_events end-to-end: `adminevents/v1` proto
  (`AdminEvents.WatchServices` server-streaming) + `gw.AddAdminEvents()`
  registers it under `admin_events/v1`. Registry hooks publish
  `ServiceChange` to `events.admin_events.WatchServices.<ns>` on
  every join/leave; the existing NATS pub/sub fanout delivers it to
  WebSocket subscribers as `admin_events_watchServices`. The UI's
  `EventsProvider` auto-subscribes; new toasts pop bottom-right and
  events also land in the tray. Fixed two pre-existing bugs uncovered
  by this work: schema rebuild crashed when a namespace had only
  streaming RPCs (now skips the empty Query sub-object), and proto
  enums were serialised as strings but graphql-go matches enum
  values by typed equality (now returns int32). 2 new tests
  (`admin_events_test.go`) + 1 WS e2e
  (`subscriptions_test.go::AdminEventsWatchServices`).
- `70ebaf2` UI events provider. `<EventsProvider>` wraps the
  Layout, holds a graphql-ws client (`ui/src/api/events.ts`) and a
  global ring buffer (50 events). Pages opt in via
  `useSubscribe({ id, query, variables, onData })`. Bell icon in
  the AppBar with unread badge opens an `EventsTray` drawer that
  renders the feed (subject, timestamp, JSON payload preview, error
  framing). Lazy WS — connection opens on first subscribe, closes
  after the last unsubscribe. graphql-ws@6 dependency added.
- `3968c69` More huma admin routes: `GET /admin/channels`
  (active subscription subjects + per-subject consumer count via
  new `gw.ActiveSubjects()`), `POST /admin/drain` (operator-driven
  graceful drain with configurable timeout, requires bearer),
  `GET /admin/openapi.json` (huma's OpenAPI spec repointed under
  /admin/ so it's reachable via the gateway's mount). All
  self-ingest as `admin_*` GraphQL fields automatically. 5 new
  tests in `admin_huma_test.go`.
Older commits (`/api/*` split + embedded UI bundle `06b1fc2`, UI admin
token entry `813b055`, cluster cross-gateway dispatch e2e `778559d`,
ForgetPeer tests `9f498eb`, schema rebuild tests `4a5b203`, gRPC
unary dispatch e2e `aabdc21`, subscription e2e, OpenAPI dispatch e2e,
AdminAuthorizer delegate, ForwardHeaders, admin boot-token, huma
self-ingest, schema export family, AddOpenAPI ingestion, /health +
Drain, graphql-ws transport + NATS bridge, cluster KV, peers KV,
embedded NATS, mTLS, schema diff, Prometheus, etc.) are in the git
log — they're not referenced by current decisions any more.
