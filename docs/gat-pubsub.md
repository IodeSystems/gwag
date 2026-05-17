# gat pubsub

`gat` ships a publish/subscribe primitive with optional best-effort
cross-node fanout. It is separate from GraphQL subscriptions (which
gat does not have) and from gwag's NATS-backed pub/sub (durable,
broker-backed). gat pubsub is in-memory, broker-free, and best-effort
— the right fit for live-update use cases across a small set of gat
instances behind a load balancer.

## Single node

`Gateway.PubSub()` is always available — no configuration:

```go
g, _ := gat.New(regs...)
defer g.Close()

ps := g.PubSub()

events, cancel := ps.Subscribe("orders.>")
defer cancel()
go func() {
    for ev := range events {
        log.Printf("%s: %s", ev.Channel, ev.Payload)
    }
}()

ps.Publish("orders.42.shipped", []byte(`{"id":42}`))
```

Channel patterns are NATS-style: segments split on `.`, `*` matches
exactly one segment, `>` matches one or more trailing segments and
must be the last token. `orders.*` matches `orders.42` but not
`orders.42.shipped`; `orders.>` matches both.

Delivery is best-effort. Each subscriber has a bounded buffer; a
consumer that falls behind loses its oldest queued events rather than
stalling the publisher.

## Clients (WebSocket)

`RegisterHTTP` and `RegisterHuma` mount a WebSocket stream:

```
GET {prefix}/_gat/subscribe?channel=orders.>   (WebSocket upgrade)
```

Each matching event arrives as a text frame carrying `{channel,
payload}` JSON (`payload` base64-encoded). It is a server-to-client
stream — the client sends nothing after the handshake. The
subscription ends when the client disconnects or the gateway closes.

### Subscribe auth

By default the subscribe endpoint is open — anyone who can reach it
streams any channel. `EnableSubscribeAuth` gates it behind HMAC
tokens:

```go
g.EnableSubscribeAuth(subscribeSecret) // distinct from PeerMesh.Auth
```

A client mints a token for the channel it wants and passes it on the
WebSocket URL:

```go
token, ts := gat.SignSubscribeToken(subscribeSecret, "orders.>")
// → GET {prefix}/_gat/subscribe?channel=orders.>&token=<token>&ts=<ts>
```

The token is `HMAC-SHA256(channel, ts)` — bound to one channel
pattern — and is accepted within ±5 minutes of `ts`. A missing,
malformed, expired, or wrong token is rejected with `401` before the
WebSocket handshake completes.

The subscribe-auth secret is **separate from the peer-mesh `Auth`
key**: gateway↔client trust and gateway↔gateway trust are different
domains. Leaving `EnableSubscribeAuth` unset keeps the endpoint open
— acceptable only on a trusted network.

## Cross-node peer mesh

Adopters running N gat instances behind a load balancer have only
in-process pubsub by default — an event published on node A doesn't
reach a subscriber on node B. `EnablePeerMesh` adds best-effort
cross-node fanout:

```go
g, _ := gat.New(regs...)
g.EnablePeerMesh(gat.PeerMesh{
    Peers: []string{"https://gat-2.internal/api", "https://gat-3.internal/api"},
    Auth:  sharedSecret, // HMAC key every mesh member holds
})
defer g.Close()
```

`Peers` are the peer gat base URLs — the `RegisterHTTP` /
`RegisterHuma` prefix root. Call `EnablePeerMesh` once, before
serving traffic, and before `RegisterHTTP` / `RegisterHuma` (the
peer-receive endpoint is mounted only when the mesh is enabled).

With the mesh on, `Publish` delivers locally and also fire-and-forgets
the event to every peer's `{prefix}/_gat/publish` endpoint. Each peer
fans the received event out to its own local subscribers — and stops
there.

### Semantics

- **Local fanout always succeeds.** Cross-node delivery is
  fire-and-forget; a peer being down never fails or slows `Publish`.
- **One hop.** A received-from-peer event fans out local-only — it is
  never re-broadcast. This caps fanout and removes the need for
  message-id dedup to break loops.
- **At-most-once per source.** Two nodes publishing the same event
  independently deliver two events. Dedup, if needed, is the
  subscriber's job.
- **No global ordering.** Each subscriber sees events in its own
  node's receive order.
- **Per-peer bounded queue + circuit breaker.** A slow or dead peer
  has its events dropped (queue overflow) rather than backing up the
  publisher; after consecutive failures the peer's circuit opens and
  half-open-probes after a cooldown.

### Auth

`PeerMesh.Auth` is a shared HMAC-SHA256 secret. Every outbound peer
POST is signed over the request body; the receive endpoint rejects a
bad or missing signature with `401`. An empty `Auth` disables
verification — only acceptable on a trusted network, or when an edge
proxy terminates mTLS in front of the mesh.

## Lifecycle

`Gateway.Close()` stops the peer-fanout goroutines and cancels every
active subscription. A gateway is single-use after `Close`.

## When to graduate to gwag

gat pubsub is deliberately lossy and broker-free. Move to full gwag
(NATS + JetStream) when you need any of: durable subscriptions,
replay, global ordering, exactly-once semantics, or a mesh larger
than a handful of nodes. The `Publish` / `Subscribe` call shape is
close enough that the migration is mostly swapping the gateway
construction.

## Limitations

- **Static peer list.** `PeerMesh.Peers` is resolved once at
  `EnablePeerMesh`. A dynamic `PeerProvider` (DNS-resolved peer sets
  for k8s headless services) is a planned followup.
- **No persistence, no replay, no durable subscriptions.** An event
  published while a subscriber is disconnected is gone. Those
  guarantees graduate to gwag.
- **Best-effort delivery throughout.** Subscriber buffer overflow and
  peer queue overflow both drop events silently.
