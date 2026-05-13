# Operations: health, drain, backpressure, metrics

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

## Backpressure

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

## Metrics

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

## WebSocket upgrade caps

`gw.Handler()` accepts graphql-transport-ws upgrades for subscriptions.
The pool's `MaxStreams` and the gateway-wide `MaxStreamsTotal` cap
*post-handshake* stream count; they don't protect the handshake itself.
A single misbehaving peer can open thousands of upgrades before hitting
the stream cap, exhausting file descriptors and `streamSem`. `WithWSLimit`
adds two per-peer-IP caps applied before `Accept`:

```go
gw := gateway.New(gateway.WithWSLimit(gateway.WSLimitOptions{
    MaxPerIP:   64,                // concurrent connections per peer
    RatePerSec: 10,                // upgrade token-bucket refill
    Burst:      20,                // token-bucket capacity (defaults to RatePerSec)
    TrustedIPs: []string{"10.0.0.1"}, // skip the cap for known proxies
}))
```

Rejected upgrades return HTTP 429 and increment
`go_api_gateway_ws_rejected_total{reason="max_per_ip"|"rate_limit"}`.

Set this when the gateway is the outermost TLS / WebSocket
terminator. If an upstream reverse proxy (nginx, HAProxy, Cloudflare,
ALB) already enforces per-IP caps, `MaxPerIP` would key on the proxy
address — leave it unset or add the proxy to `TrustedIPs`.

Peer IP comes from `r.RemoteAddr` (port stripped). The limiter does
not honour `X-Forwarded-For` — spoofable headers are not a useful
rate-limit key.
