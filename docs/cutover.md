# Cluster cutover

A cutover moves client traffic from a running cluster (blue) to a
separate one (green). Green can run a new version, or the same version
with other transforms or routing. This is not the replica swap in
[`cluster.md`](./cluster.md#replica-swap), where new nodes join the
same cluster.

What exists now: **replay verification**. Blue records a sample of its
requests with every upstream call they made; green runs each recorded
request through its own pipeline, answers every upstream call from the
recording, and reports what differs. Traffic shifting, the state copy
and the final switch are not built yet.

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

## Next

The rest of the cutover (traffic shift from blue to green, state copy,
the final switch at the load balancer) is not built.
