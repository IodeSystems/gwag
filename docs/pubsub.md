# Pub/Sub

> **Status: v1 design, in flight.** The dispatch path and channel→type
> registry land with the pre-1.0 pub/sub workstream — current
> implementation status in [`plan.md`](./plan.md) Tier 1. This doc is
> the destination; what works today may lag the description below
> until the workstream merges.

The gateway provides pub/sub as a *primitive*, not a transform
applied to every server-streaming RPC. Two GraphQL fields, both
gateway-defined; service-declared `stream Resp` methods stay as
honest per-subscriber gRPC streams.

```graphql
type Mutation {
  ps { pub(channel: String!, payload: String!, hmac: String, ts: Int): Boolean! }
}
type Subscription {
  ps { sub(channel: String!, hmac: String, ts: Int): Event! }
}
type ps.Event {
  channel: String!
  payload: String!
  payload_type: String!   # fully-qualified proto message name; blank if channel is unbound
  ts: Int!
}
```

Payload is `string` — JSON, base64, agreed-encoding-of-the-day; broker
doesn't care. Subscribers decode per the `payload_type` label
cross-referenced against `/schema/proto?service=...`.

## Channel naming

NATS subjects: `events.<namespace>.<thing>.<subject>` — free-form,
dot-segmented. Wildcards (`*` for one segment, `>` for the rest)
work on `ps.sub`:

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
sub on `events.orders.>`. Operators issuing tokens control the
pattern surface.

## Auth tiers

Operator configures per channel pattern via `WithChannelAuth(pattern, tier)`.
Patterns share the NATS wildcard grammar.

| Tier | What |
|---|---|
| `open` | No auth; `hmac` / `ts` ignored. Dev / public channels. |
| `hmac` | HMAC token over `(channel, ts)`, signed by a key the operator controls. Hot-path crypto-fast. |
| `delegate+hmac` | HMAC first, then a delegate authorizer at `_pubsub_auth/v1` (same fall-through posture as `_admin_auth/v1`). Use when authz needs request context the gateway doesn't carry. |

```go
gw := gateway.New(
    gateway.WithChannelAuth("events.public.*",  gateway.AuthOpen),
    gateway.WithChannelAuth("events.private.*", gateway.AuthHmac),
    gateway.WithChannelAuth("events.admin.>",   gateway.AuthDelegateHmac),
)
```

Patterns match in declaration order, first hit wins. Default when no
pattern matches: `hmac`. For wildcard subs that span multiple
patterns at different tiers, the **strictest tier wins** (max across
reachable patterns) — first-match-wins would leak events from private
channels through wildcards subscribed at permissive ones.

## Channel → payload-type registry

Typemap from channel pattern to proto message type. Two registration
paths, same registry:

**Proto-declarative (canonical).** Custom message option in the
service's `.proto`:

```proto
import "gwag/ps/v1/options.proto";

message OrderUpdateEvent {
  option (gwag.ps.binding) = { pattern: "events.orders.*.update"; };
  string order_id = 1;
  string action = 2;
}
```

Rides `proto_source` for cluster propagation — bindings travel with
the existing service registration, no separate KV bucket.

**Runtime API (escape hatch).** For non-proto adopters or
gateway-shipped defaults:

```go
gw := gateway.New(
    gateway.WithChannelBinding("events.orders.*.update", (*orderv1.OrderUpdateEvent)(nil)),
)
```

On `Sub` delivery, the gateway stamps `Event.payload_type` with the
matched binding's fully-qualified message name. Subscribers fetch the
descriptor at any time via `/schema/proto?service=...` for validation
or codegen.

**Tier policy inherits.** `unstable` slots' bindings overwrite on
rebake; `vN` slots' bindings lock by `(pattern, payload_fqn)` —
versioned messaging gets the existing version-tier story.

**Pattern uniqueness across slots.** Two different `(namespace, version)`
slots can't both claim the same pattern. Conflict is hard-rejected at
registration. Same-slot rebake on `unstable` swaps the binding; on
`vN` it's `AlreadyExists`.

## Contract enforcement (opt-in)

Defaults are documentation-only — broker stays permissive. Two
independent strictness axes:

- `WithChannelBindingEnforce()` — **shape strictness.** At Pub entry,
  parse payload as the bound proto message; reject mismatch with
  `InvalidArgument`. Mirrors `WithCallerIDEnforce` / `WithQuotaEnforce`.
- `WithStrictPayloadTypes()` — **coverage strictness.** Reject `Sub`
  deliveries from channels matching no `WithChannelBinding` pattern
  (where `payload_type` would otherwise be blank).

Both can be enabled together. Default: utility first, strictness as
opt-in upgrade — neither flag flips behavior unless the operator turns
it on.

## Server-streaming RPCs (the other shape)

`stream Resp` methods on registered protos are *not* pub/sub. They
pass through as per-subscriber gRPC streams: gateway opens one
upstream call per WebSocket subscriber, forwards events as
`graphql-transport-ws` next-frames. Reuses existing `streamSem`
backpressure. No multiplexing magic.

If a service wants multi-listener semantics for a stream, it calls
`ps.pub` from the handler and clients `ps.sub` to the same channel.
The two shapes don't pretend to be each other.

## Signing tokens

The gateway publishes a `Sign` admin RPC for minting HMAC tokens
(same primitive across pub/sub and caller-identity HMAC):

```bash
gwag sign --gateway gw:50090 \
          --channel "events.orders.42.update" --ttl 60
# hmac=md6l2SVJ...
# ts=1778092482
```

The downstream service that already authenticates the user does its
own permission check, then calls `Sign` to mint a client token —
authorization stays where the request context lives. The gateway
verifies on subscribe; it doesn't learn your authz model.

Sign-side bearer protection: `WithSignerSecret(...)` (separate from
the admin/boot token, lower blast radius) or the boot token as
unconditional fallback. In-process callers bypass the gate; the trust
boundary is the embedder, not the wire. Verification outcomes land in
`go_api_gateway_subscribe_auth_total{code=...}`.
