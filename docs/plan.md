# go-api-gateway: roadmap & decisions

Source of truth for open work, design decisions, and the rationale
behind both. Read at session start. Update whenever scope shifts.

Tier 1 = correctness / production-blocking. Tier 2 = design-completing
features. Tier 3 = operational polish. Known limitations = called out
intentionally; not currently planned to fix.

---

## Tier 1 — load-bearing

### Gateway admin auth — done

Boot token + pluggable delegate both shipped (see Recently Shipped).
The fall-through priority is delegate → boot token; OK accepts,
DENIED rejects without falling through, anything else (UNAVAILABLE,
NOT_CONFIGURED, transport error) falls through so the always-works
hatch keeps operators unlocked.

**Optional follow-ups (tier-2-ish, no urgency):**
- *Destructive read opt-in.* `AdminMiddleware` lets every GET
  through for the UI. Once a destructive read shows up
  (`/admin/peers/{id}/inspect-state` etc.), gate it explicitly via
  a per-route flag rather than flipping the global GET policy.
- *Auto-internal underscore namespaces.* `_events_auth` and
  `_admin_auth` rely on operators passing `AsInternal()` at
  registration. Auto-flagging any `_*` namespace as internal would
  prevent accidental schema leaks.
- *Admin auth metrics.* No `go_api_gateway_admin_auth_total{code,...}`
  counter today; subscriptions already have one. Mirror it for
  delegate decisions and bearer outcomes when an operator wants
  visibility into who's getting denied.

### Outbound auth pass-through to OpenAPI services

**Different concern.** This is *not* gateway auth. This is: when the
gateway dispatches to an OpenAPI-registered backend
(`AddOpenAPI("...")`), how does the *backend* authenticate the
gateway-originated call? Real services need bearer tokens, mTLS
client certs, session cookies, signed URLs. The boot-token model
above is unrelated; it lives entirely on the gateway's inbound side.

**Status:** v1 of this is shipped — `Authorization` is forwarded
unconditionally on every outbound OpenAPI dispatch (see
`forwardedOpenAPIHeaders` in `openapi.go`). That covers the dogfood
case (admin\_\* GraphQL mutations forward the bearer to /admin/\*) and
the simplest external case (a backend that uses bearer auth and
trusts whatever the GraphQL caller sent). It is **not** sufficient
for backends that want a separate identity, header allowlist, or
client cert.

Remaining design forks (still open):
- **Configurable header pass-through**: per-source allowlist
  (`gateway.As("foo"), gateway.ForwardHeaders("Authorization",
  "X-Api-Key")`). Replaces the static list when set.
- **Service-account token**: gateway holds a credential per
  registered service and presents it on every outbound. Doesn't carry
  user identity; service does its own authz.
- **OAuth/JWT translation**: gateway exchanges the inbound token for
  a service-specific token via a configurable issuer. Heavier.
- **mTLS client certs**: gateway dials with a configured client cert
  per service. Reuse `LoadMTLSConfig` plumbing.
- **`WithOpenAPIClient(*http.Client)`**: operator-supplied transport
  for arbitrary out-of-band auth (signed URLs, custom retry, etc.).

Recommended next step: per-source `ForwardHeaders(...)` ServiceOption.
Cheapest win, unblocks multi-backend deployments without committing
to a delegate or token-exchange model.

### Automated tests

**Seed in place:**
- `auth_admin_test.go` — AdminMiddleware read/write split, token
  persistence, header-forwarding allowlist (unit-level helper).
- `auth_admin_delegate_test.go` — AdminAuthorizer delegate
  (no-delegate, OK, DENIED, UNAVAILABLE, transport error, public
  reads bypass, request fields). Uses an in-process
  grpc.ClientConnInterface fake — no real gRPC server.
- `openapi_test.go` — full GraphQL → HTTP dispatch round-trip via
  httptest backend: GET path params, POST request body,
  Authorization forwarding default, `ForwardHeaders` override,
  backend error surfacing.
