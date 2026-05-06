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

Ordered by current leverage. The top three unblock real
deployments; the rest fill in design-completing features the
gateway claims to support.

### More huma admin routes — done

`admin_huma.go` now covers `peers`, `services`, `forgetPeer`,
`signSubscriptionToken`, `listChannels`, `drain`, and exposes the
huma OpenAPI spec at `/admin/openapi.json`. The latter three landed
in this round (see Recently Shipped). All operations self-ingest
into GraphQL automatically as `admin_*` fields via the same
OpenAPI ingestion path.

### Outbound auth pass-through alternatives

`Authorization` is forwarded by default; `ForwardHeaders(...)`
ServiceOption replaces the allowlist per source.
`WithOpenAPIClient(*http.Client)` (gateway-wide default) and
`OpenAPIClient(c)` (per-source override) let operators plug in any
transport — mTLS, custom RoundTripper for service-account token
injection, signed-URL rewriting, retry/timeout policy. That covers
the common cases without committing to a specific auth model.

Open design forks for richer cases:
- **Service-account token**: a built-in helper that wraps a
  RoundTripper and refreshes a token on schedule. Today this is
  achievable via a custom `*http.Client`; promote to first-class
  when a real deployment wants it.
- **OAuth/JWT translation**: gateway exchanges the inbound token
  for a service-specific token via a configurable issuer. Heavier;
  same story — composable today, first-class when needed.

### Dynamic OpenAPI registration over control plane — done

`ServiceBinding.openapi_spec` lets services register OpenAPI specs
dynamically through the same control plane proto bindings used.
The gateway detects which form was sent, hashes the spec
(SHA256 over raw bytes), writes the value to the registry KV, and
each gateway's reconciler picks it up — local on the receiver,
remote on the cluster peers.

`controlclient.Service` accepts either `FileDescriptor` or
`OpenAPISpec`. Single source per namespace in v1; multi-version /
multi-replica OpenAPI is the next item below.

### Downstream GraphQL ingestion

Stitch existing GraphQL services into our schema:

```go
gw.AddGraphQL("https://pets-svc/graphql", gateway.As("pets"))
```

Boot-time introspection → namespace-prefixed types (`pets_User`) →
forward original sub-query string to downstream resolver. ~300 LoC.

Subscriptions: forward via graphql-ws WebSocket dial to downstream.
Multiplex one upstream WS per (gateway, downstream-service).

**Not federation** — pure delegation. Federation v2 entity-merging
deferred to never unless multiple services need to contribute fields
to the same entity, which most teams don't actually have.

Implementation notes:
- Custom introspection client (small, focused) over
  `graphql-go/graphql`.
- Forwarding resolver captures `rp.Info.Operation` and reconstructs
  query string (or just forwards the raw HTTP body).
- Type prefixing: every introspected type renamed `<ns>_<TypeName>`.
- Auth/header pass-through follows OpenAPI auth design.

### Token rotation (kid in tokens)

Gateway accepts N secrets keyed by id. Token format becomes
`base64(kid || hmac)` or carries `kid` as a separate arg. Operator
adds a new key, old keys remain valid until their lifetime expires.

Implementation notes:
- New `SubscriptionAuthOptions.Secrets map[string][]byte`.
- HMAC computation: include kid in the signed payload so swapping kid
  doesn't allow token replay across keys.

### Admin auth follow-ups

Three loose ends inherited from the admin-auth tier-1 work that
landed; none blocks anything but each cleans up a sharp edge.
- *Destructive read opt-in.* `AdminMiddleware` lets every GET
  through for the UI. Once a destructive read shows up
  (`/admin/peers/{id}/inspect-state` etc.), gate it explicitly via
  a per-route flag rather than flipping the global GET policy.
- *Auto-internal underscore namespaces.* `_events_auth` and
  `_admin_auth` rely on operators passing `AsInternal()` at
  registration. Auto-flagging any `_*` namespace as internal would
  prevent accidental schema leaks.
- *Admin auth metrics.* No
  `go_api_gateway_admin_auth_total{code,...}` counter today;
  subscriptions already have one. Mirror it for delegate decisions
  and bearer outcomes when an operator wants visibility into who's
  getting denied.

### OpenAPI multi-replica + load balancing

Each registered OpenAPI spec currently takes one base URL. For
multi-replica: store N base URLs per pool entry, use the existing
least-in-flight `pickReplica` mechanism for HTTP just as for gRPC.

