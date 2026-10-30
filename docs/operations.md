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

## Upstream timeouts, retries, ejection

These apply to unary dispatch to gRPC pools, OpenAPI sources and
downstream GraphQL sources. Subscriptions are not affected.

**Timeout.** None by default. Set one per service with
`gateway.Timeout(d)` (or `timeout_ms` on the control-plane
`ServiceBinding`), or a default for every service with
`gateway.WithUpstreamTimeout(d)`. The service value wins. A call that
runs past it fails with `upstream timeout after <d>` as a normal
field error. The timeout covers the upstream call only, not the wait
for a backpressure slot (`MaxWaitTime` covers that).

**Retry.** The gateway re-sends a call only when it can prove the
request never reached an upstream, and only to a replica it has not
tried yet (at most 3 attempts per call, inside the same timeout):

- gRPC: the call failed `Unavailable` and grpc-go never opened a
  transport stream for it (connect refused, no connection). A
  server that returns `Unavailable` itself is not retried. Only for
  connections the gateway dials (`To("host:port")`, control plane,
  cluster); a caller-supplied `grpc.ClientConnInterface` is never
  retried.
- OpenAPI / GraphQL: the HTTP client failed before it got a
  connection (`httptrace` `GotConn` never fired). Only for clients
  that use `*http.Transport`; OpenAPI requests with uploads are never
  retried.

5xx responses, timeouts, and errors after a connection was made are
never retried, whatever the HTTP method.

**Ejection.** On by default. After 5 consecutive failures (connect
errors, timeouts, HTTP 5xx, gRPC `Unavailable`) a replica gets no
traffic for 10 s, then is tried again; a success resets the count,
and a readmitted replica that fails again is ejected again at once.
When every replica of a service is ejected the gateway still sends to
one, so a single-replica service behaves the same with or without
ejection. Tune with `gateway.WithReplicaEjection(failures, cooldown)`;
`failures <= 0` turns it off.

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

The `go_api_gateway_` prefix is the default. `gateway.WithMetricPrefix("gwag")`
emits `gwag_*` names instead (same suffixes and labels, one set of series,
no dual emission). v3 makes `gwag_*` the default; set the prefix now to
migrate dashboards ahead of time, or `WithMetricPrefix("go_api_gateway")`
to pin the old names across the v3 upgrade.

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

## Request limits

The GraphQL endpoint refuses oversized work before it dispatches
anything. Two limits are on by default:

| Option | Default | What it refuses |
|---|---|---|
| `WithMaxOperations(n)` | 64 | A document selecting more than `n` operations, aliases and fragment spreads included; a batch (JSON array body) of more than `n` documents. |
| `WithMaxRequestBytes(n)` | 1 MiB | A non-multipart body over `n` bytes (413). Upload bodies follow `WithUploadLimit`. |
| `WithMaxDepth(n)` | off | Fields nested deeper than `n`, the namespace field included. |
| `WithoutIntrospection()` | off | `__schema` / `__type`. The schema export handlers keep serving; mount them where only your tooling reaches them. |

Pass `0` to turn a default off. The operation, depth and introspection
checks are validation rules in the plan cache: each runs once per
distinct query, and a cache hit pays nothing.

A body that is not valid JSON, carries no query, or has `variables`
that are not an object (or a string holding one) is refused with 400
before any of this runs.

### Trusted documents

`WithTrustedDocuments(docs, enforce)` takes the manifest
graphql-codegen's client preset writes with `persistedDocuments`
(`ParseTrustedDocuments` reads it). Clients then send an id instead of
the query: `{"documentId": "<id>"}`, Apollo APQ's
`extensions.persistedQuery.sha256Hash`, or `GET ?documentId=<id>`. A
known id skips parsing and validation. With `enforce` true, query text
is refused (400 `TRUSTED_DOCUMENT_REQUIRED`), which closes a public
endpoint to arbitrary queries. Unknown ids answer
`PERSISTED_QUERY_NOT_FOUND`.
