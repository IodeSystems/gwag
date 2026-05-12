# Caller identity & quota

Operator progression on metrics is predictable: global RPS → per-service
RPS → per-caller RPS → quota per caller. The first two rungs ship by
default (Prometheus labels on `namespace` / `version`); the next two
opt in via one extractor seam plus one delegate.

## One seam, four flavors

`CallerIDExtractor func(ctx) (string, error)` resolves the caller-id
on every dispatch. The library ships four implementations; pick one at
boot:

| Flavor | Option | Hot path | Forgeable | Use case |
|---|---|---|---|---|
| Public | `WithCallerIDPublic()` | header read | yes | dev; behind an authenticated reverse proxy |
| HMAC | `WithCallerIDHMAC(o)` | HMAC-SHA256 verify | no (without secret) | production default for untrusted ingress |
| Delegated | `WithCallerIDDelegated(o)` | TTL-cached delegate RPC | issuer-controlled | token → caller-id with side attrs |
| mTLS via proxy | proxy injects `X-Caller-Id` | n/a at gateway | no (proxy verifies cert) | operator-side; gateway runs Public |

`caller_id` and the credential material (kid, token) are separate
axes — rotate the credential without invalidating dashboards built on
`caller_id`. Same pattern subscriptions use.

## Public mode

```go
gw := gateway.New(gateway.WithCallerIDPublic())
```

Reads `X-Caller-Id` (HTTP / WebSocket upgrade) or `caller-id` (gRPC
metadata). Forgeable by design — appropriate for development, for an
mTLS-terminating reverse proxy that injects the header after cert
verification, or for any trusted-network deployment.

## HMAC mode

Production answer for untrusted ingress. The caller signs
`(caller_id, timestamp, kid)` with a shared secret; the gateway
verifies on every dispatch:

```go
gw := gateway.New(gateway.WithCallerIDHMAC(gateway.CallerIDHMACOptions{
    Secrets: map[string][]byte{
        "k1": secretV1,
        "k2": secretV2,  // rotate by adding the new entry, switching signers, then dropping the old
    },
    SkewWindow: 5 * time.Minute,
}))
```

Wire format (HTTP / gRPC):

| HTTP header | gRPC metadata |
|---|---|
| `X-Caller-Id` | `caller-id` |
| `X-Caller-Timestamp` | `caller-timestamp` |
| `X-Caller-Kid` | `caller-kid` |
| `X-Caller-Signature` | `caller-signature` |

Caller-side helper for minting tokens:

```go
sig, kid, ts := gateway.SignCallerIDTokenWithKid(secretV1, "k1", "alice", 60)
req.Header.Set(gateway.PublicCallerIDHeader,        "alice")
req.Header.Set(gateway.HMACCallerIDTimestampHeader, strconv.FormatInt(ts, 10))
req.Header.Set(gateway.HMACCallerIDKidHeader,       kid)
req.Header.Set(gateway.HMACCallerIDSignatureHeader, sig)
```

Same verification primitive the subscription HMAC channel auth uses —
one rotation knob, one skew window.

## Delegated mode

For opaque tokens that resolve to a caller-id at issue time:

```go
gw := gateway.New(gateway.WithCallerIDDelegated(gateway.CallerIDDelegatedOptions{
    TTL:         60 * time.Second,
    NegativeTTL: 30 * time.Second,
    Timeout:     3 * time.Second,
}))
```

The gateway reads an opaque token from `X-Caller-Token` /
`caller-token`, looks it up in a local TTL cache, and on a miss calls
`_caller_auth/v1::Authorize(token)`. A `CallerAuthorizer` service
registered under that namespace returns `{caller_id, code, ttl_seconds}`.
Concurrent misses are singleflight-collapsed — a token-rotation
thundering herd produces one RPC per `(gateway, token)`, not one per
request. Hit rate target is >99.9% under steady-state traffic.

Negative invalidation: the delegate publishes a `TokenRevoked` event
on `events.caller_auth.Revoked` (NATS); every gateway in the cluster
drops the matching cache entry without waiting for TTL.

## Enforce mode

By default, unresolved or anonymous callers are recorded as
`caller="unknown"` and the request proceeds — the day-1 posture
prevents adopters from bricking their own traffic while wiring the
extractor. Once dashboards confirm every legitimate path carries an
id, flip to reject-anonymous:

```go
gw := gateway.New(
    gateway.WithCallerIDHMAC(opts),
    gateway.WithCallerIDEnforce(),
)
```

Anonymous / failed-extraction requests now short-circuit with
`CodeUnauthenticated` (HTTP 401 / gRPC 16) before they reach the
service. Subscriptions are not gated — the HMAC channel auth on
subscribe is the per-subscription seam.

## Metrics cardinality cap

Untrusted ingress can flood the `caller` label and blow up Prometheus.
Cap admitted labels with `WithCallerIDMetricsTopK(k)` — the first `k`
distinct callers get their own buckets; everyone else folds into
`__other__` (`gateway.OtherCallerID`). Admission is LRU-bumped on
every hit so a burst of one-off callers can't displace steady
traffic from real services:

```go
gw := gateway.New(
    gateway.WithCallerIDPublic(),
    gateway.WithCallerIDMetricsTopK(1000),
)
```

## Quota: block-permit pattern

`WithQuota(opts)` adds a per-`(caller_id, namespace, version)` permit
bucket. The bucket is debited locally; when it empties the gateway
calls `_quota_auth/v1::AcquireBlock(caller_id, namespace, version,
requested_permits)` to refill it. No per-request RPC on the hot path:

```go
gw := gateway.New(
    gateway.WithCallerIDHMAC(hmacOpts),
    gateway.WithQuota(gateway.QuotaOptions{
        BlockSize:        100,     // permits requested per refill
        MaxBlockSize:     10_000,  // caps a misbehaving delegate
        EmergencyPermits: 1,       // fallback when delegate is down
        EmergencyTTL:     5 * time.Second,
        Timeout:          3 * time.Second,
    }),
)
```

The `QuotaAuthorizer` delegate replies with
`{granted_permits, valid_until}` — `valid_until` lets it enforce
time-windowed quotas (per-second, per-minute) without a per-request
consult.

Code policy:

- **OK** — bucket refilled; dispatch proceeds.
- **DENIED** — gateway rejects with `CodeResourceExhausted` (HTTP 429 +
  `Retry-After`, GraphQL `extensions.retryAfterSeconds`, gRPC 8).
- **UNAVAILABLE / NOT_CONFIGURED / transport error** — gateway grants
  an `EmergencyPermits`-sized block so a degraded quota service doesn't
  brick traffic. `WithQuotaEnforce()` flips this to fail-closed for
  surfaces where bypass-on-outage is unacceptable (e.g. paid-tier).

Concurrent refill misses are singleflight-collapsed; per-bucket debits
use the bucket's own mutex so distinct callers never contend on the
hot path. Subscription dispatch bypasses the quota gate — pair with the
HMAC channel-auth seam on subscribe instead.

## Known gaps

- **Permits are per-gateway, not cluster-shared.** A caller hitting
  three gateways at burst sees up to 3× the configured burst.
  Cluster-shared accounting needs a JetStream-backed counter or
  delegate-side coordination; deferred until use case.
- **Public mode is forgeable by design.** Public mode on an open
  ingress is a configuration mistake; use HMAC or mTLS-via-proxy for
  untrusted networks.
