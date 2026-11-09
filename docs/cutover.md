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
- **State copy.** Blue's operator-set state (stable and deprecated
  marks, app and system keys, external dependents, Schema Registry
  contents) goes to green in one document, under a freeze of blue's
  admin writes.
- **Event stream mirror.** Green's `EV_*` JetStream streams source
  blue's over a NATS leafnode connection that a system key
  authorizes, until the switch.
- **System keys**, the credential one cluster presents to the other.

The final switch at the load balancer is not built yet.

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
| `Token` | none | bearer sent to green: a system key of green's (`gws_...`), or the admin role |
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

Use a system key as `Token` ([System keys](#system-keys)): it is
valid on every node of green, and it can call nothing but verify and
the state copy. The admin token also works, but it is per node unless
pinned (`WithAdminToken`) and it can call every admin route.

### Configure green

```go
gw := gateway.New(gateway.WithReplayVerifyEndpoint(), /* ... */)
adminMux, _, _ := gw.AdminHumaRouter()
mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))
```

`POST /admin/replay/verify` takes a `ReplayExchange` JSON body (up to
64 MiB) and returns a `ReplayResult`. It needs a system key or the
admin role: it runs a request as whatever client the recording names,
and its result holds the upstream requests green built, injected
header values included.
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
   ([State copy](#state-copy)). Replay verification shows no
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

Events: start the [event stream mirror](#event-stream-mirror) before
step 3, and stop it after step 6. While requests go to green, events
published through green stay on green; the mirror runs one way.

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

## System keys

A system key is the credential of one cluster calling another. Mint
it on the cluster that will be called, with the admin token:

```
$ gwag key create --system --ttl 30d --note "blue -> green" --endpoint http://green:8080
gws_8d2f61c04a7be913_51c0...
system key id 8d2f61c04a7be913, expires 2026-12-08T10:00:00Z. The key is shown once; store it now.
```

The admin route is `POST /admin/system-keys` (`{ttl, note}`). A
system key is stored like an app key (its SHA-256, an expiry, in the
`gwag-app-keys` bucket with `"kind": "system"`), so it is valid on
every node of the cluster at once, and `gwag key list` shows it
(`KIND system`), `gwag key revoke ID` revokes it, and the expiry log
lines and `app_key_expiry_seconds{key_id}` cover it. Rotate it like an
app key: mint a new one, deploy it, revoke the old one.

It authorizes these admin routes and the mirror connection, and
nothing else:

| Route | |
|---|---|
| `POST /admin/replay/verify` | replay verification |
| `GET /admin/cluster/state` (`exportClusterState`) | state export |
| `POST /admin/cluster/state` (`importClusterState`) | state import |
| `GET /admin/cluster/mirror` (`getClusterMirror`) | mirror status |
| `POST /admin/cluster/mirror` (`startClusterMirror`) | mirror start |
| `POST /admin/cluster/mirror/stop` (`stopClusterMirror`) | mirror stop |
| NATS leafnode to `--mirror-listen` | the [event stream mirror](#event-stream-mirror) connection |

Every other admin route answers a valid system key with 403; the
control plane, the Schema Registry and the `admin` GraphQL fields
refuse it; the `AdminAuthorizer` and `AppKeyAuthorizer` delegates are
never sent one. An app key is refused on these routes and on the
mirror listener. The admin role may call the routes too (the mirror
status also takes dev).

Why a distinct prefix (`gws_`, app keys are `gwk_`): the middleware
routes the bearer to the right check without a KV read, a leaked key
names its kind, and the record's kind is checked as well, so neither
kind passes for the other.

## State copy

What green needs from blue before it serves, and how it gets there:

| State | Copied by | Note |
|---|---|---|
| stable marks (`gwag-stable`) | `gwag cluster export` / `import` | |
| deprecated marks (`gwag-deprecated`) | export / import | a mark for a line green has not registered yet applies when it registers |
| app keys and system keys (`gwag-app-keys`) | export / import | with the stored hashes: the same keys work on green |
| external dependents (`gwag-external-dependents`) | export / import | |
| Schema Registry (`gwag-schemas`) | export / import | every context, the default one included, with ids and versions kept; the control plane subject is not copied (each gateway registers its own) |
| registrations (`gwag-registry`) | not copied | services register with both clusters, see below |
| MCP include / exclude config, drains, notifications | not copied | set them on green with the admin API |
| event streams (`EV_*`) | not copied | [event stream mirror](#event-stream-mirror) |

### Steps

```
gwag cluster freeze   --gateway http://blue:8080  --bearer $BLUE_ADMIN --ttl 10m
gwag cluster export   --gateway http://blue:8080  --bearer $SYSTEM_KEY > state.json
gwag cluster import   --gateway http://green:8080 --bearer $GREEN_ADMIN --dry-run state.json
gwag cluster import   --gateway http://green:8080 --bearer $GREEN_ADMIN state.json
gwag cluster unfreeze --gateway http://blue:8080  --bearer $BLUE_ADMIN
```

`--gateway` here is the gateway's HTTP endpoint (the admin API at
`/api/admin`), not the control-plane address `gwag cluster finalize`
takes. `--bearer` is the admin token or, for export and import, a
system key. The first import into a fresh green needs green's admin
token (green holds no keys yet); after it, blue's system keys are
valid on green too.

Admin routes: `GET /admin/cluster/state[?allowUnfrozen=true]` returns
the document; `POST /admin/cluster/state` takes
`{state, overwrite, dryRun}` (up to 64 MiB) and returns counts per
section (`added`, `unchanged`, `overwritten`), the Schema Registry
counts, the conflicts and warnings.

### The document

One JSON object, `"v": 1`:

```json
{
  "v": 1,
  "exportedAt": "2026-11-08T10:00:00Z",
  "exportedBy": "system:8d2f61c04a7be913",
  "node": "blue-0",
  "frozen": true,
  "stable": [{"namespace": "greeter", "vN": 2}],
  "deprecated": [{"namespace": "greeter", "version": "v1", "reason": "use v2"}],
  "keys": [{"id": "3f9c0a1b2c3d4e5f", "namespaces": ["orders"], "hash": "…", "created_unix_ms": 0, "expires_unix_ms": 0}],
  "externalDependents": [{"id": "acme", "kind": "customer", "depends": [{"service": "greeter", "lines": ["v1"]}], "updated_unix_ms": 0}],
  "schemaRegistry": {"contexts": [{"name": ".", "schemas": [], "versions": [], "levels": []}]}
}
```

Key records and external dependents keep their stored form. The key
hashes are SHA-256 of the whole key: the document lets nobody call
with a key, but it lists every key id, namespace and expiry. Treat it
as a secret. Export reads every bucket from its stream leader, so a
replica that is behind does not drop a value.

### Freeze

`gwag cluster freeze [--ttl 10m] [--note TEXT]` (`POST
/admin/cluster/freeze`, admin) writes one record, `freeze` in the
`gwag-cluster` bucket, with an end time. Until it ends, or until
`gwag cluster unfreeze` (`POST /admin/cluster/unfreeze`), every node
refuses the writes that change copied state:

| Write | Refused with |
|---|---|
| `retractStable`, `deprecate`, `undeprecate` (admin routes and control-plane RPCs) | 409 / `FailedPrecondition` |
| `createAppKey`, `createSystemKey`, `revokeAppKey` | 409 |
| `putExternalDependent`, `reviewExternalDependent`, `deleteExternalDependent` | 409 |
| Schema Registry REST writes: register, delete, config, mode, `gwag/import` | 409, `error_code` 40903 |

The message names the end time and who froze:
`cluster frozen for cutover until 2026-11-08T10:10:00Z (set by admin): deprecate is refused; gwag cluster unfreeze ends the freeze`.

Not frozen: registrations, heartbeats, drains and client traffic. A
Schema Registry register of a schema the subject already holds is
answered with its id, so a serde that registers on start keeps
working; a new schema is refused. Lookups (`POST /subjects/{s}`) are
reads.

The TTL defaults to 10 minutes and is at most 24 hours; freezing again
replaces the end time. An operator who crashes mid-copy freezes the
cluster for the TTL at most. `GET /admin/cluster/freeze`
(`getClusterFreeze`, dev) shows the freeze in force. Every check reads
the record from the bucket's stream leader, so a freeze set on one
node applies on every node at the next write; when the leader cannot
be asked, the write fails (503 / `Unavailable`) instead of passing
unchecked. A standalone gateway keeps the record in memory. Gateways
older than the freeze do not read the record: in a blue with mixed
versions, those nodes still accept writes.

Export refuses while the cluster is not frozen (409).
`--allow-unfrozen` (`?allowUnfrozen=true`) exports anyway and puts a
warning in the document; freeze and export again before the switch.
Import does not check a freeze.

Writes that a registration causes go on during the freeze: a service
that registers a new `vN` raises its stable mark, and new channels add
Schema Registry subjects in the default context. Services register
with green as well, so green sets those itself.

### Import: idempotent, conflicts

An item green holds with the same value is unchanged; importing the
same document twice changes nothing. An item green holds with another
value is a conflict: a stable mark at another `vN`, a deprecation with
another reason, a key record or an external dependent that differs.
With any conflict nothing is written and the answer (409) lists them.
`--overwrite` takes the document's value for each. `--dry-run` reports
the counts and conflicts and writes nothing.

Schema Registry conflicts cannot be overwritten: a schema id green
allocated to another schema, or a subject version that names another
id. Kafka messages carry the ids, so green must hold blue's. The
import checks every context before it writes. To avoid them, import
before services register channels with green: a fresh green's
registry holds only its own control plane subject (id 1 of the
default context, which blue's copy leaves out). When services register
their channels with green after the import, green finds blue's
schemas and allocates nothing. `gwag schema import --into .` stays
refused: a foreign registry's ids never go into the default context;
only this copy from another gwag does.

### Registrations

Registrations are not copied. Each service runs one registration per
cluster: two `controlclient.SelfRegister` calls (one with blue's
control-plane address, one with green's), or two `register()` calls
with the TypeScript or Python client. `GatewayAddrs` is failover
within one cluster, not registration with several. With
`--require-app-keys` on green, the service's key is valid there once
the state is imported.

Tested in `gw/cluster_state_test.go`: system key scope (accepted on
verify, export and import; 403 on other admin routes; refused by the
control plane and the Schema Registry; an app key refused on verify;
expiry and revoke); freeze on a standalone gateway (every write kind
refused, registrations and heartbeats go on, export refused unfrozen,
expiry and unfreeze); a freeze set on one node of a two-node cluster
refusing every write kind on the other until its TTL; an export from a
two-node cluster and an import into a separate one-node cluster (the
same app key registers a service on green, blue's system key passes
green's verify, Schema Registry ids equal in the default and a named
context and green's own channel registration adds nothing, stable and
deprecated marks equal, a second import changes nothing, the service
registered with both clusters by two controlclient registrations);
import conflicts, dry run, overwrite, and a Schema Registry conflict
that overwrite does not take. `gw/cmd/gwag/cutover_test.go` drives the
CLI. Not tested: a TypeScript or Python service registered with two
clusters, a freeze on a cluster with nodes older than the freeze,
large registries near the 64 MiB body cap.

## Event stream mirror

Until the switch, green's event streams (`EV_*`, one per named
channel) copy blue's. NATS does the copying: green's stream has a
JetStream *source* on blue's stream of the same name, reached over a
NATS leafnode connection from green to blue. A system key of blue
authorizes the connection. gwag configures the connection at boot and
adds or removes the source per stream at runtime.

### How it works

- Blue runs with `--mirror-listen ADDR` (`ClusterOptions.MirrorListen`)
  on every node: a NATS leafnode listener next to the client port.
- Green runs with `--mirror-from ADDR` and a system key minted on
  blue (`--mirror-key-file`, or `GWAG_MIRROR_KEY`;
  `ClusterOptions.MirrorFrom`, `MirrorKey`) on every node. Each green
  server connects to blue as a leafnode. With several blue addresses
  (comma-separated) NATS picks one at random and tries the next when
  it fails.
- `gwag cluster mirror` (on green) adds a source to green's stream of
  each event stream blue has. Green creates a stream it does not have,
  with blue's subject and gwag's event stream config. The source is
  the only mirror state: there is no other record, and the stream
  config shows it.
- A sourced message keeps its subject, payload and headers,
  `Nats-Msg-Id` included; NATS adds `Nats-Stream-Source`. It gets
  green's next sequence, so the sequence numbers of the two sides
  differ. Blue's messages arrive in blue's order. Messages published
  on green itself are stored too and interleave with them.
- Within the 2-minute duplicates window, a message whose
  `Nats-Msg-Id` green already holds is dropped, sourced or published.
- After a restart of green, NATS resumes each source after the last
  blue message green stored, without duplicates.

Only these subjects cross the leafnode. Blue binds the connection to a
user with these permissions, and nothing else gets through: no core
NATS subject, no event published on the other side, no gateway KV
bucket.

| Direction | Subjects | For |
|---|---|---|
| green to blue | `$GWAG.MIRROR.API.>` | the JetStream API: blue maps `$GWAG.MIRROR.API.CONSUMER.CREATE.>` and `.STREAM.LIST` to `$JS.API.…` |
| green to blue | `$JS.FC.>` | flow control replies |
| blue to green | `$GWAG.MIRROR.DELIVER.>` | the sourced messages |
| blue to green | `$JSC.R.>`, `$GWAG.MIRROR.INBOX.>` | API replies |

`CONSUMER.DELETE` is not mapped, so a key holder cannot delete blue's
consumers. The consumer NATS creates on blue for a source
(`JS_SRC_…`) is removed 10 seconds after the source stops
(its inactive threshold).

Auth on blue's listener: a leafnode connection must give a system key
of blue as its password (or token). Blue checks it on connect like any
system key, ends the connection at the key's expiry, and checks live
connections again every 30 seconds: a revoked key loses its
connection, and its reconnects are refused. No key, an app key, or a
wrong key: refused. Use `--mirror-tls-cert`, `--mirror-tls-key` and
`--mirror-tls-ca` (mutual TLS, both sides; `ClusterOptions.MirrorTLS`),
or the key crosses the network in the clear.

The client port does not change. Without `--mirror-listen` the server
has the same config as before. With it, gwag checks client
connections itself, with the same rules: standalone, any client; in a
cluster, no credentials (global account) or gwag's in-process system
user, and nothing else.

### Configure

On blue, every node:

```
gwag up ... --mirror-listen :14223
gwag key create --system --ttl 30d --note "green mirror" --endpoint http://blue:8080 > mirror.key
```

On green, every node (a restart if green is running):

```
gwag up ... --mirror-from blue-0:14223,blue-1:14223,blue-2:14223 --mirror-key-file mirror.key
```

### Start, status, stop

```
$ gwag cluster mirror --gateway http://green:8080 --from blue-0:14223
source=nats-leaf://blue-0:14223,... leafnodes=1
STREAM                                   MIRRORED   BLUE_SEQ  GREEN_SEQ      LAG   ACTIVE ERROR
EV_orders_v1_Audit                       true            120        120        0     215ms
EV_orders_v1_OrderEvents                 true          50211      50211        0     98ms
```

- Start (no flag): every event stream blue has, or each `--channel`
  (a stream name or a channel subject, repeatable). `--from`, when
  given, must be green's configured source. Running it again changes
  nothing.
- `--status`: per event stream on either side, whether green sources
  it, each side's last sequence, and `LAG`, the number of blue
  messages not on green yet as NATS last reported it (the source
  consumer's pending count). `ACTIVE` is the time since blue last
  delivered or sent a heartbeat (every second). `ERROR` is the
  source's replication error. A stream is caught up when `LAG` is 0
  and `ACTIVE` is about a second or less; compare blue's message count
  with what green holds from blue when green has its own messages too.
- `--stop`: every mirrored stream, or each `--channel`. The source is
  removed; the stream and its messages stay as a normal stream.

`--gateway` is green's HTTP endpoint. `--bearer`: green's admin token
or a system key of green (after the state copy, blue's system keys are
green's too); the status also takes the dev token. Admin routes: `GET
/admin/cluster/mirror`, `POST /admin/cluster/mirror` (`{from,
streams}`), `POST /admin/cluster/mirror/stop` (`{streams}`).

Stream config on green: gwag's event stream config (max age
`WithEventStreamMaxAge`, 7 days by default; 2-minute duplicates
window; file storage; limits retention), and replicas from the
cluster replica policy, as for a stream gwag creates for a channel.
When a service registers the channel with green, or the replica count
changes, gwag keeps the source. A stream green already has keeps its
own config; only the source is added.

### Consumers

The mirror copies messages, not consumers. Durable consumers on blue
are not copied, and their positions do not carry over: green's
sequence numbers are its own.

A durable that the gwag event runtime creates on green on the first
subscribe starts with *deliver new*: after green's last message at
that time (`events/jetstream`; `DeliverAll` starts at green's first
message, every mirrored day of it). An existing durable on green keeps
its position: the runtime uses it as it is. So the moment a service's
consumer first subscribes on green sets where it starts.

No gap and no duplicate, per stream:

1. The mirror runs and `LAG` is 0.
2. Stop the producers of the stream (on blue). Events published
   through green (switchover traffic) are producers too.
3. Wait for `LAG` 0 and every consumer on blue at 0 pending
   (`nats consumer info`).
4. `gwag cluster mirror --stop --channel STREAM`.
5. Move the consumers to green. Each starts after green's last
   message: everything up to there was consumed on blue.
6. Move the producers to green.

Without a pause, accept duplicates instead: create each green durable
before step 5 with a start time a little before the moment its blue
consumer stopped (`nats consumer add EV_… GROUP --deliver
2026-11-08T10:00:00Z`; the runtime uses it as it is; a sourced message
has the time green stored it), and drop repeats in the handler by
`Message.ID` (`Nats-Msg-Id`).

### Order with the other steps

1. [State copy](#state-copy): freeze, export, import, unfreeze.
2. Blue nodes with `--mirror-listen`; green nodes with `--mirror-from`
   and a system key of blue.
3. `gwag cluster mirror` start; wait for `LAG` 0.
4. [Replay verification](#replay-verification) and
   [switchover](#switchover) steps. The mirror keeps running. Events
   published through green during the switchover stay on green: the
   consumers on blue do not see them. To keep such mutations on blue
   until the consumers move, exclude them (`--rule op=NAME,exclude`).
5. At the switch: the consumer steps above, per stream, then `gwag
   cluster mirror --stop`.
6. Load balancer to green, drain blue.

### Limits

- One source per green cluster, set at boot (`--mirror-from`).
  Changing it needs a restart of green's nodes. NATS can add a leafnode
  remote at runtime (`ReloadOptions`); gwag does not use it.
- One way, blue to green.
- A stream that start creates is in the replica bookkeeping of the
  node that ran the start, until a service registers its channel with
  green (then every node has it).
- Revoking the key ends the connection within 30 seconds, not at once.
- `LAG` is what NATS last saw; it is not a count taken now.

Tested in `gw/cluster_mirror_test.go` (in-process, nats-server
v2.15.2): a 3-node blue and a 1-node green; messages published on
blue appear on green in order with the same `Nats-Msg-Id` and
subject; the status before start, after start, and caught up (blue
and green last sequence equal, `LAG` 0, `ACTIVE` set); start for one
channel subject leaves the others alone; `--from` that is not the
source and an unknown stream refused; green registering the same
channel keeps the source; a publish on green with a sourced message's
id is a duplicate; a core subject published on either side does not
reach the other, and a green publish does not reach blue's stream;
green restarted while blue receives three more messages resumes with
no duplicate; stop leaves the stream and its config, and later blue
messages do not arrive; a revoked key closes the connection and it
stays closed; no key, an app key, a wrong key, and an app key as token
refused by the listener; the client port takes no credentials and
refuses a system key; `MirrorKey` set to an app key refused at start.
A 1-node blue and a 2-node green (each green node connected): each
message lands once, and a stream the mirror creates gets R2 under the
`auto` policy. TLS: plain TCP and a certificate of another CA refused,
the same CA connects. `gw/cmd/gwag/mirror_test.go`: the CLI start,
status, stop, `--from` refused, a system key of green accepted, an app
key refused, and the `gwag up` flags. Not tested: a blue node restart
or failure while mirroring, a network between the clusters, load and
lag under load, a replica count change while a stream is mirrored,
the consumer procedures above.

## Next

The final switch at the load balancer (with an example) is not built.