- `subscriptions_test.go` — full WebSocket round-trip via embedded
  NATS (`StartCluster` with ephemeral ports + tempdir): happy-path
  publish → next frame; HMAC SIGNATURE_MISMATCH and TOO_OLD;
  NOT_CONFIGURED; client `complete` cleans up the broker entry.

Pattern: httptest + `gw.Handler()` for OpenAPI/subscription; fakes
or direct helper calls for unit-shape. Every new feature should add
same-shape coverage.

**Still missing** (every entry is silent-regression risk):
- Multi-replica + version e2e (`examples/multi`-style scripted)
  exercising gRPC unary dispatch.
- Cluster mode 2-node with cross-gateway dispatch.
- ForgetPeer happy path + alive-rejection.
- Schema rebuild on pool create/destroy + pool-hash collision
  rejection.
- Quiet the embedded-NATS log spam in tests — `ConfigureLogger`
  uses defaults; either expose a `Logger` field on `ClusterOptions`
  or capture stderr in `newSubFixture`. Cosmetic, not blocking.

---

## Tier 2 — design-completing

### Dynamic OpenAPI registration over control plane

`AddOpenAPI` is boot-time only. To register dynamically: extend
`ServiceBinding` proto with an optional `openapi_spec` field; gateway
detects which form was sent. Same registry KV story; spec hash gates
collisions.

Implementation notes:
- New proto field: `bytes openapi_spec = 5` on `ServiceBinding`.
- Either `file_descriptor_set` OR `openapi_spec` set, not both.
- Hash function: same canonical-marshal pattern (sort by path, etc.).
- Multi-replica: each replica advertises its own HTTP base URL.
- `controlclient` gains `RegisterOpenAPI(...)` helper.

### OpenAPI multi-replica + load balancing

Each registered OpenAPI spec currently takes one base URL. For
multi-replica: store N base URLs per pool entry, use the existing
least-in-flight pickReplica mechanism for HTTP just as for gRPC.

Implementation notes:
- HTTP `pickReplica` analogue: track in-flight per URL, lowest wins.
- Conn pool: `http.Client` per (pool, replica) — reuses keep-alive.
- Backpressure: same MaxInflight/MaxWaitTime applies.

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
- Custom introspection client (small, focused) over `graphql-go/graphql`.
- Forwarding resolver captures `rp.Info.Operation` and reconstructs
  query string (or just forwards the raw HTTP body).
- Type prefixing: every introspected type renamed `<ns>_<TypeName>`.
- Auth/header pass-through follows OpenAPI auth design.

### OpenAPI oneOf / anyOf → GraphQL Union

Currently falls back to a JSON scalar. GraphQL Union supports the
common case (each variant is an Object with a known name). When all
variants in a `oneOf` resolve to known objects, emit a Union; otherwise
keep the JSON scalar fallback.

Edge cases:
- Discriminator field → resolver picks the variant.
- Inline objects without `$ref` → synthesise type names.

### Token rotation (kid in tokens)

Gateway accepts N secrets keyed by id. Token format becomes
`base64(kid || hmac)` or carries `kid` as a separate arg. Operator
adds a new key, old keys remain valid until their lifetime expires.

Implementation notes:
- New `SubscriptionAuthOptions.Secrets map[string][]byte`.
- HMAC computation: include kid in the signed payload so swapping kid
  doesn't allow token replay across keys.

### Embed UI bundle into the gateway binary

`ui/dist/` (after `pnpm run build`) should be served by the gateway
itself so a single binary boots with everything. Recommended shape:
`gw.UIHandler(fs.FS) http.Handler` that the operator passes via
`embed.FS` (or a runtime `os.DirFS` for dev).

Example wiring:

```go
//go:embed ui/dist/*
var uiBundle embed.FS

mux.Handle("/", gw.UIHandler(uiBundle))   // SPA fallback to index.html
```

