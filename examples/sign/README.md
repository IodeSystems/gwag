# examples/sign

Sign-as-API worked example for plan §2 (subscription-signer collapse).
Demonstrates the post-§2.3 model: the gateway is a pure signer, a
downstream service holds the signer secret, does its own authz, and
calls the gateway over gRPC to mint subscription tokens.

## What runs

One binary, three things in-process:

| Component | Listens | Role |
|---|---|---|
| Gateway GraphQL | `:8080` | Public surface (queries / mutations / subscriptions). |
| Control-plane gRPC | `:50090` | `SignSubscriptionToken` lives here. **Bearer-gated**. |
| Auth-shim HTTP | `:8090` | Receives client subscribe-token requests, does business authz, calls back into the gateway over gRPC with the signer-secret bearer. |

The shim is the part you'd write in your own backend. The gateway is
the library — it never sees your user identities or entitlements.

## Run

```
$ go run ./examples/sign
control plane listening on 127.0.0.1:50090
graphql listening on 127.0.0.1:8080
auth-shim listening on 127.0.0.1:8090
```

If the default ports collide, override with `--graphql`,
`--control-plane`, `--shim`.

## Try it

Alice signing her own channel works:

```
$ curl -sS -X POST http://localhost:8090/subscribe-token \
    -H 'Authorization: Bearer demo-user-alice' \
    -H 'Content-Type: application/json' \
    -d '{"channel":"events.user.alice.likes","ttl_seconds":60}'
{"hmac":"…","timestamp":1778012345,"channel":"events.user.alice.likes","kid":""}
```

Alice trying to sign bob's channel is rejected by the **shim**, not
the gateway:

```
$ curl -sS -X POST http://localhost:8090/subscribe-token \
    -H 'Authorization: Bearer demo-user-alice' \
    -d '{"channel":"events.user.bob.likes"}'
{"error":"forbidden"}
```

A request with no shim bearer is rejected by the shim before any
sign call goes out:

```
$ curl -sS -X POST http://localhost:8090/subscribe-token \
    -d '{"channel":"events.user.alice"}'
{"error":"unauthenticated"}
```

Bypassing the shim and hitting `SignSubscriptionToken` directly is
gated by the gateway:

```
$ gwag sign --gateway 127.0.0.1:50090 \
           --channel events.user.alice
--bearer is required with --gateway (the sign endpoint is bearer-gated)

$ gwag sign --gateway 127.0.0.1:50090 \
           --bearer 11111111111111111111111111111111 \
           --channel events.user.alice
hmac=…
ts=1778012345
```

## Why this shape

Inverts the earlier "authorizer delegate" model where the gateway
called back out at sign time. That forced the gateway to *predict*
what context the authorizer needed (user ID? IP? scope? custom
claims?) and bake it into a delegate proto. The signer-as-API model
puts the authz decision in the service that already has full request
context — composition over prediction.

Two bearers, two roles:

- **Shim's inbound bearer** (`Bearer demo-user-<id>`) — authenticates
  the *user* to the shim. Real services use JWTs, sessions, etc.
- **Gateway's inbound bearer** (`Bearer <signer-secret>`) —
  authenticates the *service* to the gateway. "This service speaks
  for me; sign what it asks." The gateway has zero opinion about
  the end user.

The signer secret is rotatable independently of the gateway's
admin/boot token (plan §2.1) — leak the signer secret, rotate it,
admin token unaffected.

## What's intentionally toy

- `allowSubscribe` is a one-line prefix check (`alice` can sign
  `events.user.alice.*`). Real services consult their entitlement
  model.
- `userFromBearer` accepts any string after `Bearer demo-user-`.
  Real services validate signed tokens.
- Demo secrets are hardcoded hex (`11…`, `22…`). Real deployments
  use `--signer-secret $(openssl rand -hex 32)` or equivalent and
  store the value out-of-band.
