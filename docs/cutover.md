# Cluster cutover

A cutover moves client traffic from a running cluster (blue) to a
separate one (green). Green can run a new version, or the same version
with other transforms or routing. This is not the replica swap in
[`cluster.md`](./cluster.md#replica-swap), where new nodes join the
same cluster.

What exists now:

- **Replay verification.** Blue records a sample of its requests with
  every upstream call they made; green runs each recorded request
  through its own pipeline, answers every upstream call from the
  recording, and reports what differs.
- **Switchover.** Blue forwards a share of client requests to green
  and returns green's response. The share is set at runtime (5, 50,
  100 percent) and a client stays on one side.

The state copy and the final switch at the load balancer are not built
yet.

## Replay verification

### How it works

1. A client sends a GraphQL request to blue. Blue serves it as usual.
   For a sampled request it records, while serving:
   - the client request (method, path, headers, body) and the client
     response (status, body);
   - each upstream call ("leg"): the request as blue sent it (path,
     headers or gRPC metadata, body) and the response as blue got it.
2. After the response is written, blue puts the recording on a bounded
   queue. A worker posts it to green's verify endpoint. The client
   request does not wait for the queue or for green. When the queue is
   full, the recording is dropped and counted.
3. Green runs the client request through its GraphQL pipeline with
   the client's headers. Each upstream call green makes is paired with
   one of blue's legs and answered with blue's response. Green calls no
   upstream, so a mutation runs once, on blue.
4. Green compares, and returns the result:
   - per leg: the request green built against the request blue sent;
   - the client response green built against blue's.
5. Blue counts the outcome and keeps the recent results that did not
   match.

There is one upstream call per client request in total.

### Upstream kinds