Codegen prerequisite: the dist/ bundle is the output of
`pnpm run gen && pnpm run build` against a running gateway, so the
UI's typed SDK matches the gateway's actual SDL.

### More huma admin routes

`admin_huma.go` covers `peers`, `services`, `forgetPeer`,
`signSubscriptionToken`. Useful additions, all backed by existing
in-process state:
- `GET /admin/channels` — list active subscription subjects from
  `subBroker.activeSubjectCount` etc.; useful for the UI's events
  page.
- `POST /admin/drain` — trigger graceful drain remotely (operator
  flow, not just SIGTERM). Returns when drain completes.
- `GET /admin/health` — currently public; could move under /admin
  once auth lands.
- `GET /admin/openapi.json` — re-emit the admin spec for tooling
  that wants to inspect it directly (huma already serves something
  similar at /openapi.json).

---

## Tier 3 — operational polish

- **Connection-rate limiting / per-IP caps.** Reject excessive new WS
  connections per IP / per token. Prevents trivial DoS on the
  WebSocket terminator. Use a token bucket; configurable knob.
- **k8s + docker-compose example deployments.** YAML manifests for the
  3-gateway cluster from `examples/multi`. Shows how to wire `--nats-peer`,
  health probe, drain-on-shutdown.
- **NATS server log noise control.** Currently routes log everything at
  INFO. Expose `--nats-log-level` flag and surface in the example.
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
- **Pool replica per gRPC service registered.** Each `Register` adds a
  replica; a service is one address. The `_events_auth` namespace and
  reconciler work this way too. No "one binary registering many
  protos with different lifecycles" support — bundle into one Register
  call.

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
| **`Authorization` forwarded unconditionally on OpenAPI dispatch** | Cheapest design that makes admin\_\* GraphQL mutations work end-to-end with one bearer. External backends that need a different identity will need the per-source `ForwardHeaders(...)` follow-up; revisit when a real use case shows up. |
| **AdminAuthorizer fall-through priority: delegate → boot token** | Boot token is the always-works emergency hatch. A delegate that crashes / DOS's / mis-deploys cannot lock operators out: UNAVAILABLE / transport error / NOT_CONFIGURED fall through. Only an explicit DENIED short-circuits. Operators can still get in with the on-disk token. |

---

## Schema export family

Three sibling endpoints under `/schema/*`, each accepting a
`?service=ns[:ver][,...]` selector (selector applies to proto and
openapi; graphql currently returns the whole schema):

- `GET /schema/graphql` — SDL (default) or introspection JSON via
  `?format=json`. Derived from registered protos + OpenAPI.
- `GET /schema/proto` — FileDescriptorSet (binary `application/protobuf`)
  with **gateway transformations applied**: hidden fields stripped via
  the `Pair.Hides` set; internal namespaces excluded. Not the raw
  registered protos — the contract surface as the gateway exposes it.
- `GET /schema/openapi` — JSON object keyed by namespace, re-emitting
  each ingested OpenAPI spec.

`/schema` (without sub-path) stays as a back-compat alias for SDL.

Selector grammar:
- `service=auth:v1,library` → auth at v1 + all versions of library.
- Missing version → all versions of that namespace.
- Missing service param → everything (subject to internal filtering).

Tier-2 follow-up: support selectors on `/schema/graphql` too. Requires
a filtered schema-build path; not difficult, just hasn't been needed.

## Dogfooding: huma → OpenAPI → GraphQL

The gateway's own admin operations are defined via huma
(`admin_huma.go`), mounted as plain HTTP at `/admin/*`, and
**self-ingested** at boot via `AddOpenAPIBytes`:

```go
adminMux, adminSpec, _ := gw.AdminHumaRouter()
mux.Handle("/admin/", adminMux)
gw.AddOpenAPIBytes(adminSpec,
    gateway.As("admin"),
    gateway.To("http://localhost:18080"))
```

