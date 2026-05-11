# gwag

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

**Register every service once, in the protocol it already speaks.
Clients get typed access in any of three.**

`.proto`, OpenAPI 3.x, downstream GraphQL → one consolidated GraphQL
surface for browser/mobile, **plus** typed proto/OpenAPI/GraphQL
clients (codegen-friendly) for service-to-service. Same schema, same
auth middleware, same backpressure, same metrics across all three.

```
TS / mobile ─┐                              ┌── auth-svc      (.proto)
             ├──▶  /api/graphql      ──┐    ├── billing-svc   (OpenAPI)
service A ──▶│  /api/schema/{*} ◀── codegen ├── inventory-svc (.proto)
service B ──▶│  /api/ingress/...      ─┘    ├── legacy-svc    (GraphQL stitch)
             └──┬─ gwag (1 binary) ─┬─┘     ├── …
                │ tiered versioning │       └── any new service: register at runtime
                │ HA via NATS + JS  │
                │ subscriptions out │
                └───────────────────┘
```

## What you get for it

- **One GraphQL surface for clients** — no juggling N service URLs and M auth schemes; deprecations propagate through to client codegen.
- **Typed clients in all three formats for service-to-service** — proto FDS / OpenAPI / GraphQL SDL all re-emitted, simultaneously, from the same registry.
- **Live-reload schema** — services self-register over the gRPC control plane; new fields land without a gateway redeploy.
- **Tier-based versioning** — `unstable` / `stable` / `vN`; older `vN` auto-`@deprecated`; CI gate on schema diff. ([§Service lifecycle](#service-lifecycle))
- **HA out of the box** — embedded NATS + JetStream KV; any node dispatches to any service registered with any peer. ([§Cluster mode](#cluster-mode))
- **Subscriptions for free** — server-streaming gRPC becomes a flat GraphQL subscription field; one upstream publish fans out to N WebSocket clients via NATS. ([§Subscriptions](#subscriptions-events))
- **Backpressure that respects ownership** — per-pool + per-replica caps; slow service X can't gate calls to service Y. ([§Backpressure & metrics](#backpressure--metrics))
- **Auth / logging as middleware** — one declaration applies across every protocol. `InjectType[T]` / `InjectPath` / `InjectHeader` for the "fill from context, hide from external schema" pattern. ([§Middleware](#middleware))
- **Health / drain / metrics** — `/api/health` (200/503), `gw.Drain(ctx)` for rolling deploys, `/api/metrics` Prometheus. ([§Health & graceful drain](#health--graceful-drain))

## Cost

**Setup:** one Go binary (`gwag`) or a library import — no separate
control plane to deploy. Cluster is opt-in (one extra flag adds a
NATS peer); single-node is the default. The default dispatch path
is reflection-based and accepts any registered service without a
build step; codegen and plugin paths layer on top when you want
extra throughput.

**Per-request overhead** (1 k rps × 15 s, loopback; gateway adds
on top of a direct dial in the matching wire format, lib default
`gwag up`):

| Ingress | Source | Δp50 | Δp95 |
|---|---|---|---|
| gRPC | proto upstream | +283 µs | +336 µs |
| HTTP/JSON | OpenAPI upstream | +208 µs | +245 µs |
| GraphQL | GraphQL upstream | +344 µs | +505 µs |

Per-request middleware adds on top — one `InjectHeader` rule
firing on every dispatch costs ~15–20 µs at p50. Reproduce with
`bin/bench-overhead` (single pass) or `bin/bench-overhead --with-raw`
(side-by-side `gwag up` vs example gateway with one demo middleware
rule); see [§Gateway overhead](#gateway-overhead) for the table and
recipe. Cross-kind ingress (e.g. GraphQL → proto upstream) has no
direct equivalent — that path only exists because the gateway makes
it possible.

## Try it in 60 seconds

```bash
git clone https://github.com/iodesystems/go-api-gateway && cd go-api-gateway
cd examples/multi && ./run.sh        # gateway + greeter + library
```

In another terminal:

```bash
curl -s -X POST http://localhost:8080/api/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}'
# → {"data":{"greeter":{"hello":{"greeting":"Hello, world!"}}}}
```

`Ctrl-C` in the first terminal cleans everything up.

For the full bench / Prometheus / Grafana stack: `bin/bench up`. For
the operator CLI: `gwag --help`.

Library equivalent of the above:

```go
gw := gateway.New()
gw.AddProto("./protos/auth.proto",   gateway.To("authsvc:50051"))
gw.AddProto("./protos/user.proto",   gateway.To("usersvc:50051"))
gw.AddOpenAPI("./billing-openapi.json",
    gateway.As("billing"),
    gateway.To("https://billing.internal"))
http.ListenAndServe(":8080", gw.Handler())
```

Codegen the typed S2S clients for any registered service:

```bash
curl https://gw.internal/api/schema/proto?service=billing > billing.fds
buf generate billing.fds                                  # proto stack

curl https://gw.internal/api/schema/openapi?service=billing > billing.json
openapi-generator-cli generate -i billing.json -g typescript-axios -o ./gen
```

## Why this vs. the alternatives

The short version (deeper comparison in [§Why this vs. service
discovery, service meshes, or other API gateways](#why-this-vs-service-discovery-service-meshes-or-other-api-gateways)):

| | gwag | Apollo Federation | Hasura | Kong / Envoy |
|---|---|---|---|---|
| Single schema across services | ✓ | ✓ (GraphQL only) | ✓ (DB-centric) | ✗ (route-level only) |
| Ingest gRPC `.proto` natively | ✓ | ✗ | ✗ | partial |
| Ingest OpenAPI 3.x natively | ✓ | ✗ | ✗ | partial |
| Stitch downstream GraphQL | ✓ | ✓ (federation) | ✗ | ✗ |
| Re-emit proto / OpenAPI for typed S2S clients | ✓ | ✗ | ✗ | ✗ |
| Subscriptions out of the box | ✓ (NATS) | partial | ✓ (DB triggers) | ✗ |
| Runtime schema reload | ✓ (control plane) | restart | ✓ (DDL) | restart / config push |
| Tiered versioning + deprecation | ✓ (`unstable`/`stable`/`vN`) | manual | manual | manual |
| HA cluster | ✓ (embedded NATS / JetStream) | ✓ | ✓ | ✓ |
| Setup | one binary | gateway + N federated subgraphs | DB + Hasura | mesh + control plane |

vs. **Apollo Federation:** stitching covers most teams; entity-merging
across services that share entity identity is a Federation feature
gwag doesn't replicate (use Federation if you need it). vs. **Hasura:**
gwag wraps services, not databases — owners stay owners. vs. **Kong /
Envoy:** those route bytes; gwag understands the schema and produces
typed clients.

---

## Service lifecycle

Services don't just appear and vanish — they're version-tagged,
deprecated with warnings, and retired only after their callers move
off. The gateway's job is to make every step of that visible.

### Register

Services self-register over the gRPC control plane and heartbeat
to stay alive. One Register call can carry many services on one
address (multiple RPCs in one binary). Heartbeats every TTL/3;
missed heartbeats past TTL evict.

```go
import (
    _ "embed"

    "github.com/iodesystems/go-api-gateway/controlclient"
)

//go:embed greeter.proto
var greeterProto []byte

reg, _ := controlclient.SelfRegister(ctx, controlclient.Options{
    GatewayAddr: "gateway:50090",
    ServiceAddr: "greeter:50051",
    Services: []controlclient.Service{
        {Namespace: "greeter", ProtoSource: greeterProto},
    },
})
defer reg.Close(ctx) // graceful deregister
```

Services ship raw `.proto` bytes; the gateway compiles them via
`protocompile` so leading / trailing comments survive into the
GraphQL SDL. Multi-file `.proto` layouts (one entrypoint with
`import "..."` statements) use `ProtoFS fs.FS` + `ProtoEntry string`
with any `fs.FS` (`embed.FS`, `os.DirFS(...)`, archives), or pass
the imports map explicitly via `ProtoImports map[string][]byte`.

OpenAPI services use the same `Service` struct with `OpenAPISpec`
instead of `ProtoSource`; same shape, both formats ship raw source.
The control-plane API is in
[`gw/proto/controlplane/v1/control.proto`](./gw/proto/controlplane/v1/control.proto).

### Version

A service can register at one of three **tiers** per namespace:

| Tier | What it is | Mutability | Deprecation |
|---|---|---|---|
| `unstable` | Trunk-tip; published on every push from CI | Always overwrites the prior `unstable` | Pinning to it lights up `@deprecated(reason: "unstable — pin to stable or vN for releases")` so codegen flags it in IDE/lint |
| `stable` | Alias to the most-recent numbered cut | Auto-rolls when a new `vN` is registered | Never deprecated |
| `vN` (`v1`, `v2`, …) | Pinned historical cuts | Immutable | Auto-`@deprecated` when a newer `vN` exists |

```graphql
type UserQuery {
  unstable: UserUnstableQuery @deprecated(reason: "unstable — pin to stable or vN for releases")
  stable:   UserV2Query        # alias to the latest cut (currently v2)
  v1:       UserV1Query @deprecated(reason: "use v2")
  v2:       UserV2Query
  # newest fields hoisted to the top
  profile(id: ID!): Profile     # from stable / v2
}
```

The flow that makes this earn its keep:

1. **Trunk publishes to `unstable`.** Every CI green re-registers the service at `unstable`. Schema rebuilds, but the slot is mutable — no `vN` churn, no deprecated noise. Backend teams *building against* a dep can opt into its `unstable` for fast iteration.
2. **Cut a release → freeze the current `unstable` into a new `vN` and `stable` rolls forward.** The team that owns the service decides when to cut. Existing numbered cuts stay callable but `vN-1` and earlier get `@deprecated` automatically.
3. **Caller-side lint forces dependency negotiation.** Set `controlclient.Options.BuildTag` and the client refuses to register `unstable` from a release-tagged binary. So a service that cuts a release *can't* depend on its dependencies' `unstable` — it must pin `stable` or a numbered `vN`. If a dep's `unstable` has diverged from its `stable` with breaking changes, you can't cut your release until that dep cuts theirs (or you adopt their breakage). The schema becomes a forcing function for upstream/downstream cut coordination, not just an artifact of past decisions.

Generated clients propagate `@deprecated` through their normal
codegen channels: `protoc-gen-go` emits `// Deprecated: ...` from
`option deprecated = true;`; graphql-codegen emits JSDoc
`@deprecated` on TS hooks; openapi-generator marks operations
deprecated. So consumers see the warning in their IDE / linter
without anyone telling them.

### Per-tier policy

Each gateway boots with `--allow-tier` controlling which tiers can
register against it:

```bash
# trunk-friendly gateway: anything goes
$ gwag --allow-tier unstable,stable,vN

# release-track gateway: no unstable
$ gwag --allow-tier stable,vN

# locked-down gateway: pinned cuts only (or stable+vN if you want evergreen)
$ gwag --allow-tier vN
```

A service that tries to register a disallowed tier is rejected at
the control plane with a clear error code. Operators who need
physical isolation between deployments (PCI/HIPAA, blast-radius
separation) run multiple gateways with distinct cluster names — that
side of the picture is a deployment concern, not something the
gateway needs to model.

### Deprecate

Two paths:

1. **Auto-deprecation on version fold.** Older `vN` get `@deprecated` automatically when newer cuts register; `unstable` carries a fixed deprecation reason any time it's queried. Free.
2. **Manual deprecate / undeprecate.** Operators flip a `deprecated` bit on any `(namespace, version)` from the admin UI or RPC, with a reason string. Useful for sunsetting a still-current `vN` ahead of cutting `vN+1`, or for un-deprecating in a rollback.

CI gate with `schema diff --strict`:

```bash
$ gwag schema diff \
    --from https://gw.internal/schema \
    --to   ./candidate.graphql --strict
```

`--strict` fails on changes that can break a working client query.
The rules:

- **Breaking** — exit non-zero: required arg / required input field
  removed, output field removed (any nullability), output field type
  changed, default value changed or removed, required arg / required
  input field added without a default, type / enum value removed.
- **Info** — printed but not a failure: optional arg / optional input
  field removed (callers who didn't pass it are unaffected; callers
  who did get a recoverable validation error), default value added,
  optional arg / field added, deprecation flipped on, new types /
  enums / fields.

The conservative "any removal is breaking" policy is one Tier-2
upgrade away — the planned **caller-side usage tracking** workstream
turns "is anyone passing this optional arg?" into a queryable
question. Until then, the relaxation matches the asymmetric reality
that *adding* fields is always safe but *removing* fields is mostly
safe for the optional ones.

### Retire

Stop heartbeating; the gateway evicts after TTL. The schema
rebuilds; the field disappears.

### What this *doesn't* (yet) tell you

You can't currently ask the gateway *who's still calling a deprecated
operation*. The metrics carry `namespace, version, method, code` —
not caller identity. Operators today have to read upstream service
logs to find the laggards.

A planned **call-site usage tracking** workstream adds an inbound
caller dimension (request-tagged `User-Agent` or service-account
identity, propagated to the metric label set), with a UI panel
listing recent callers per deprecated operation. Until it ships,
deprecation is "announce, wait, retire" — same as without the
gateway, except the schema-side warnings are at least automatic.

## Examples

- [`examples/multi`](./examples/multi) — three separate processes
  (gateway + greeter + library) wired via the control plane.
  Schema rebuilds in place as services join and leave.
- [`examples/auth`](./examples/auth) — `InjectType[*authv1.Context]`
  hiding auth fields globally and filling them from a registered
  internal service.

## Cluster mode

A gateway can embed a NATS server with JetStream and form a cluster
with peer gateways. The service registry moves into a JetStream KV
bucket so any gateway can dispatch to any service registered with any
other gateway:

```go
cluster, _ := gateway.StartCluster(gateway.ClusterOptions{
    NodeName:      "n1",
    ClientListen:  ":14222",
    ClusterListen: ":14248",
    Peers:         []string{"127.0.0.1:14249"},
    DataDir:       "/var/lib/go-api-gateway/n1",
})
defer cluster.Close()

gw := gateway.New(gateway.WithCluster(cluster))
```

- **Bootstrap.** First node in a fresh cluster runs in standalone
  JetStream (R=1) when `Peers` is empty. To scale beyond one node,
  every node — including the seed — must start with at least one
  `Peers` entry.
- **Replicas auto-bump.** As peers join, the registry KV's replica
  count rises monotonically toward `min(peers, 3)`. Killing a peer
  does *not* shrink R automatically; that path is operator-driven
  via `peer forget` (see CLI).
- **Cross-gateway dispatch.** A reconciler on every gateway watches
  the registry KV and dials services it sees, regardless of which
  gateway received the registration.
- **Optional mTLS.** `gateway.LoadMTLSConfig` + `ClusterOptions.TLS`
  + `gateway.WithTLS` requires mutual TLS on both NATS cluster
  routes and outbound gRPC dials.
- **Forget disconnected peers.** `ForgetPeer` (RPC + CLI) drops a
  peer that has TTL-expired and shrinks the registry replica count
  if appropriate. Refuses to forget a still-alive peer.

A runnable 3-gateway demo is in
[`examples/multi/run-cluster.sh`](./examples/multi/run-cluster.sh).

## CLI

```
$ go install github.com/iodesystems/go-api-gateway/cmd/gwag@latest
$ gwag \
    --proto ./greeter.proto=greeter-svc:50051 \
    --proto ./library.proto=commerce@library-svc:50052 \
    --addr :8080
```

`--proto PATH=[NAMESPACE@]ADDR`, repeatable. Default namespace is the
filename stem; default addr is `:8080`. Insecure dial — wrap in real
TLS via the library API for production.

For a running cluster, the same binary exposes operator and codegen
subcommands:

```
$ gwag peer list   --gateway gw1.internal:50090
$ gwag peer forget --gateway gw1.internal:50090 NODE_ID

$ gwag services list --gateway gw1.internal:50090
$ gwag schema fetch  --endpoint https://gw.internal/schema > schema.graphql
$ gwag schema diff   --from https://gw1.internal/schema \
                                --to   https://gw2.internal/schema --strict
$ gwag sign          --gateway gw1.internal:50090 \
                                --channel events.UserEvents.UserCreated.42 --ttl 60
```

- `peer forget` only succeeds against a node whose KV entry has
  expired (safe shrink); `peer list` shows live entries.
- `services list` returns `(namespace, version, hash, replica_count)`
  for every registered service across all binding kinds (proto,
  OpenAPI, downstream GraphQL) — identical hashes across two
  clusters mean identical schema bytes for the same kind.
- `schema fetch` GETs the gateway's `/schema` endpoint as SDL (or
  introspection JSON via `--json`) for client codegen.
- `schema diff --strict` fails CI when a candidate schema would break
  existing consumers.

## Subscriptions (events)

**The mental model: the stringified call is multiplexed with HMAC for multi-listener / single-producer.**

Three claims hold simultaneously:

1. **Channel name *is* the canonical call.** The NATS subject format is `events.<namespace>.<MethodName>.<arg0>.<arg1>...` — built deterministically from `(namespace, method, request fields in declaration order)`. Distinct subscribers for distinct args land on distinct subjects automatically; missing or empty args render as `*` (NATS wildcard). No ad-hoc topic routing, no per-event metadata fan-out logic.
2. **HMAC binds (channel, timestamp).** A token signed for `events.UserEvents.UserCreated.42` cannot subscribe to `.7`. The signer is the policy authority — your auth/business service, the one that already has the request context — not the gateway. The gateway just verifies on subscribe.
3. **One upstream publish, many WS clients.** The gateway shares one NATS subscription across every WebSocket watching the same subject. A producer that publishes once delivers to N listeners with one upstream cost; the per-listener cost is just the WS write. This is the single-producer / multi-listener guarantee.

Transport is the standard `graphql-transport-ws` WebSocket subprotocol (Apollo, urql, graphql-codegen all speak it). Backing storage is **NATS pub/sub**, not gRPC streaming — services don't implement server-streaming RPC, they just `nats.Publish()` to the resolved subject. The server-streaming method on the proto is the *schema declaration*, not the runtime path.

### Schema mapping

Each `rpc Foo(Filter) returns (stream Event)` becomes a flat field `<namespace>_<lowerCamel(method)>` on the `Subscription` root, with the request fields as arguments plus injected `hmac: String!` and `timestamp: Int!`:

```proto
service UserEvents {
  rpc UserCreated(UserCreatedFilter) returns (stream UserCreatedEvent);
}
message UserCreatedFilter { string user_id = 1; }
message UserCreatedEvent  { string user_id = 1; string email = 2; }
```

becomes

```graphql
type Subscription {
  userEvents_userCreated(userId: ID!, hmac: String!, timestamp: Int!): UserCreatedEvent
}
```

### Channel-name contract

The NATS subject is computed from the resolved arg values in field-declaration order:

| Filter args                    | Resolved subject                          |
|--------------------------------|-------------------------------------------|
| `{userId: "42"}`               | `events.UserEvents.UserCreated.42`        |
| `{userId: "*"}` or `{userId: ""}` | `events.UserEvents.UserCreated.*`      |
| `{userId: "42", region: "us"}` | `events.UserEvents.UserCreated.42.us`     |

Wildcards follow standard NATS semantics: `.42` matches only that exact value; `.*` matches any single-token value at that position. Producers publish to the *concrete* subject (`...42`); subscribers can use `*` to receive across all values for that arg. The implementation is `subjectFor` in `gw/subscriptions.go`.

### Worked example

The `examples/multi` stack ships a working publisher — the `greeter` service has a server-streaming `Greetings` RPC and a timer-driven `nats.Publish` loop:

```go
// examples/multi/cmd/greeter/main.go
payload, _ := proto.Marshal(&greeterv1.Greeting{
    Greeting: "Hello, " + name + "!",
    ForName:  name,
})
nc.Publish(fmt.Sprintf("events.greeter.Greetings.%s", name), payload)
```

Run the stack and subscribe over GraphQL:

```bash
$ cd examples/multi && ./run.sh    # boots gateway + greeter + library
                                   # (run.sh passes --insecure-subscribe)
```

```typescript
// Any graphql-ws client. Wildcard args match across all producer values.
const { data } = useSubscription(gql`
  subscription($n: String!, $h: String!, $t: Int!) {
    greeter_greetings(name: $n, hmac: $h, timestamp: $t) {
      greeting
      forName
    }
  }
`, { variables: { n: "*", hmac: "", timestamp: 0 } });
```

For production, replace `--insecure-subscribe` with `--subscribe-secret <hex>` and have the client fetch `hmac` / `timestamp` from `SignSubscriptionToken` — see `## HMAC channel auth` below.

### HMAC channel auth

The gateway owns the HMAC secret and is responsible for two things:
**verifying** tokens on subscribe and **minting** them on demand. It
does *not* try to be the policy authority. Business authz (which user
can subscribe to which channel) lives in whatever service has the
request context — the gateway just signs.

Verification modes (operator picks at gateway boot):

- `--insecure-subscribe` — bypass verification (dev only).
- `--subscribe-secret <hex>` — gateway holds a shared secret;
  verifies HMAC-SHA256(secret, "<channel>\n<timestamp>") base64 on
  every subscribe. Default `--subscribe-skew 5m`.

**Signing is an exposed endpoint, not a delegated decision.** The
gateway publishes `SignSubscriptionToken` (gRPC + the
`gwag sign` CLI). A downstream service that already
authenticates the end user — your auth service, the service that
owns the events stream, your BFF, whatever — does its own
permission check, then calls Sign to mint a token for the client:

```
client subscribes via service-X →
service-X authenticates the user (its own session/JWT/whatever) →
service-X checks "may this user read events.UserEvents.UserCreated.42?" →
service-X calls gateway.SignSubscriptionToken(channel, ttl) →
service-X returns {hmac, ts} to the client →
client opens the WebSocket with hmac/ts as subscription args →
gateway verifies HMAC and accepts.
```

The caller in this flow already has the full request context (the
user's session, the resource being subscribed to, your authz
policy), so authorization stays where the context lives. The
gateway doesn't need to learn your authz model.

**Protecting the signer.** Remote (gRPC peer) calls to
`SignSubscriptionToken` require an `authorization: Bearer <hex>`
metadata header. The accepted bearers are:

- `--signer-secret <hex>` (or `WithSignerSecret(...)`) — sign-specific
  bearer; rotate independently of the admin token. Lower blast radius
  than handing out the admin/boot token.
- The boot/admin token — unconditional fallback.

In-process callers (the huma `/admin/sign` handler, library embedders
calling `cp.SignSubscriptionToken` directly) bypass the gate — the
trust boundary is the embedder, not the wire. Outcomes land in
`go_api_gateway_sign_auth_total{code}` with codes `in_process`,
`ok_signer`, `ok_bearer`, `denied_bearer`, `missing_bearer`,
`no_token_configured`.

Stack mTLS on the gRPC listener and/or network policy on top for
defense in depth.

Tokens are minted via gRPC or the CLI:

```
$ gwag sign --gateway gw1.internal:50090 \
                      --channel events.UserEvents.UserCreated.42 --ttl 60
hmac=md6l2SVJ...
ts=1778092482
```

Or signed locally if you hold the secret yourself (operator tooling,
break-glass):

```
$ gwag sign --secret <hex> --channel events.... --ttl 60
```

Verification outcomes are surfaced as `SubscribeAuthCode` (`OK`,
`TOO_OLD`, `SIGNATURE_MISMATCH`, `MALFORMED`, `DENIED`, `UNAVAILABLE`,
`NOT_CONFIGURED`, `UNKNOWN_KID`). The code lands in
`go_api_gateway_subscribe_auth_total{code=...}` and in the WebSocket
error frame's `extensions.subscribeAuthCode`.

Client-streaming and bidi RPCs aren't promoted — they're filtered
with a registration-time warning so operators can see what's hidden.

## Admin auth (boot token)

The gateway protects its own admin surface (`/admin/*` writes,
`admin_*` GraphQL mutations) with a bearer token. On boot, the
gateway either reads an existing token from
`<adminDataDir>/admin-token` or generates a fresh 32-byte one and
persists it. The token is logged to stderr at startup:

```
admin token = ab9089b1...  (persisted to /var/lib/gateway/admin-token)
```

Wire it as standard `Authorization: Bearer <hex>`. Reads (GETs on
`/api/admin/*`, `admin_listPeers` / `admin_listServices` queries)
are public so the UI works unauthenticated; writes require the token.

```go
gw := gateway.New(
    gateway.WithAdminDataDir("/var/lib/gateway"),
)
adminMux, adminSpec, _ := gw.AdminHumaRouter()
mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))

// admin_* GraphQL mutations dispatch through /api/admin/*; the
// inbound Authorization header is forwarded automatically, so one
// bearer covers both surfaces.
gw.AddOpenAPIBytes(adminSpec,
    gateway.As("admin"),
    gateway.To("http://localhost:8080/api"))
```

### Pluggable AdminAuthorizer delegate

For richer authz than a single static token, register an
`AdminAuthorizer` service at `_admin_auth/v1` (proto in
[`adminauth/v1`](./gw/proto/adminauth/v1)). The middleware consults it on
every protected request:

| Delegate response       | Middleware action                          |
|-------------------------|--------------------------------------------|
| `OK`                    | Accept                                     |
| `DENIED`                | 401, no fall-through                       |
| `UNAVAILABLE`           | Fall through to boot token                 |
| `NOT_CONFIGURED`        | Fall through to boot token                 |
| Transport error / panic | Fall through to boot token                 |

The boot token is an unconditional fallback. A delegate that
crashes, mis-deploys, or DOS's cannot lock operators out — only an
explicit `DENIED` short-circuits.

Admin auth is unrelated to outbound auth to OpenAPI backends. For
that, see the next section.

## Outbound HTTP transport for OpenAPI dispatch

By default, `Authorization` is forwarded from the inbound GraphQL
request to the outbound OpenAPI dispatch. Override the allowlist
per source with `gateway.ForwardHeaders(...)`.

For anything beyond plain bearer pass-through — mTLS, a custom
`http.RoundTripper` that injects a service-account token, signed-URL
rewriting, retry/timeout policy — supply a `*http.Client`:

```go
// Gateway-wide default — used by every OpenAPI source unless
// overridden per-source.
gw := gateway.New(gateway.WithOpenAPIClient(&http.Client{
    Transport: customRoundTripper,
    Timeout:   10 * time.Second,
}))

// Per-source override — beats the gateway-wide default.
gw.AddOpenAPI("https://billing.internal/openapi.json",
    gateway.As("billing"),
    gateway.To("https://billing.internal"),
    gateway.OpenAPIClient(billingClient),  // custom mTLS to this one backend
)
```

When neither is set, dispatches use `http.DefaultClient`.

## Health & graceful drain

`gw.HealthHandler()` mounts a `/health` endpoint that returns:

```json
{"status":"serving","active_streams":0,"node_id":"NA..."}
```

with HTTP 200 normally, or HTTP 503 once `gw.Drain(ctx)` has been called.
Wire `/health` to your load balancer's health check.

`gw.Drain(ctx)` performs the rolling-deploy preamble:

1. `/health` flips to 503 — LB pulls this node out within its check
   interval (typically 5-30 s).
2. New WebSocket upgrades return 503.
3. Existing WebSocket connections have their context cancelled —
   graphql-go emits `complete` per active subscription, then close.
4. Drain waits for `streams_inflight_total` to reach 0 or `ctx`
   to expire.
5. After Drain returns, run your gRPC/`Cluster.Close()` teardown.

HTTP unary queries are *not* actively drained — they're sub-second and
finish on their own once the LB stops sending new traffic.

The example wires SIGTERM to a 30-second drain, so a `kubectl delete`
or `docker stop` triggers the right behaviour automatically.

## Backpressure & metrics

Each `(namespace, version)` pool has its own dispatch concurrency caps
and per-dispatch wait budget. Slow services back up *their own* pool
without blocking dispatches to other pools — a sluggish `auth`
service does not gate `library` requests. Subscriptions have a
*separate* slot from unary so long-lived streams don't crowd queries.

Defaults (override via `gateway.WithBackpressure(...)`):

```go
DefaultBackpressure = BackpressureOptions{
    MaxInflight:     256,             // per-pool concurrent unary dispatches
    MaxStreams:      10_000,          // per-pool active subscription streams
    MaxStreamsTotal: 100_000,         // gateway-wide stream ceiling (file descriptors, RAM)
    MaxWaitTime:     10 * time.Second, // wait budget; exceeded → fast-reject
}
```

The hybrid stream caps (per-pool + gateway-wide) keep streaming
isolation honest while still bounding the actual scarce resource.
A dispatch that cannot acquire its pool's slot within `MaxWaitTime`
fails with `Reject(ResourceExhausted, "could not acquire slot in N")`
— this is the "you can't even get a slot" backoff. There's no flat
gateway-wide unary cap by design (it would couple unrelated requests);
visibility comes from the per-pool metrics below.

Every dispatch is timed by default. `gw.MetricsHandler()` exposes:

```
go_api_gateway_dispatch_duration_seconds{namespace,version,method,code}
go_api_gateway_pool_queue_dwell_seconds{namespace,version,method,kind}
go_api_gateway_pool_backoff_total{namespace,version,method,kind,reason}
go_api_gateway_pool_queue_depth{namespace,version,kind}            (gauge)
go_api_gateway_pool_streams_inflight{namespace,version}            (gauge)
go_api_gateway_subscribe_auth_total{namespace,version,method,code}
```

- `code` (dispatch) is `ok` on success, the gRPC status string on
  failure, or a `Reject` code when middleware short-circuits.
- `kind` is `unary` or `stream` — splits queries from subscriptions
  on the same backpressure metrics.

Mount alongside the GraphQL endpoint:

```go
mux := http.NewServeMux()
mux.Handle("/graphql", gw.Handler())
mux.Handle("/schema", gw.SchemaHandler())
mux.Handle("/metrics", gw.MetricsHandler())
```

Override or disable:

```go
gw := gateway.New(gateway.WithoutMetrics())            // disable
gw := gateway.New(gateway.WithMetrics(myCustomSink))   // plug in your own
```

## Gateway overhead

> First question every adopter asks: *what does this cost vs. going
> direct?*

Two numbers matter for capacity planning:

- **Raw** is the lib default — `gwag up`, no application middleware.
  This is what every dispatch costs before you add anything.
- **Extras** is the same gateway with one `InjectHeader("X-Source-IP")`
  rule firing on every outbound dispatch — the example gateway's
  worked-example dressing. It's the cost of one trivial middleware
  rule; richer rules add more.

Per-request overhead at 1 k rps × 15 s, loopback:

| Ingress | Source | Direct p50 / p95 | Raw p50 / p95 | Raw Δ p50 / p95 | Extras Δ p50 / p95 | Dressing (extras − raw) p50 / p95 |
|---|---|---|---|---|---|---|
| gRPC    | proto upstream  (`hello-proto`)    | 235 / 283 µs | 518 / 618 µs | **+283 / +336 µs** | +297 / +362 µs | +15 / +31 µs |
| HTTP/JSON | OpenAPI upstream (`hello-openapi`) | 161 / 199 µs | 369 / 444 µs | **+208 / +245 µs** | +224 / +267 µs | +19 / +27 µs |
| GraphQL | GraphQL upstream (`hello-graphql`) | 353 / 463 µs | 697 / 967 µs | **+344 / +505 µs** | +350 / +508 µs | +8 / +5 µs |
| Cross-kind (e.g. GraphQL → proto upstream) | gateway-only path | — | — | n/a | n/a | n/a |

Read this as: the gateway's IR translation layer adds ~200–350 µs at
p50 on this host before any middleware runs. Each active middleware
rule on the hot path adds ~15–20 µs at p50 — `InjectHeader` here, but
also auth, logging, rate-limit, anything else you wire up. The
"direct" pass dials the upstream in its native wire format; the
"gateway" passes route through the matching gateway ingress.
Cross-kind ingress (e.g. GraphQL ingress hitting an OpenAPI source)
has no direct equivalent and renders N/A — that cell exists only
because the gateway makes it possible.

**Reproduce locally:**

```bash
bin/bench-overhead --with-raw   # raw + extras passes; labeled compare
bin/bench-overhead              # whatever stack is up; single shape
```

`bin/bench up` boots a single-gateway stack (n1 + greeter +
Prometheus + Grafana); `bin/bench up --raw` boots the same with
`gwag up` instead of the example gateway. Each `bin/bench traffic`
run prints a `gateway` vs `direct` table with mean / p50 / p95 / p99 / Δ;
saturation drops, codes, and example bodies are in the per-pass
blocks above the compare. Raise `--rps` and `--duration` past the
1 k × 15 s default for a steadier signal.

## Promotion path

Tier flow (`unstable` → `stable` → `vN`) and the BuildTag forcing
function are covered in [§Service lifecycle](#service-lifecycle). Two
extra gates wire the same axis into CI:

1. **`services list`** exposes per-pool hashes — CI diffs hash sets
   across clusters to confirm the bytes match for every
   `(namespace, tier|version)` you intend to promote.
2. **`/schema` + `schema diff --strict`** is the client-perspective
   gate — additions are fine, removals or required-arg changes fail
   the build (rules in [§Deprecate](#deprecate)).

Per-cluster drift is prevented by the canonical hash gate in the
pool: a replica with a mismatched proto can't join an existing
`(ns, version)` pool.

## Middleware

One `Transform` declaration carries up to four reshaping concerns
that fire at different layers, in lockstep:

| Field | Layer | Effect |
|---|---|---|
| `Schema` (`[]SchemaRewrite`) | once at boot | Rewrites the external schema (hide types, hide fields, flip nullability) |
| `Runtime` (`Middleware`) | per request | Wraps the dispatch handler — read or mutate request and response |
| `Headers` (`[]HeaderInjector`) | per dispatch | Stamps outbound HTTP headers / gRPC metadata |
| `Inventory` (`[]InjectorRecord`) | registration time | Surfaces what an operator declared at `/admin/injectors` |

`Runtime` is the same `next()` chain you've seen in every Go
middleware library:

```go
mw := func(next gateway.Handler) gateway.Handler {
    return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
        // pre — filter or transform request
        resp, err := next(ctx, req)
        if err != nil { return nil, err }
        // post — transform response
        return resp, nil
    }
}

gw.Use(gateway.Transform{Runtime: mw})
```

- **Observer**: call `next`, return its result, do something on the side (log, metric, trace).
- **Filter**: return an error without calling `next` (auth, rate limit, allow-list). Use `gateway.Reject(code, msg)` so the gateway can map to the right GraphQL error.
- **Transform**: wrap `next` and mutate input or output.

Schema and Runtime often need to stay in sync — hiding `userID` from
the external schema is meaningless without a runtime hook to fill it
from context. The library ships three constructors that build a
matched `Transform` so the schema/runtime invariant is enforced by
construction:

| Constructor | What you address | Schema half | Runtime half |
|---|---|---|---|
| `InjectType[T](resolve, opts...)` | every field/arg of Go type `T` | hides (default) or `Nullable(true)` | calls `resolve(ctx, current *T)` to fill the field |
| `InjectPath("ns.method.arg", resolve, opts...)` | one specific call site (only way to address a primitive arg) | hides or rewrites at that path | resolves at request time for that path |
| `InjectHeader(name, resolve, opts...)` | one outbound HTTP header / gRPC metadata key | n/a | adds the header on every dispatch |

`Hide(true)` (the default for `InjectType` / `InjectPath`) strips the
arg from the external schema and the resolver always sees
`current=nil`. `Hide(false)` keeps the arg visible and gives the
resolver the caller-provided value to inspect-and-decide.

Single-purpose middleware (logging, rate limit) builds a `Transform`
that fills only `Runtime`.

> **Subscriptions don't run through `Runtime`.** Server-streaming
> RPCs are exposed as GraphQL subscription fields backed by NATS
> pub/sub (see §Subscriptions); the per-request middleware chain is
> for unary calls.

### The auth case end-to-end

The shape that drove the API: globally hide auth fields, fill them from
a registered auth service, and hide that service from the external
schema too. See [`examples/auth`](./examples/auth):

```go
gw := gateway.New()

// Internal: not exposed in the GraphQL surface, but callable by hooks.
gw.AddProto("./protos/auth.proto",
    gateway.To(authConn),
    gateway.AsInternal(),
)

// Public services.
gw.AddProto("./protos/user.proto", gateway.To(userConn))

// One declaration: schema half hides every input field of type
// *authv1.Context; runtime half fills them. With Hide(true) the arg
// never reaches the wire, so `current` is always nil here.
gw.Use(gateway.InjectType[*authv1.Context](func(ctx context.Context, _ **authv1.Context) (*authv1.Context, error) {
    token := bearerFromContext(ctx)
    if token == "" {
        return nil, gateway.Reject(gateway.CodeUnauthenticated, "missing bearer token")
    }
    resp, err := authClient.Resolve(ctx, &authv1.ResolveRequest{Token: token})
    if err != nil {
        return nil, err
    }
    return resp.GetContext(), nil
}))

http.ListenAndServe(":8080", gw.Handler())
```

External GraphQL surface contains no `auth` namespace and no
`Context` type. Internally, every RPC whose input embeds
`auth.v1.Context` gets it filled from one cached call to the auth
service per request.

## Design notes

- **Reflection-based default path.** `.proto` and OpenAPI specs
  parse at boot via `bufbuild/protocompile` / `kin-openapi`; gRPC
  calls go out via `dynamicpb`; HTTP calls assemble from the spec.
  Any registered service works without a build step. Codegen and
  plugin paths layer on as opt-in upgrades for extra throughput.
- **Path-based identity.** Namespaces default to filename stems;
  collisions across registered files are an error, not silent
  overwrite.
- **Two registries.** Public schema view vs internal callable
  registry. Internal-only services live in the callable registry but
  not the external schema; hooks (auth resolver, etc.) call them.
- **Caching is library-side.** A naive auth resolver gets called once
  per field per request; the library memoises per-(request, type) so
  users don't reinvent it.
- **`Reject(code, msg)` for short-circuits.** Plain errors are mapped
  to opaque internal errors; typed rejections become the right GraphQL
  error code (and gRPC status when bridged outbound).
- **Auto-internal `_*` namespaces.** Any namespace starting with `_`
  is hidden from the public schema. `_events_auth`, `_admin_auth`,
  `_admin_events`, etc. — operators don't have to remember a flag.
- **Dogfood the OpenAPI path.** The gateway's own admin operations
  are defined via huma → OpenAPI → self-ingested → surfaced as
  `admin_*` GraphQL fields. Same path any external service takes.

## Why this vs. service discovery, service meshes, or other API gateways

Three reasonable questions any new reader will have. The short
answer in each case is **different scope** — these systems sit at
different layers and you'll typically run more than one.

### "Isn't k8s service discovery / Consul / etcd enough?"

Service discovery routes *bytes* — `auth-svc.cluster.local:50051`
resolves to a pod IP. It doesn't know what the service offers,
whether it's deprecated, what its schema is, or whether your client
is using an out-of-date version of it. You still hand-write or
hand-wire the client.

This gateway sits *above* discovery. Discovery answers *"where is
the auth service?"*; the gateway answers *"what does the auth
service expose, in what shape, and how do I codegen a typed client
for my language?"* Both layers coexist — discovery routes within
the cluster; the gateway is the schema-aware aggregator.

### "Isn't a service mesh enough?"

Meshes (Istio, Linkerd, Consul Connect) own L7 wire concerns: mTLS,
retries, traffic shifting by percentage, circuit-breaking. Their
observability is wire-shaped — bytes/sec, request counts by HTTP
status code.

This gateway owns *schema* concerns: typed dispatch, versioning,
deprecation propagation through to client codegen, schema diff in
CI, hide-and-inject middleware, per-method backpressure. Its
observability is schema-shaped —
`dispatch_duration_seconds{namespace,version,method}`, p95 latency
per replica per version, recent callers per deprecated method.

Run both. Mesh for the wire, gateway for the contract.

### "Isn't Kong / APIGee / AWS API Gateway / Apollo Federation enough?"

Most edge-style API gateways are HTTP-routing-shaped: path rewrite,
auth-header injection, rate-limit, send to a backend. Single
inbound protocol, single outbound protocol, no schema unification
across formats.

Apollo Federation is the closest peer for the unified-GraphQL
story, but federation only ingests GraphQL — you can't drop a
`.proto` or an OpenAPI spec on it.

The axes that differ:

| Concern | Kong / APIGee / etc. | Apollo Federation | go-api-gateway |
|---|---|---|---|
| **Ingest formats** | HTTP/REST (some gRPC plugin) | GraphQL only | `.proto`, OpenAPI 3.x, GraphQL stitching |
| **Codegen targets** | None — operators bring their own | GraphQL SDL only | Proto FDS + OpenAPI + GraphQL SDL, simultaneously |
| **Versioning** | Per-route metadata; manual | Per-subgraph; manual | Tier model (`unstable` / `stable` / `vN`); auto-deprecation; dependency-negotiation forcing function |
| **Metrics shape** | Wire-shaped (status, RPS) | GraphQL-shaped (operation, type) | Schema-aware: per-pool, per-replica, per-method, per-tier |
| **Custom transforms** | Plugins, often language-locked | Schema directives + custom resolvers | Go middleware `Pair` — one declaration applies across all protocols |
| **HA story** | External LB | Stateless; depends on subgraphs | Embedded NATS + JetStream KV; any node dispatches to any service |

### Where this is specifically strong

[§What you get](#what-you-get-for-it) is the headline list. Two
points worth re-stating in the comparison frame above:

- **Schema-aware metrics.** Per-pool, per-replica, per-method
  dispatch latency and queue dwell with deprecation labels — wire-level
  metrics can answer *"is the auth service slow?"*, schema-level
  metrics can answer *"who's still calling deprecated `users.v1.list`?"*
- **One transform, every protocol.** A `Transform` (e.g. via
  `InjectType[T]`) hides a field from the public schema *and* fills it
  from request context in one declaration; the same code applies
  whether the underlying service is proto, OpenAPI, or downstream
  GraphQL.

### What it doesn't replace

- **Service discovery.** Use k8s Services / Consul / DNS / whatever
  you already have. Bridge into the gateway via the gRPC control
  plane.
- **Service mesh, mTLS, traffic shifting at the wire.** Sibling
  layer; run a mesh.
- **Observability backends.** Metrics export Prometheus; pick your
  scraper, alerting, and dashboards.
- **CI/CD and deploy automation.** The control plane lets you wire
  register/deregister into your deploy pipeline; the pipeline
  itself is yours.

## What's not in here

- Rolling deploy / hot reload of the gateway itself. Run blue/green
  like any other binary, or scale by adding peers.
- Caller-side usage tracking on deprecated operations. (Roadmap —
  see [§Service lifecycle](#service-lifecycle).)
- Apollo Federation. Stitching covers the common case; federation's
  entity-merging is overkill for most teams.
- AsyncAPI export. GraphQL SDL with Subscription types covers TS
  codegen; AsyncAPI's TS tooling is patchier with little payoff.

## License

MIT. See [LICENSE](./LICENSE).
