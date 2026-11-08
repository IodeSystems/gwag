# Operations: health, drain, backpressure, metrics

## Health & graceful drain

`gw.HealthHandler()` mounts a `/health` endpoint that returns:

```json
{"status":"serving","active_streams":0,"node_id":"NA..."}
```

with HTTP 200 normally. It returns HTTP 503, same body shape, in three
states:

- `"status":"starting"`: a cluster node (`WithCluster` plus
  `ControlPlane()`) that has not yet replayed the service registry
  from JetStream KV. The reconciler's KV watch delivers every existing
  value, then an end-of-initial-values marker; the node turns ready
  when it has applied every value before that marker and rebuilt the
  schema. The reconciler also checks the replay against the key count
  the registry stream's leader reports, and watches again if the
  replay came up short (a rejoining node can get a marker before any
  value). A gateway without a cluster is ready at once. A node that
  cannot reach JetStream stays `starting` and logs
  `cluster tracking pending` on each retry; it does not become ready
  after a timeout, because its schema would miss the cluster's
  services.
- `"status":"draining"`: `gw.Drain(ctx)` has been called.
- `"status":"incompatible"`: a cluster node whose binary cannot run at
  the cluster's feature level; the body's `"error"` names both levels.
  It stays so until it runs a binary that can
  ([`cluster.md`](./cluster.md#feature-levels)).

Wire `/health` to your load balancer's health check. It is the only
endpoint: it answers both "is the process up" and "may it take
traffic". On Kubernetes use it as the readiness probe; a liveness
probe on it must allow for the `starting` window (use a
`startupProbe`, or a TCP liveness probe on the HTTP port).

`gw.Drain(ctx)` performs the rolling-deploy preamble:

1. `/health` flips to 503 — LB pulls this node out within its check
   interval (typically 5-30 s).
2. New WebSocket upgrades return 503.
3. Existing WebSocket connections have their context cancelled —
   graphql-go emits `complete` per active subscription, then close.
4. Drain waits for `streams_inflight_total` to reach 0 or `ctx`
   to expire.
5. After Drain returns, run your gRPC/`Cluster.Close()` teardown.

`Drain` does not wait for unary HTTP requests. `gateway.ServeHTTP`
does, and every long-running mode uses it: `gwag` (default mode),
`gwag up`, `gwag serve` (both the full-gateway and the lite
`--openapi` / `--proto` paths) and the example gateway. On SIGTERM or
SIGINT it runs, in order:

1. `Drain` (full-gateway modes; the lite `serve` path has no gateway
   and no `/health`). `/health` goes 503, WebSockets are cancelled
   and waited on. WebSockets are hijacked connections, so
   `http.Server.Shutdown` cannot see them; this step ends them.
2. Sleep `--shutdown-delay` (default `5s`). The node keeps serving
   while the LB sees the 503.
3. `http.Server.Shutdown`: close the listener, wait for in-flight
   requests (GraphQL, REST, Connect, MCP, SSE). Bounded by
   `--shutdown-timeout` (default 30 s); after that the remaining
   connections are closed.
4. gRPC `GracefulStop`, then `Cluster.Close()`.

A second signal exits at once.

The default is `5s`. Set `--shutdown-delay=0` for an interactive run,
or when no load balancer is in front of the node; an interactive Ctrl-C
then does not wait. Behind a load balancer, set it to at least the
LB's probe interval times its unhealthy threshold. With a shorter
delay the listener closes before the LB has marked the node down, and
requests the LB still sends fail with connection refused. Example:
`--shutdown-delay 10s` with a 5 s probe. Set a Kubernetes
`terminationGracePeriodSeconds` above delay + timeout. In Kubernetes
a `preStop` sleep is an alternative.

`--shutdown-timeout` bounds the stream drain and, separately, the
request wait, so the worst case is about delay + 2 x timeout. An
open SSE response counts as an in-flight request: it holds `Shutdown`
until the client leaves or the timeout closes it.

`gateway.NewHTTPServer` sets `ReadHeaderTimeout` 10 s (Slowloris) and
`IdleTimeout` 120 s (above common 60 s LB idle timeouts). It sets no
`ReadTimeout` and no `WriteTimeout`: either would cut slow uploads,
SSE responses and subscriptions. Bound unary latency with
`--upstream-timeout`.

The example wires `--shutdown-timeout` to 30 s by default, so a
`kubectl delete` or `docker stop` triggers the right behaviour
automatically.

## Registrations when a cluster node dies

A control-plane registration lives on the node that accepted it. That
node re-puts its registry KV entries on each heartbeat; the entries
expire 30 s after the last one. The registry and peers buckets have
JetStream subject delete markers on (`LimitMarkerTTL`), so an expiry
reaches every node's watch as a delete: every node drops the
instance, its start-gate state and its drain cover, and a node that
replays the registry later agrees with the nodes that were watching.
A node also looks up each registry key it holds every 15 s, and drops
a key missing for 30 s, for the case where its watch was stalled past
the marker's own 30 s life (a partition, a watch consumer on the dead
node). Buckets made by an older gateway get markers turned on in
place at boot.

Give each service every control-plane address of the cluster
(`controlclient.Options.GatewayAddrs`), or one LB address in front of
them. When a heartbeat cannot reach its gateway, the client registers
again on the next address, with a new registration ID; the old
entries expire. No node stops routing the service, as long as the
client moves within the 30 s TTL: the default heartbeat (10 s,
bounded to 5 s per call) does. A service with one address and a dead
gateway is dropped by every node 30 s after the last refresh, and
registers again when its heartbeat reaches the restarted gateway.

After a node dies, a node whose registry watch ran on it sees new
registry writes 10-20 s late (measured in the gw tests: the watch
consumer is recreated). It keeps routing the old entries meanwhile,
and the watch delivers the client's new registration before the old
entry's delete marker, so routing has no gap.

`/api/admin/peers` drops a dead peer when its peers entry expires
(30 s TTL); the drain status's live-peer set follows the same delete.

## Cluster KV replicas

The cluster keeps its state in JetStream KV buckets (peers, registry,
stable, deprecated, mcp config, drain, external dependents, app keys,
notifications, `gwag-cluster` (the feature level, see
[`cluster.md`](./cluster.md#feature-levels)), and `gwag-schemas` with
`--schema-registry`). They start
at one replica. As peers join, each node raises every bucket to the
count the replica policy gives for the live peers. Set the policy with
`--cluster-replicas` (`gwag up`, example gateway) or
`gateway.WithClusterReplicas`:

| Policy | Replicas for N live peers | Use |
|---|---|---|
| `odd` (default) | largest odd number <= min(N, 3): 1, 1, 3, 3, ... | Never runs a 2-replica group. |
| `auto` | min(N, 3): 1, 2, 3, 3, ... | The behaviour before `odd`. |
| `1`..`5` | that number, capped at N | Fixed. |

Why `odd`: a 2-replica group has no quorum when either member dies. A
cluster forming through R2 and losing a node in that window stalled
the registry and peers buckets: `/api/admin/peers` answered 500 until
the node returned, and registrations that expired meanwhile were not
replaced. Odd counts skip R2. An invalid value fails at boot.

Consequences:

- 2 nodes run at R1. The buckets live on one node. If it dies, writes
  (registrations, heartbeats, peer refresh) stop until it returns, and
  if its disk is lost the buckets' state is lost. Run 3 nodes for fault
  tolerance, or pick `auto` and accept the R2 quorum risk.
- 3 nodes run at R3 and survive one node loss. The move from R1 to R3
  takes a few seconds to tens of seconds. A node killed during it can
  still leave a bucket without a quorum. Wait for the buckets to settle
  (`WaitStreamsReplicated` in the e2e harness) before planned kills.
- The count never drops when a peer is lost, so a brief loss does not
  flap. A peer only leaves the live set after its peers entry expired
  (30 s). `ForgetPeer` is the only path that lowers the count; it
  applies the same policy to the remaining peers. A policy change at
  restart raises but does not lower existing buckets.

## Schema Registry

`gwag up --schema-registry` serves the Confluent Schema Registry API at
`/schema-registry` (read-only by default; state in the `gwag-schemas`
bucket). `gwag schema import` copies an existing registry in, keeping
the schema ids. See [schema-registry.md](./schema-registry.md).

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

`gw.MetricsHandler()` serves every series below. Names are shown
without the prefix: the default is `go_api_gateway_`, and
`WithMetricPrefix` changes it. Every series needs the default
Prometheus sink: `WithoutMetrics()` records and allocates none of them,
and a sink passed to `WithMetrics(custom)` receives only what the
`Metrics` interface carries (dispatch, dwell, backoff, depth, streams,
auth, ws-rejected, sub-fanout, `RecordRequest`); the other series
(marked "default sink only") are not recorded for it.

### Inbound requests

Ingresses: `graphql` (`Handler`, HTTP), `graphql_ws` (the same path,
WebSocket upgrade), `http` (`IngressHandler`, REST/OpenAPI shapes),
`grpc` (`GRPCUnknownHandler`), `connect` (`ConnectHandler`: Connect,
gRPC-Web, gRPC over HTTP), `mcp` (`MCPHandler`).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `request_duration_seconds` | histogram | `ingress`, `code`, `operation_type` | Wall-clock per request. `_count` is the request total. Not recorded for open-ended calls: subscriptions, SSE, MCP GET, WebSocket connections. |
| `request_self_seconds` | histogram | `ingress` | Gateway self-time: total minus the sum of dispatch durations. Recorded for `graphql`, `http`, `grpc`, `connect`; `mcp` has no dispatch accounting. |
| `requests_in_progress` | gauge | `ingress` | Requests being served now (`graphql_ws`: open connections). Default sink only. |
| `request_size_bytes` | histogram | `ingress` | Request body bytes read (the declared `Content-Length` when larger, so a 413 shows the size that was refused). `grpc`: protobuf message bytes. Counted as bytes flow; nothing is buffered. Not recorded for `graphql_ws`. Default sink only. |
| `response_size_bytes` | histogram | `ingress` | Response bytes written. `grpc`: protobuf message bytes. Not recorded for `graphql_ws`. Default sink only. |
| `requests_rejected_total` | counter | `ingress`, `reason` | Requests refused before execution. Default sink only. See reasons below. |
| `graphql_errors_total` | counter | `kind` | GraphQL operations whose response carried errors: `parse`, `validate`, `execution`. Request-limit refusals are counted under `requests_rejected_total`, not here. A batch counts per operation; a WebSocket subscription counts per event. Default sink only. |
| `graphql_partial_responses_total` | counter | none | Responses that carried both `data` (not null) and `errors`. Default sink only. |
| `ws_rejected_total` | counter | `reason` | WebSocket upgrades refused by `WithWSLimit`: `max_per_ip`, `rate_limit`. |

`code` label rule, one for every ingress: the HTTP status number as sent
(`"200"`, `"400"`, `"413"`, `"500"`), except for a gRPC-protocol
response, where it is the gRPC code name (`"OK"`, `"NotFound"`,
`"Unavailable"`). The `grpc` ingress always uses gRPC code names;
`connect` uses the status of the gRPC-protocol response when it carries
one (`grpc-status`), else the HTTP status. A GraphQL response is `200`
whatever the query did; read `graphql_errors_total` for its errors.
Connect streaming errors travel in the stream and show as `200`. A
client that disconnects early is not given a separate code.

`operation_type` is `query`, `mutation` or `subscription` for GraphQL
requests that planned, and empty for everything else, for GraphQL
requests refused or failed before planning, and for a batch it is the
type of the first operation that planned.

`requests_rejected_total` reasons:

| `reason` | Ingress | Cause |
|---|---|---|
| `max_request_bytes` | `graphql` | Body over `WithMaxRequestBytes` (413). |
| `malformed_body` | `graphql`, `http` | Unreadable JSON, bad `variables`/`extensions`, empty batch; REST: path, query or body did not decode (400). |
| `missing_query` | `graphql` | No query and no document id (400). |
| `max_operations` | `graphql` | Document or batch over `WithMaxOperations`. |
| `max_depth` | `graphql` | Document over `WithMaxDepth`. |
| `introspection_disabled` | `graphql` | `__schema` / `__type` under `WithoutIntrospection`. |
| `trusted_required` | `graphql` | Query text under enforced `WithTrustedDocuments` (400). |
| `trusted_unknown` | `graphql` | Document id not in the manifest (`PERSISTED_QUERY_NOT_FOUND`, 200). |
| `persisted_unsupported` | `graphql` | Document id sent, trusted documents not configured (400). |
| `mutation_over_get` | `graphql` | Mutation sent with GET (405). |
| `max_per_ip`, `rate_limit` | `graphql_ws` | `WithWSLimit` (also in `ws_rejected_total`). |
| `draining` | `http`, `grpc`, `connect`, `graphql_ws` | The gateway is draining. |

Admin auth denials are already counted in `admin_auth_total`
(`outcome=denied_*`); there is no second series.

### Upstream dispatch

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `dispatch_duration_seconds` | histogram | `namespace`, `version`, `method`, `code`, `caller` | One upstream dispatch. `code`: `ok` or the gRPC code name (`Internal`, `DeadlineExceeded`), the same for every upstream kind. A `Reject` code uses the gRPC name of the same meaning (`Unauthenticated`, `PermissionDenied`, `ResourceExhausted`, `InvalidArgument`, `NotFound`, `Internal`). `caller` comes from `WithCallerIDExtractor` / `WithCallerHeaders`, default `unknown`, capped by `WithCallerIDMetricsTopK` (overflow is `__other__`). |
| `pool_queue_dwell_seconds` | histogram | `namespace`, `version`, `method`, `kind` | Time waiting for an in-flight slot. `kind`: `unary`, `stream`, `unary_instance`. |
| `pool_backoff_total` | counter | `namespace`, `version`, `method`, `kind`, `reason` | Dispatches refused by backpressure (`reason=wait_timeout`). |
| `pool_queue_depth` | gauge | `namespace`, `version`, `kind` | Dispatches waiting for a pool slot. |
| `replica_queue_depth` | gauge | `namespace`, `version`, `kind`, `replica` | Dispatches waiting for a per-replica slot (`MaxConcurrencyPerInstance`). |
| `pool_streams_inflight` | gauge | `namespace`, `version` | Active subscription streams in a pool. Open `ps_sub` pub/sub subscriptions count under `namespace="ps"`, `version="v1"` (the gateway's own slot; it has no upstream pool). |
| `streams_inflight_total` | gauge | none | Active subscription streams, gateway-wide: proto and OpenAPI upstream streams plus open `ps_sub` subscriptions. Same value as `active_streams` in `/api/health`; Drain waits for it to reach 0. Downstream-GraphQL fanouts are counted by `graphql_sub_fanouts_active`, not here. |
| `graphql_sub_fanout_total` | counter | `namespace`, `event` | Downstream-GraphQL subscription fanout `open` / `close`. |
| `graphql_sub_fanouts_active` | gauge | `namespace` | Active downstream-GraphQL fanouts. |
| `upstream_retries_total` | counter | `namespace`, `version`, `reason` | A call re-sent to another replica because the first attempt provably never reached an upstream (see "Upstream timeouts, retries, ejection"). `reason`: `unavailable` (gRPC, no transport stream), `no_connection` (HTTP, no connection). Default sink only. |
| `upstream_timeouts_total` | counter | `namespace`, `version` | Calls that ran past the upstream timeout. gRPC pools also show `DeadlineExceeded` in dispatch `code`, but OpenAPI and GraphQL sources report `Internal`, so this series is the one to alert on. Default sink only. |
| `replica_ejections_total` | counter | `namespace`, `version`, `replica` | Replica ejections: the failure limit was reached, or a readmitted replica failed again. Default sink only. |
| `replicas_ejected` | gauge | `namespace`, `version` | Replicas ejected now (scrape time). Default sink only. |

### Registry and cluster

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `schema_rebuilds_total` | counter | `result` | Gateway-wide schema rebuilds, `ok` or `error`. Default sink only. |
| `schema_rebuild_duration_seconds` | histogram | none | Duration of those rebuilds. Default sink only. |
| `registrations_total` | counter | `kind`, `outcome` | Registrations seen by this node. `kind`: `proto`, `openapi`, `graphql`, `mcp`, `internalproto`. `outcome`: `created`, `joined` (another replica of an identical schema), `swapped` (`unstable` slot replaced), `rejected`. Includes the gateway's own pub/sub slot at boot. Default sink only. |
| `registered_services` | gauge | `kind` | Registered services on this node, internal slots excluded (scrape time). Default sink only. |
| `cluster_peers` | gauge | none | This node plus the distinct remote cluster nodes it has a live NATS route to; 0 when not clustered (scrape time). A killed peer drops within seconds; a restarted node (new node id) replaces its old id. Read from NATS route state, not the peers KV bucket, so `/api/admin/peers` can still list a dead peer for up to its 30s TTL after the gauge drops. Default sink only. |
| `cluster_feature_level` | gauge | `kind` | Cluster nodes only. `kind=active`: the cluster's feature level as this node last read it; `min`, `max`: the levels this binary can run at. See [`cluster.md`](./cluster.md#feature-levels). Default sink only. |
| `build_info` | gauge | `version`, `go_version` | Always 1. `version` is the module version from the build, `devel` when unknown. Default sink only. |

The scrape-time gauges take the gateway's registry lock briefly; a
scrape during a long schema rebuild waits for it.

### Service lifecycle

Start gate, drain, dependencies. All are per-node views, like usage;
gauges are computed at scrape time, so a series disappears with its
instance or line. Default sink only.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `service_instances` | gauge | `namespace`, `version`, `state` | Instances by start-gate state: `ready`, `waiting`, `degraded`. |
| `service_drain_instances` | gauge | `namespace`, `version`, `drain_state` | Instances by drain state: `draining`, `blocked`, `safe_to_stop`. |
| `start_gate_transitions_total` | counter | `namespace`, `version`, `from`, `to` | State changes (`none`, `waiting`, `ready`, `degraded`, `removed`). READY/DEGRADED flips are sampled every 2s and at scrape, so a shorter flip can go uncounted. |
| `dependency_unmet` | gauge | `consumer_kind`, `consumer_namespace`, `consumer_version`, `provider`, `channel` | 1 when no accepted line of the provider has a ready instance. `channel`: the channel key of a subscription edge (`provider` is empty while no line declares it; see [lifecycle.md](./lifecycle.md#channel-subscriptions)), empty for `depends` edges. `consumer_kind`: `service` (a registered line) or `external` (a declared external dependent: `consumer_namespace` is its id, `consumer_version` is empty; see [lifecycle.md](./lifecycle.md#external-dependents)). Review-due declarations keep their series. |
| `line_sunsetable` | gauge | `namespace`, `version` | 1 when the line is sunset-able (admin dependency graph rule, default 1h usage window). |
| `drain_requests_total` | counter | `outcome` | Drain requests: `requested`, `already_requested`, `cancelled`, `unknown_instance`, `error`. |
| `declarations_review_due` | gauge | — | External dependents past their review date (or with none). They still count as dependents. Cluster-wide set, read on this node. See [lifecycle.md](./lifecycle.md#review-date). |
| `declarations_hook_failures_total` | counter | — | Declarations hook deliveries this node gave up: retries ran out, a 4xx other than 408/429, or a full queue. See [lifecycle.md](./lifecycle.md#declaration-sinks). |

### Auth, pub/sub and caches

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `admin_auth_total` | counter | `method`, `outcome`, `role` | Admin auth checks: `ok_delegate`, `ok_bearer`, `denied_delegate`, `denied_bearer`, `forbidden_role` (valid token, role too low for the route), `no_token_configured`. `role`: `admin`, `dev` or `anonymous`. A read without a bearer is not counted. |
| `app_key_auth_total` | counter | `op`, `outcome` | App-key checks. `op`: `register`, `heartbeat`, `deregister`, `drain_instance`, `schema_registry_write`, or an operator RPC (`forget_peer`, `retract_stable`, `deprecate`, `undeprecate`). `outcome`: `ok`, `ok_admin`, `missing`, `invalid`, `expired`, `denied_delegate`, `namespace_forbidden`, `unavailable`, `admin_required`. Control-plane checks run only with `--require-app-keys`. Default sink only. |
| `app_key_expiry_seconds` | gauge | `key_id` | Seconds until each key the gateway minted expires; negative once expired. Cluster-wide set, read at scrape. Default sink only. |
| `app_keys_expiring` | gauge | none | Minted keys that expire within 7 days and have not expired. Default sink only. |
| `sign_auth_total` | counter | `code` | `SignSubscriptionToken` bearer checks: `in_process`, `ok_signer`, `ok_bearer`, `denied_bearer`, `missing_bearer`, `no_token_configured`. |
| `pub_no_binding_total` | counter | none | Publishes to a channel with no channel binding (empty `payload_type`). |
| `pubsub_bus_degraded` | gauge | none | 1 while `ps.pub`/`ps.sub` run local-only because the cluster NATS connection is down. |
| `pubsub_typed_skipped_total` | counter | `reason` | Typed events (`Gwag-Type` header) `ps_sub` dropped. `reason`: `unknown_type` (no registered proto declares the type) or `decode` (the body is not that type). |
| `plan_cache_hits_total` | counter | none | Plan-cache lookups that skipped parse, validate and plan. |
| `plan_cache_misses_total` | counter | none | Lookups that paid for them; a rising rate means the working set exceeds `WithDocCacheSize`. |

Request-size buckets run from 64 B to 256 MiB (x4). Latency histograms
use the Prometheus default buckets.

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

## gRPC, Connect and gRPC-Web

`gw.GRPCUnknownHandler()` serves every registered service as gRPC on a
grpc-go server. `gw.ConnectHandler()` (experimental) serves the same
routes over Connect, gRPC-Web and gRPC on a plain `http.Handler`, at
`/package.Service/Method`; browsers reach the gateway this way:

```go
mux.Handle("/api/connect/", http.StripPrefix("/api/connect", gw.ConnectHandler()))
```

The `gwag` binary mounts it at `/api/connect/` in `gwag up`, the plain
serve path, and `gwag serve --graphql` / `--mcp`. There is no flag to
disable it. The lite `gwag serve --openapi` / `--proto` path (no
gateway) does not serve it.

Both run on gat's gRPC ingress and behave the same: `Unavailable` while
draining, per-pool backpressure on unary proto calls, the
`gateway.grpc` span and request metrics. The subscription auth handles
(`x-gateway-hmac`, `x-gateway-timestamp`, `x-gateway-kid`) are metadata
on gRPC and request headers on Connect and gRPC-Web. An upstream's HTTP
error status picks the gRPC code: 404 NotFound, 409 AlreadyExists, 429
ResourceExhausted, 502/503/504 Unavailable, other 5xx Internal (see
`gat.ErrorCode`). The REST ingress (`gw.IngressHandler()`) answers with
that code's HTTP status (`gat.HTTPStatus`), so an upstream 409 is 409
there too.

## GraphQL error codes

A dispatch error carries gat's extensions shape:
`{"status": 404, "code": "not_found"}`. `code` is the lower-case gRPC
code, the same one the gRPC and REST ingresses use for that error.
`status` is the HTTP status. An upstream 409 is `already_exists`; an
upstream 502 is `unavailable`. `retryAfterSeconds` is kept. Errors that
carry their own extensions (downstream GraphQL errors,
`subscribeAuthCode`) are left alone.

Before v2.11.0 a `Reject` error carried its upper-case `Code` name
(`{"code": "NOT_FOUND"}`), other dispatch errors carried no code, and
an upstream 409 was `INVALID_ARGUMENT`. `gateway.WithLegacyErrorCodes()`
restores that output. It is for migration and is removed in v3.
`WithConnectErrorCodes()` is deprecated and does nothing.

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
| `WithMaxOperations(n)` | 64 | A document selecting more than `n` operations, aliases and fragment spreads included; a batch (JSON array body) of more than `n` documents, or whose documents select more than `n` operations together (the whole batch is refused). |
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