| Kind | Seam | Request compared | Response replayed |
|---|---|---|---|
| `grpc` | gRPC `Invoke` to a pool replica | method path, outgoing metadata, message (decoded with green's descriptor) | message bytes, or status code and message |
| `openapi` | HTTP call of an OpenAPI operation | method, path and query, headers, body | status, headers, body; a transport error as its text |
| `graphql` | HTTP call to a downstream GraphQL service | method, path, headers, body (query and variables) | status, headers, body |
| `mcp` | `tools/call` on an MCP server | tool name and arguments | the tool result, or the error text |
| `inproc` | in-process proto handlers (`ps.pub`, the admin events service) | method path, message | message, or status |

The replica address or base URL is recorded but not compared: replicas
differ per pick and per cluster. On green, an HTTP call is built
against the base URL blue used, so error texts that name it read the
same.

### Pairing and outcomes

A green call is paired with an unused blue leg of the same kind,
namespace, version and operation. When there is none, a blue leg of
the same kind, namespace and operation in another version is used, so
a call routed to another version shows as a `version` diff. Among the
candidates the first with an equal request wins, else the first.

| Outcome | Meaning |
|---|---|
| `match` | every leg request and the client response are equal |
| `diff` | a leg request differs, a blue leg was not called by green (`not_called`), or the client response differs |
| `unverifiable` | green made an upstream call blue never made. Green cannot answer it: the resolver gets an error, which usually also shows as a client response diff |
| `dropped` | blue did not send it: queue full, the request was over `MaxBytes` or `MaxLegs`, or a leg could not be copied (streamed upload body) |
| `error` | green could not be asked (network, auth, non-200) or could not run it |

Not compared: the trace headers gwag stamps or forwards
(`traceparent`, `tracestate`, `baggage`, B3, `x-request-id`,
`x-amzn-trace-id`, `x-cloud-trace-context`) and `grpc-timeout`.
Everything else is compared, injected headers included. There are no
ignore rules: gwag transforms are functions of the client request and
the upstream responses, and both are the same on green. A custom
`Runtime` middleware that reads the clock or makes ids produces real
diffs.

A diff path names where: `legs[0].request.header.x-tenant[0]`,
`legs[1].request.path`, `legs[0].request.version`,
`response.body.data.greeter.hello.greeting`, `response.status`. Values
are JSON; `(absent)` means the side has no value there.

### Configure blue

```go
gw := gateway.New(
    gateway.WithReplayVerify(gateway.ReplayVerifyOptions{
        Target: "https://green.internal:8080/api/admin/replay/verify",
        Token:  os.Getenv("GREEN_ADMIN_TOKEN"), // hex admin token of green
        Match: []gateway.ReplayMatch{
            {OperationName: "CreateOrder", SampleRate: 1},  // every CreateOrder
            {Header: "X-Canary", HeaderValue: "1"},         // canary clients
        },
        SampleRate: 0.01, // other matching requests: 1%
    }),
)
```

| Field | Default | Meaning |
|---|---|---|
| `Target` | required | green's verify URL |
| `Token` | none | bearer sent to green; the endpoint needs the admin role |
| `Client`, `Timeout` | client with 10 s | the HTTP client for posts to green |
| `Match` | all requests | rules; the first that matches sets the rate. Fields: `OperationName` (the request's `operationName`, or the name of the document's only operation), `Header`, `HeaderValue`, `SampleRate` |
| `SampleRate` | 1 | fraction (0..1] of matching requests recorded |
| `QueueSize`, `Workers` | 256, 2 | queue length and posting goroutines |
| `MaxBytes`, `MaxLegs` | 4 MiB, 128 | caps per recording |
| `Keep` | 100 | recent non-matching results kept for `GET /admin/replay/diffs` |
| `LogEvery` | 10 s | least time between two diff log lines |
| `Redact` | none | `func(*ReplayExchange) bool`, runs on the worker before the post; may change the recording, `false` drops it uncounted |

Recorded: `POST` requests with `Content-Type: application/json` to
`Handler()`. Not recorded: batches (a JSON array), multipart uploads,
`GET` queries, WebSocket and SSE subscriptions, the HTTP/JSON ingress
(`IngressHandler`), gRPC and Connect ingress.

The admin token is per node unless pinned. Behind a load balancer in
front of several green nodes, pin the same token on each
(`WithAdminToken`) or use an `AdminAuthorizer` delegate, so one
credential works on every node.

### Configure green

```go
gw := gateway.New(gateway.WithReplayVerifyEndpoint(), /* ... */)
adminMux, _, _ := gw.AdminHumaRouter()
mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))
```

`POST /admin/replay/verify` takes a `ReplayExchange` JSON body (up to
64 MiB) and returns a `ReplayResult`. It needs the admin role: it runs
a request as whatever client the recording names, and its result holds
the upstream requests green built, injected header values included.
It is a plain route on the admin mux, not an admin GraphQL operation.
Without `WithReplayVerifyEndpoint` it answers 404.

Green must have the same services registered (services register with
both clusters). A replay does not need a live replica: the upstream
address comes from the recording.

Replays are not client traffic on green: no request metrics, no
request log line, no dispatch metrics or stats. They do take pool
dispatch slots (backpressure applies).

`Gateway.ReplayVerify(ctx, *ReplayExchange)` is the same check as a Go
call, for tools and tests.

### Read the results

- Metric `replay_verify_total{outcome}` on blue (default Prometheus
  sink only): `match`, `diff`, `unverifiable`, `dropped`, `error`.
- Log line on blue, at most one per `LogEvery`, with the id, operation
  name, outcome and the first diff.
- `GET /admin/replay/diffs` on blue (`listReplayDiffs`, admin role):
  outcome counts and the last `Keep` results that did not match, with
  their legs and diffs.

### Data in recordings

A recording carries real payloads: client request headers
(`Authorization` included), bodies, upstream requests with injected
headers, and upstream responses. It goes from blue to green over the
`Target` URL; use https. Recent diffs on blue hold payload values and
are readable by the admin role only. Use `Redact` to remove what green
does not need; a redacted value that green's pipeline would produce
differently shows as a diff.

### Limits

- Calls made outside gwag's dispatch run for real on green:
  `InjectType` / `InjectPath` resolvers with their own clients, and
  the caller-id, quota and `AdminAuthorizer` delegates. A quota
  delegate counts replayed requests.
- When several calls to one operation differ from all of blue's legs,
  pairing is by order and a diff can name the wrong pair. The outcome
  is still `diff`.
- An upstream timeout on blue replays as the raw upstream error, not
  as gwag's `upstream timeout after` text, so the client response
  differs.
- A request whose upstream body is streamed (multipart upload) is not
  recorded.

Tested in `gw/replay_verify_test.go`: match, `unverifiable` and
`not_called` for each kind (one upstream call in total, mutations
included); header diffs from `InjectHeader` (gRPC, OpenAPI) and
`ForwardHeaders` (OpenAPI, downstream GraphQL); routing diffs (gRPC to
another version, OpenAPI operation on another path); a client
response diff from a `Runtime` middleware; match rules; queue-full
drops with green stuck; two gateways through the admin endpoint with
token checks, the metric and the diffs route. Not tested: a real
two-cluster deployment, load, and recordings near the size caps.

## Switchover

Clients keep calling blue. Blue sends a share of the GraphQL requests
to green's GraphQL endpoint and returns green's response to the
client. The request goes as the client sent it, with the client's
headers (`Authorization`, caller-id headers): green authenticates the
client itself. Blue needs no credential for green.

### Which requests go to green

Rules are like replay verification's match rules. The first rule that
matches decides; a request that matches no rule stays on blue. With no
rules every request matches at the global percent.

| Rule field | Meaning |
|---|---|
| `OperationName` | the request's `operationName`, or the name of the document's only operation |
| `Header`, `HeaderValue` | a header that must be present, and its value |
| `Percent` | 1..100, the share for this rule; 0 uses the global percent |
| `Exclude` | the matching requests stay on blue |

A client stays on one side. Its key is the caller id
([caller-identity.md](./caller-identity.md): `WithCallerIDPublic`,
HMAC, delegated, or `WithCallerHeaders`), else the `StickyHeader`
(for example `X-Client-Id`), else the source address (`RemoteAddr`
without the port). The key goes to a bucket, `fnv32a(key) mod 100`;
the request goes to green when the bucket is below the percent. The
hash has no seed, so every blue node puts a client in the same bucket.
A client's bucket never changes, so raising the percent only moves
clients from blue to green, never back. Lowering it moves them back.
A change of rules can move a client either way.

Behind a proxy or load balancer, `RemoteAddr` is the proxy for every
client: without a caller id or `StickyHeader`, all clients then share
one bucket. Use `StickyHeader: "X-Forwarded-For"` when the proxy sets
it.

All rules use the client's one bucket. A client can be on green for
one operation (a rule at 50) and on blue for another (a rule at 5).

Forwarded: `POST` with `Content-Type: application/json`, one operation
(not a batch), body up to `MaxBytes`; and WebSocket upgrades
(`graphql-transport-ws`). A WebSocket upgrade has no operation name, so
rules with `OperationName` do not match it. Not forwarded, served on
blue: `GET` queries, multipart uploads, batches, larger bodies, the
HTTP/JSON, gRPC and Connect ingresses, and requests that carry
`X-Gwag-Switchover` (a request blue already forwarded; two gateways
that point at each other do not loop).

### Configure blue

```go
gw := gateway.New(
    gateway.WithSwitchover(gateway.SwitchoverOptions{
        Target:       "https://green.internal:8080/api/graphql",
        StickyHeader: "X-Client-Id",
        // Percent and Rules: the static share. Usually 0 here; the
        // share is set at runtime.
    }),
)
```

`gwag up --switchover [--switchover-target URL] [--switchover-sticky-header NAME]`
does the same for the binary.

| Field | Default | Meaning |
|---|---|---|
| `Target` | none | green's GraphQL URL. Empty: nothing is forwarded until `setSwitchover` sets one |
| `Percent` | 0 | share (0..100) of matching requests sent to green |
| `Rules` | none | see above |
| `StickyHeader` | none | header used as the key when there is no caller id |
| `Client` | no timeout, no redirects | HTTP client for green. A `Client.Timeout` also ends SSE responses |
| `Timeout` | 30 s | until green's complete answer (SSE or `multipart/mixed`: until its headers) |
| `MaxBytes` | 4 MiB | larger request bodies stay on blue |
| `Keep`, `LogEvery` | 50, 10 s | recent green errors kept; least time between two error log lines |

### Set the share at runtime

```
gwag cluster switchover --gateway http://blue:8080 --percent 5
gwag cluster switchover --gateway http://blue:8080 --percent 50 --rule op=CreateOrder,exclude --rule percent=50
gwag cluster switchover --gateway http://blue:8080              # show
gwag cluster switchover --gateway http://blue:8080 --clear      # back to the static options
```

`--gateway` is blue's HTTP endpoint here, not the control-plane
address. A rule spec is comma-separated `op=NAME`, `header=NAME`,
`value=VALUE`, `percent=N`, `exclude`. `--target URL` sets green's URL
when the gateway has none or it changes.

Admin ops: `getSwitchover` (`GET /admin/switchover`, dev role),
`setSwitchover` (`PUT /admin/switchover`, body
`{target, percent, rules}`, admin role), `clearSwitchover`
(`DELETE /admin/switchover`, admin role). The runtime setting replaces
the static `Target` (when it sets one), `Percent` and `Rules` as a
whole. A gateway without `WithSwitchover` answers 409.

In a cluster the setting is stored in the `gwag-cluster` KV bucket,
key `switchover`. Every node with `WithSwitchover` reads it from the
stream leader once a second and applies it, so all blue nodes use the
same share within about a second. The node that took the write applies
it at once. A failed read keeps the setting in force. A node without
`WithSwitchover` ignores the key. Standalone, the setting is in memory
and is lost at restart.

### When green fails

Green failed when:

| Reason | Meaning | Green ran the request? |
|---|---|---|
| `connect` | the request was not completely sent: dial, TLS or write error | no |
| `timeout` | no complete answer within `Timeout` after the request was sent | maybe |
| `transport` | the connection failed after the request was sent | maybe |
| `status` | HTTP 5xx, 404, 405, 1xx or 3xx | maybe |
| `bad_response` | the body is not JSON with `data` or `errors` (a login page, a proxy error page) | maybe |

A response with `errors` from green's resolvers is not a failure: it
is green's answer and goes to the client. So is a 4xx with a GraphQL
body (gwag answers parse and validation errors that way). A
compressed body (`Content-Encoding`) is passed on without the check.

On a failure blue serves the request itself when that cannot run it
twice:

- a query or subscription: always;
- a mutation: only for `connect`. For the other reasons green may
  have run it; the client gets HTTP 502 with a GraphQL error
  (`extensions.code` `unavailable`, `extensions.switchoverReason`). A
  request whose operation type cannot be read (no query text, as with
  a persisted document id; several operations and no
  `operationName`) counts as a mutation.

A client that disconnects is not a green failure; nothing is counted.

Every failure is reported on blue:

- `switchover_green_errors_total{reason}` and
  `switchover_total{outcome=green|fallback|error|blue}` (default
  Prometheus sink only);
- a log line, at most one per `LogEvery`;
- `GET /admin/switchover`: the setting this node applies, its outcome
  and reason counts, and the last `Keep` errors with reason, outcome,
  operation name and error text. Counts are per node.

An SSE or `multipart/mixed` response streams to the client as it
comes. A failure after its headers went to the client cannot fall
back; it is counted as `transport` with outcome `green`.

### WebSocket subscriptions

The rule is applied when a connection opens. A connection that goes to
green is proxied for its whole life; a percent change does not move
it, and one that stays on blue stays there. Only new connections
follow the new share. A dial error or an error status from green
falls back to blue (nothing ran on green). Blue's `Drain` ends the
proxied connections too.

### What blue and green see

- A forwarded request is not in blue's request metrics, request log or
  replay recording: green serves it. `switchover_total` counts it on
  blue. A request that fell back is served, measured and may be
  recorded like any other.
- Green sees blue as the source address. Blue sets
  `X-Forwarded-For` (appended), `X-Forwarded-Host`,
  `X-Forwarded-Proto` and `X-Gwag-Switchover: 1`. Green's
  `WithWSLimit` per-IP limit counts every forwarded WebSocket as
  blue's address.
- Each forwarded request costs one more HTTP hop.

### Runbook

1. Green runs with the same services registered and the state copied
   (state copy: not built yet). Replay verification shows no
   unexpected diffs.
2. Blue nodes run with `WithSwitchover` (or `gwag up --switchover`)
   and green's `Target`, share 0.
3. `gwag cluster switchover --percent 5`. Watch
   `switchover_green_errors_total` on blue and green's own metrics.
4. `--percent 50`, then `--percent 100`. Clients already on green stay
   there at each step.
5. If green fails: `--percent 0` (or `--clear`) sends every new
   request back to blue at once. Open WebSocket connections to green
   stay until they close.
6. At 100, switch the load balancer to green and drain blue. This
   step is not built yet (it is the next part of the cutover).

### Limits

- Blue reads the whole request body before it decides (up to
  `MaxBytes`), and the whole of green's response before it writes it
  (except SSE and `multipart/mixed`).
- Green's response to a forwarded request is not compared with blue's.
  Use replay verification for that.
- A WebSocket connection to an `https` `Target` is not tested.

Tested in `gw/switchover_test.go`: the bucket is stable per key,
monotone in the percent, and puts about half of 2000 keys on green at
50; rule order, `Exclude`, header values and the WebSocket case; the
mutation check; each failure reason with a query (falls back) and a
mutation (falls back only for `connect`, else 502); green's GraphQL
errors and 4xx passed through; requests that stay on blue; two
gateways at 0, 50 and 100 percent by caller id, sticky header and
source address, client headers reaching green, green down (blue
answers; metric, admin view); the admin ops and their roles; a
WebSocket to green and its fallback; the setting moving between two
cluster nodes through the KV bucket. `gwag cluster switchover` in
`gw/cmd/gwag`. Not tested: a real two-cluster deployment, load, and
the added latency.

## Next

The state copy and the final switch at the load balancer are not
built.