Implementation notes:
- HTTP `pickReplica` analogue: track in-flight per URL, lowest wins.
- Conn pool: `http.Client` per (pool, replica) — reuses keep-alive.
- Backpressure: same MaxInflight/MaxWaitTime applies.

### OpenAPI oneOf / anyOf → GraphQL Union

Currently falls back to a JSON scalar. GraphQL Union supports the
common case (each variant is an Object with a known name). When all
variants in a `oneOf` resolve to known objects, emit a Union;
otherwise keep the JSON scalar fallback.

Edge cases:
- Discriminator field → resolver picks the variant.
- Inline objects without `$ref` → synthesise type names.

### `/schema/graphql` selector support

`/schema/proto` and `/schema/openapi` accept `?service=ns:ver,...`
filters; `/schema/graphql` returns the whole schema regardless.
Requires a filtered schema-build path. Not difficult, just hasn't
been needed — codegen consumers always want the whole thing.

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
  `--nats-log-level` CLI flag. Subsumes the tier-1 test-side
  cosmetic blocker.
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

Where to crib from when adding tests:

- `auth_admin_test.go` — AdminMiddleware read/write split, token
  persistence, header-forwarding allowlist (unit-level helper).
- `auth_admin_delegate_test.go` — AdminAuthorizer delegate
  (no-delegate, OK, DENIED, UNAVAILABLE, transport error, public
  reads bypass, request fields). Uses an in-process
  `grpc.ClientConnInterface` fake — no real gRPC server.
- `openapi_test.go` — full GraphQL → HTTP dispatch round-trip via
  httptest backend: GET path params, POST request body,
  Authorization forwarding default, `ForwardHeaders` override,
  backend error surfacing.
- `grpc_dispatch_test.go` — full GraphQL → gRPC dispatch
  round-trip: real `grpc.Server` on `127.0.0.1:0` implementing
  GreeterService.Hello, registered via `AddProtoDescriptor`, queries
  through `gw.Handler()`. Covers happy path, v1 sub-object, backend
  error, no-live-replicas.
- `schema_rebuild_test.go` — pool create rebuilds schema (greeter
  field appears); pool destroy via `removeReplicasByOwnerLocked`
  rebuilds (field disappears); hash mismatch on second registration
  errors; same-hash second registration joins the pool (replica
  count = 2); multiple namespaces coexist; `AsInternal()` hides
  the namespace from Query but keeps the pool dispatchable.
- `forget_peer_test.go` — single-node cluster, manual peer KV
  manipulation. Covers: alive-rejection (peer present in KV),
  happy path (peer expired/deleted → Removed=true), no-op for
  never-registered peer, refuse-self, empty node_id, standalone
  gateway (no cluster) errors with "cluster not configured".
- `cluster_dispatch_test.go` — two `StartCluster` instances peering
  via NATS routes; A receives a Register call, B's reconciler
  picks it up via the registry KV and dials the greeter. GraphQL
  query through B's `gw.Handler()` reaches the greeter (registered
  on A) and returns the right payload. The same query through A
  also works.

**Lifetime gotcha:** `startClusterTracking(ctx)` captures `ctx` as
the parent of the long-running watch + reconciler goroutines. Test
helpers must pass `context.Background()` (not a `WithTimeout`) so
those goroutines outlive the helper return. Cleanup runs through
`gw.Close → tracker.stop`. Burned ~30 minutes diagnosing this in
the cross-gateway test — the symptom was "registry KV has the key
on both nodes but B's reconciler never creates the pool".
- `subscriptions_test.go` — full WebSocket round-trip via embedded
  NATS (`StartCluster` with ephemeral ports + tempdir): happy-path
  publish → next frame; HMAC SIGNATURE_MISMATCH / TOO_OLD;
  NOT_CONFIGURED; client `complete` cleans up the broker entry.

Pattern: httptest + `gw.Handler()` for OpenAPI/subscription/gRPC;
fakes or direct helper calls for unit-shape. Every new feature
should add same-shape coverage.

### Schema export family

Three sibling endpoints under `/schema/*`, each accepting a
`?service=ns[:ver][,...]` selector (selector applies to proto and
openapi; graphql currently returns the whole schema — see Tier 2
*`/schema/graphql` selector support*):

- `GET /schema/graphql` — SDL (default) or introspection JSON via
  `?format=json`. Derived from registered protos + OpenAPI.
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

