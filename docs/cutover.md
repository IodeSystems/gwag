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
- **State copy.** Blue's operator-set state (stable and deprecated
  marks, app and system keys, external dependents, Schema Registry
  contents) goes to green in one document, under a freeze of blue's
  admin writes.
- **System keys**, the credential one cluster presents to the other.

Traffic shifting and the final switch are not built yet.

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

It authorizes three admin routes, and nothing else:

| Route | |
|---|---|
| `POST /admin/replay/verify` | replay verification |
| `GET /admin/cluster/state` (`exportClusterState`) | state export |
| `POST /admin/cluster/state` (`importClusterState`) | state import |

Every other admin route answers a valid system key with 403; the
control plane, the Schema Registry and the `admin` GraphQL fields
refuse it; the `AdminAuthorizer` and `AppKeyAuthorizer` delegates are
never sent one. An app key is refused on the three routes. The admin
role may call them too.

Why a distinct prefix (`gws_`, app keys are `gwk_`): the middleware
routes the bearer to the right check without a KV read, a leaked key
names its kind, and the record's kind is checked as well, so neither
kind passes for the other.

The NATS event stream mirror (not built) will use the same key.

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
| event streams (`EV_*`) | not copied | NATS stream mirror, not built |

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

## Next

Traffic shift from blue to green and the final switch at the load
balancer are not built.
