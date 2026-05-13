# Pub/Sub

The gateway provides pub/sub as a *primitive*, not a transform applied
to every server-streaming RPC declaration. Two GraphQL fields, both
gateway-defined; service-declared `stream Resp` methods stay as honest
per-subscriber gRPC streams.

```graphql
type Mutation {
  ps { pub(channel: String!, payload: String!, hmac: String, ts: Int): Boolean! }
}
type Subscription {
  ps { sub(channel: String!, hmac: String, ts: Int): ps_Event! }
}
type ps_Event {
  channel: String!
  payload: String!
  payload_type: String!   # fully-qualified proto message name; blank if channel is unbound
  ts: Int!
}
```

The backing proto lives at `gw/proto/ps/v1/ps.proto`. The `ps` slot is
installed in-process when `WithCluster(...)` is set — it dispatches
through the internal-proto kind, not a network gRPC call, but the IR /
SDL / admin-listing / control-plane surface is the same as every other
proto service.

Payload is `string` — JSON, base64, agreed-encoding-of-the-day; the
broker doesn't care. Subscribers decode per the `payload_type` label,
cross-referenced against `/api/schema/proto?service=...`.

## Channel naming

NATS subjects: `events.<namespace>.<thing>.<subject>` — free-form,
dot-segmented. Wildcards (`*` for one segment, `>` for the rest) work
on `ps.sub`; publishes must use a literal subject.

```graphql
subscription {
  ps {
    sub(channel: "events.orders.>", hmac: "...", ts: 1700000000) {
      channel payload payload_type ts
    }
  }
}
```

The producer signs over the *literal pattern string the client
requests* — a token for `events.orders.42.update` does not satisfy a
sub on `events.orders.>`. Operators issuing tokens control the pattern
surface.

## Auth tiers

Operator configures per channel pattern via `WithChannelAuth(pattern, tier)`.
Patterns share the NATS wildcard grammar.

| Tier | What |
|---|---|
| `ChannelAuthOpen` | No auth; `hmac` / `ts` ignored. Dev / public channels. |
| `ChannelAuthHMAC` | HMAC token over `(channel, ts)`, signed by `WithSubscriptionAuth`'s secret. Hot-path crypto-fast. |
| `ChannelAuthDelegate` | HMAC first, then a delegate authorizer at `_pubsub_auth/v1` (same fall-through posture as `_admin_auth/v1`). Use when authz needs request context the gateway doesn't carry. |

```go
gw := gateway.New(
    gateway.WithChannelAuth("events.public.*",  gateway.ChannelAuthOpen),
    gateway.WithChannelAuth("events.private.*", gateway.ChannelAuthHMAC),
    gateway.WithChannelAuth("events.admin.>",   gateway.ChannelAuthDelegate),
)
```

Pub channels are literal: rules try in declaration order, first hit
wins. Default tier when no rule matches: `ChannelAuthHMAC`. For
wildcard `Sub` patterns the gateway computes the **strictest tier**
across every rule whose pattern intersects the requested pattern,
folding in the default when no single rule fully covers — wildcards
can't leak events from a stricter pattern through a permissive one.

The `ChannelAuthDelegate` proto (`gw/proto/pubsubauth/v1/pubsubauth.proto`)
mirrors `AdminAuthorizer`'s shape: the authorizer is registered under
namespace `_pubsub_auth`, version `v1`, and answers `Authorize` per
subscribe. Codes: `OK` accepts, `DENIED` rejects without falling
through; `UNAVAILABLE` / `NOT_CONFIGURED` / `UNSPECIFIED` / transport
error all fall through to HMAC-only (which already verified).

## Channel → payload-type registry

Typemap from channel pattern to proto message type. Two registration
paths, same registry:

**Proto-declarative (canonical).** Custom message option in the
service's `.proto`:

```proto
import "gw/proto/ps/v1/options.proto";

message OrderUpdateEvent {
  option (gwag.ps.v1.binding) = { pattern: "events.orders.*.update" };
  string order_id = 1;
  string action = 2;
}
```

Rides `proto_source` for cluster propagation — bindings travel with
the existing service registration, no separate KV bucket.

**Runtime API (escape hatch).** For non-proto adopters or gateway-
shipped defaults, register pattern → FQN by string:

```go
gw := gateway.New(
    gateway.WithCluster(...),
    gateway.WithChannelBinding("events.orders.*.update", "myco.orders.v1.OrderUpdateEvent"),
)
```