Result: SDL gains flat `Query.admin_listPeers`,
`Query.admin_listServices`, `Mutation.admin_forgetPeer`,
`Mutation.admin_signSubscriptionToken`. Each huma handler
delegates to the existing `controlPlane` gRPC methods in-process
(no extra hop). External clients see one GraphQL surface; the
huma OpenAPI is the contract source.

This is the same path any external huma-defined service takes —
`gw.AddOpenAPI("https://service/openapi.json", gateway.To(...))`.
Dogfooding it for the gateway's own admin keeps the integration
path tested by the gateway itself.

`AddProtoDescriptor` survives for the gRPC-self-registration
case (e.g., expose a service whose proto is compiled into the
gateway binary) but the recommended path is huma + OpenAPI for
admin/operator surfaces.

## UI

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

Followups:
- Embed `dist/` via `embed.FS` so a single gateway binary serves
  the UI (tier-2 above).
- An "Events" page subscribing via graphql-ws to demo subscriptions.
- **Admin token entry / storage in the UI.** Now that `admin_*`
  mutations require a bearer, the UI needs a settings drawer that
  takes the token (paste from the boot log), stores it
  (sessionStorage — never persistence by default), and attaches it
  to graphql-codegen's fetcher as `Authorization: Bearer <hex>`.
  Without this, the Peers page's Forget button 401s.

## Recently shipped

(Last n commits worth knowing about for context. Update on commit; trim
older entries when they get stale.)

- *(uncommitted)* Subscription e2e tests (`subscriptions_test.go`):
  embedded NATS via `StartCluster` (ephemeral ports + tempdir),
  greeter proto registered via `AddProtoDescriptor`, full WebSocket
  round-trip through `gw.Handler()`. Covers happy path
  (publish → next frame), HMAC SIGNATURE_MISMATCH, HMAC TOO_OLD,
  NOT_CONFIGURED (no auth mode set), and client-`complete` broker
  cleanup. First tests in the repo that exercise the embedded NATS
  + WebSocket path together.
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
  restart reuses the same token. Reads are public, writes require
  bearer. Verified end-to-end in examples/multi.
- `df56e35` huma self-ingest of admin routes (the dogfood)
- `f5cf789` AddProtoDescriptor + UI scaffold + CLAUDE.md
- `4df3f80` decisions: proto/gRPC canonical for s2s
- `9b0a5bf` docs: docs/plan.md
- `f9b30dd` schema export family `/schema/{graphql,proto,openapi}`
- `dc5e0f7` AddOpenAPI ingests OpenAPI 3.x specs into the GraphQL schema
- `be4e832` /health endpoint + Gateway.Drain for rolling deploys
- `8f731a9` hybrid stream caps (per-pool + gateway-wide) + raised defaults
- `e091a18` surface auth code via extensions + sign CLI + README events
- `6ace0ca` subject-keyed NATS sub fanout + MaxStreams cap
- `31f80e0` SignSubscriptionToken + SubscriptionAuthorizer delegate
- `ea76f54` HMAC verify on subscriptions with SubscribeAuthCode enum
- `292c16f` graphql-ws WebSocket transport + NATS bridging for subscriptions
- `da5bc38` server-streaming RPCs become Subscription fields in SDL
- `32c3141` per-pool backpressure with wait timeout + queue metrics
- `32cedee` default Prometheus dispatch timings + /metrics endpoint
- `93a79d9` schema export + cross-cluster promotion tooling
- `833e9f2` ForgetPeer admin RPC + peer CLI subcommand
- `eabf740` optional mTLS on NATS routes + outbound gRPC
- `1d65123` examples/multi: 3-gateway cluster runner
- `bfdc796` KV-driven reconciler for cross-gateway dispatch
- `953c8e2` parallel-write registry KV alongside in-memory map
- `85c5d1d` peers KV + monotonic replica bump
- `7d7ed12` embed NATS server with JetStream
