# go-api-gateway: roadmap & decisions

Source of truth for open work, design decisions, and the rationale
behind both. Read at session start. Update whenever scope shifts.

Tier 1 = correctness / production-blocking. Tier 2 = design-completing
features. Tier 3 = operational polish. Known limitations = called out
intentionally; not currently planned to fix.

---

## Tier 1 — load-bearing

### Auth pass-through for OpenAPI services

The current OpenAPI ingestion has **no authentication story**. Today's
HTTP dispatch sends a request with `Content-Type: application/json` and
nothing else. Real services have bearer tokens, mTLS client certs,
session cookies, signed URLs, etc.

Design forks (pick before implementing):
- **Header pass-through**: forward configured headers from the inbound
  GraphQL request to the outbound HTTP request. Simplest. Operator
  configures `[]string{"Authorization", "X-Api-Key"}` per registered
  spec. Inbound headers come from `WithHTTPRequest` ctx already plumbed.
- **Service-account token**: gateway holds a credential per registered
  service and presents it on every outbound. Doesn't carry user
  identity; service does its own authz from per-request context.
- **OAuth/JWT translation**: gateway exchanges the inbound user token
  for a service-specific token via a configurable issuer. Heavier.
- **mTLS client certs**: gateway dials with a configured client cert
  per service. Reuse `LoadMTLSConfig` plumbing.

Recommended v1: header pass-through + a `WithOpenAPIClient(*http.Client)`
hook so operators can plug in their own transport (mTLS, retry, etc.).
Combine for most cases.

### Automated tests

Zero unit + integration tests across the codebase. All verification has
been by ad-hoc shell e2e during development. A regression in any of
this — pool dispatch, registry KV, subscription HMAC, OpenAPI dispatch
— would be silent.

Prioritize integration tests over unit tests because the most
load-bearing logic is glue (registration → schema rebuild → dispatch).
Targets:
- Multi-replica + version e2e (`examples/multi`-style scripted).
- Cluster mode 2-node with cross-gateway dispatch.
- Subscription publish → WebSocket frame round-trip.
- HMAC verify codes (OK, TOO_OLD, SIGNATURE_MISMATCH, etc.).
- OpenAPI request/response round-trip.
- ForgetPeer happy path + alive-rejection.

Highest-leverage single thing in the entire codebase. Without this,
every change requires manual verification.

### Server-streaming gRPC dispatch (not just SDL)

Server-streaming RPCs are surfaced as Subscription fields and bridged
via NATS pub/sub when services publish to a NATS subject directly. But
**gRPC server-streaming dispatch isn't wired** — we don't open a
gRPC stream against a registered service to receive its messages. If a
service implements an actual server-streaming gRPC method (rather than
publishing to NATS), we can't read from it.

Pick one:
- (a) Document that subscription fields require NATS publishes; never
  dispatch gRPC streams. Simpler, our preferred mental model.
- (b) Also support gRPC streaming dispatch as a transport option per
  service. Heavier; adds a per-stream gRPC client lifecycle.

Lean (a) — keep one transport story (NATS pub/sub for events). Files
declaring server-streaming RPCs but publishing via gRPC are not
supported; they'd need to publish to NATS instead. Document this.

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
| **Server-streaming gRPC filtered with warning, not implemented** | Subscription path is NATS-backed. Lifting actual gRPC streams adds a transport story we'd rather not maintain. |

---

## Recently shipped

(Last n commits worth knowing about for context. Update on commit; trim
older entries when they get stale.)

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