The runtime path takes the FQN directly so the caller doesn't need
the generated Go type available at construction. The cross-slot
uniqueness rule below applies to both paths.

On `Sub` delivery the gateway stamps `Event.payload_type` with the
matched binding's FQN. Subscribers fetch the descriptor at any time
via `/api/schema/proto?service=...` for validation or codegen. List
the active registry through the admin endpoint
`GET /api/admin/bindings` (rows: `channel_pattern → payload_type →
namespace/version → tier`).

**Tier policy inherits.** `unstable` slots' bindings overwrite on
rebake; `vN` slots' bindings lock by `(pattern, payload_fqn)` —
versioned messaging gets the existing version-tier story.

**Pattern uniqueness across slots.** Two different `(namespace,
version)` slots can't both claim the same pattern. Conflict is
hard-rejected at registration. Same-slot rebake on `unstable` swaps
the binding; on `vN` it's `AlreadyExists`.

## Contract enforcement (opt-in)

Defaults are documentation-only — broker stays permissive. Two
independent strictness axes:

- `WithChannelBindingEnforce()` — **shape strictness.** At Pub entry,
  parse payload as the bound proto message; reject mismatch with
  `InvalidArgument`. Mirrors `WithCallerIDEnforce` / `WithQuotaEnforce`.
  No-op for runtime bindings whose FQN isn't resolvable against a
  registered descriptor.
- `WithStrictPayloadTypes()` — **coverage strictness.** Reject Pub
  publishes whose channel matches no `WithChannelBinding` pattern
  (where `payload_type` would otherwise be blank). Keeps the `open`
  tier usable for ad-hoc / dev channels by default.

Both can be enabled together. Default: utility first, strictness as
opt-in upgrade — neither flag flips behavior unless the operator turns
it on.

## Server-streaming RPCs (the other shape)

`stream Resp` methods on registered protos are *not* pub/sub. They
pass through as per-subscriber gRPC streams: gateway opens one
upstream call per WebSocket subscriber (`protoDirectSubscriptionDispatcher`
in `gw/proto_direct_subscription_dispatcher.go`), forwards events as
`graphql-transport-ws` next-frames. Reuses the existing gateway-wide
and per-pool `streamSem` backpressure. No multiplexing magic, no
auto-injected HMAC args — auth for the upstream stream is the
upstream's responsibility.

If a service wants multi-listener fan-out for a stream, it calls
`ps.pub` from the handler and clients call `ps.sub` on the same
channel. The two shapes don't pretend to be each other.

## Signing tokens

Tokens for the HMAC and `ChannelAuthDelegate` tiers come from the
gateway's `SignSubscriptionToken` control-plane RPC. The downstream
service that already authenticated the user does its own permission
check, then mints a client token via `SignSubscriptionToken` —
authorization stays where the request context lives. The gateway
verifies on subscribe; it doesn't learn your authz model.

CLI surface:

```bash
# Remote (over the gateway's control plane; bearer required).
gwag sign --gateway localhost:50090 --bearer "$ADMIN_OR_SIGNER_TOKEN" \
          --channel "events.orders.42.update" --ttl 60
# hmac=md6l2SVJ...
# ts=1778092482
# kid=

# Local (pure crypto; no gateway round-trip).
gwag sign --secret "$HEX_SECRET" \
          --channel "events.orders.42.update" --ttl 60
```

Sign-side bearer: `WithSignerSecret(...)` (separate from admin/boot
token; lower blast radius) or the admin/boot token as unconditional
fallback. In-process callers bypass the gate — the trust boundary is
the embedder, not the wire.

## Migration from the pre-1.0 implicit-channel transform

Earlier versions auto-transformed `rpc Method(...) returns (stream
Resp)` into a NATS-backed subscription with auto-injected
`hmac`/`timestamp`/`kid` args. That transform is gone. `stream Resp`
now opens a per-subscriber gRPC stream upstream — no auth injection,
no implicit fan-out.

If you relied on the implicit fan-out, switch to explicit `ps.pub`
on the service side and `ps.sub(channel: ..., ...)` on the client
side. If you relied on the auto-injected auth args, the HMAC gate
now belongs to `ps.sub` via `WithChannelAuth`.

## See also

- [`docs/architecture.md`](./architecture.md) — IR layer, slot model, cluster reconciler
- [`docs/cluster.md`](./cluster.md) — NATS / JetStream KV that backs the broker
- [`docs/admin-auth.md`](./admin-auth.md) — `AdminAuthorizer` delegate pattern that `ChannelAuthDelegate` mirrors