- *(uncommitted)* dynamic OpenAPI registration through the
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
- `06b1fc2` `/api/*` split + embedded UI bundle. Library:
  `gateway.UIHandler(fs.FS)` for SPA serving with index.html
  fallback. New `ui` package embeds `ui/dist/` via `//go:embed
  all:dist`. Example: all gateway routes moved under `/api/*`,
  `/` serves the UI; unmatched `/api/*` returns JSON 404 so
  client typos don't render the SPA. UI updated to use `/api/...`
  everywhere (vite proxy, GraphQLClient endpoint, schema fetch,
  pnpm schema curl). Pattern lifted from zdx-go. Closes both the
  tier-2 *Embed UI bundle* and the implicit "single-binary
  deploy" goal.
- `813b055` UI admin token entry: `ui/src/api/auth.ts`
  (sessionStorage-backed store), `ui/src/components/SettingsDrawer.tsx`
  (paste/save/clear UI), `useAdminToken()` hook, lazy Authorization
  header in `client.ts`, gear icon + "no token" badge dot in the
  AppBar. Closed the tier-2 "Forget button 401s" item.
- `778559d` cluster cross-gateway dispatch e2e
  (`cluster_dispatch_test.go`): two `StartCluster` instances
  peering on free TCP ports; A receives Register, B's reconciler
  picks it up via the registry KV, and a GraphQL query through B
  reaches the greeter registered on A. Closed the tier-1
  test-coverage gap. Also: applied the lifetime-context fix to
  `forget_peer_test.go` (helpers were passing 10s ctx into
  `startClusterTracking`, which would have killed long-running
  reconciler/watch goroutines mid-test).
- `9f498eb` ForgetPeer tests (`forget_peer_test.go`): 6
  cases against a single-node cluster, manipulating the peers KV
  directly. Covers the alive-rejection / happy-path / refuse-self
  flow plus the standalone "no cluster configured" error.
- `4a5b203` schema rebuild tests (`schema_rebuild_test.go`):
  6 cases verifying that pool create/destroy/hash-collision flow
  through `assembleLocked` correctly. Pure package-level; no NATS.
- `aabdc21` gRPC unary dispatch e2e (`grpc_dispatch_test.go`):
  in-process `grpc.Server` on `127.0.0.1:0` implementing
  GreeterService.Hello, registered via `AddProtoDescriptor`, queries
  through `gw.Handler()`. Covers happy path, v1 sub-object dispatch,
  backend error surfacing, drained-pool no-live-replicas error.
- `1f85546` subscription e2e via embedded NATS + WebSocket.
  `StartCluster` on ephemeral ports + tempdir; greeter registered
  via `AddProtoDescriptor`; happy-path publish → next frame, HMAC
  SIGNATURE_MISMATCH / TOO_OLD, NOT_CONFIGURED, client-`complete`
  broker cleanup. First tests exercising NATS + WebSocket together.
- `4346c12` AdminAuthorizer delegate (`adminauth/v1`) + wiring
  in `AdminMiddleware`. Service registers under `_admin_auth/v1`;
  delegate consulted first, boot token is the fallback. Tests cover
  no-delegate, OK accept, DENIED short-circuit, UNAVAILABLE
  fall-through, transport error → boot token still works, reads
  remain public. Boot token is non-negotiable: a misbehaving
  delegate cannot lock operators out.
- `299c0ee` OpenAPI dispatch round-trip e2e tests
  (`openapi_test.go`): httptest backend + `gw.Handler()`; covers
  GET, POST-with-body, Authorization forwarding, ForwardHeaders
  override, backend-error → graphql-error.
- `f0cfe46` `ForwardHeaders(...)` ServiceOption + first
  package-level tests (`auth_admin_test.go`). Replaces the static
  global header allowlist with per-source allowlist.
- `5bf7cdf` admin boot-token auth + `Authorization` forwarding on
  OpenAPI dispatch. Token persists to `<adminDataDir>/admin-token`;
  reads public, writes require bearer.
- `df56e35` huma self-ingest of admin routes (the dogfood; the
  surface admin\_\* GraphQL fields are built on).
- `f9b30dd` schema export family `/schema/{graphql,proto,openapi}`.
- `dc5e0f7` `AddOpenAPI` ingests OpenAPI 3.x specs into the GraphQL
  schema.
- `be4e832` `/health` + `Gateway.Drain` for rolling deploys.
- `292c16f` graphql-ws WebSocket transport + NATS bridging for
  subscriptions.

Older commits (cluster KV, peers KV, embedded NATS, ForgetPeer,
mTLS, schema diff, Prometheus, hybrid stream caps, HMAC verify,
SignSubscriptionToken, etc.) are in the git log — they're not
referenced by current decisions any more, so they've been trimmed
here.
