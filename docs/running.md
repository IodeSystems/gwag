# Running the gateway

This page is for running gwag itself: how many nodes, how traffic
reaches them, which setups are tested, and what fails when a node, a
service or the load balancer goes down, with measured numbers. Flags
and the 3-node walkthrough are in [`cluster.md`](./cluster.md), health
states and drain in [`operations.md`](./operations.md), the generated
deployment files in [`deploy.md`](./deploy.md).

## Topologies

| Nodes | Use | Survives | Notes |
|---|---|---|---|
| 1 | Development; production that accepts downtime on a restart | nothing | With a data directory (`gwag up --data-dir`, example gateway `--nats-data`) JetStream keeps registrations, stable and deprecated marks, app keys and event streams on disk across a restart ([f](#measured-failures)). Without one, that state is in memory. A 1-node cluster cannot grow into a 3-node one with its data ([`cluster.md`](./cluster.md#single-node-to-three)); start with 3 if you will need 3. |
| 2 | Not recommended | nothing for writes | The JetStream meta group of 2 needs both nodes for a quorum, and the default `odd` replica policy keeps every bucket at R1 on one node. One node down leaves no meta leader ([`cluster.md`](./cluster.md#bootstrap-rule)); if the buckets were on the dead node, they are gone with it. Expected: writes fail and the other node routes from the registry it holds, as in [b](#measured-failures). Not run with 2 nodes. |
| 3 | Production | 1 node | Buckets and event streams at R3. With 2 of 3 down, the survivor keeps serving every known service; writes fail until a second node is back ([b](#measured-failures)). |
| 5 | Production, two failures | 2 nodes for routing; see notes for state | The meta group of 5 keeps a quorum with 2 down. Buckets stay at R3 under `odd` and `auto` (the cap is 3), so losing the 2 nodes that hold 2 replicas of the same bucket stalls writes to it. Use `--cluster-replicas 5` to put every bucket and event stream on all 5. Not tested. |

Run an odd number of nodes. Every node serves every service; more
nodes add capacity and failure tolerance, not partitioning.

## How routing works with several nodes

- **Any node serves every service.** Each node watches the registry
  (JetStream KV) and dials every registered instance itself. A call
  never hops between gateway nodes.
- **Clients go through a load balancer.** Round robin (or least
  connections) over the nodes' HTTP port, health check `GET
  /api/health`, take a node out on anything but 200. Set the drain
  delay (`--shutdown-delay`) to at least probe interval times
  unhealthy threshold ([`operations.md`](./operations.md#health--graceful-drain)).
- **A WebSocket stays on its node.** `ps_sub` and other subscriptions
  live on the node that accepted the socket. When that node goes, the
  socket closes; the client reconnects through the load balancer and
  subscribes again. Events published while it had no subscription are
  lost (`ps_sub` is pub/sub, no replay): 0 to 5 in S14 at about 30
  publishes a second ([`TestS8SubscriptionFailover`](../e2e/s8_subscription_failover_test.go)
  measures the window in detail). Publishes and subscriptions on
  different nodes meet over NATS.
- **Typed events go over NATS, not HTTP.** Producers and listeners
  connect to the nodes' NATS client ports; give them every node's URL
  (`nats://n1:14222,nats://n2:14222,...`) so they move when one dies.
  Streams are replicated, so a listener on another node gets what was
  published before the loss.
- **Services register through the control plane.** Give each service
  every node's control-plane address (`controlclient.Options.GatewayAddrs`)
  or one load-balanced address in front of them (it must pass HTTP/2,
  as gRPC needs). A registration lives on the node that took it; when
  that node stops answering, the client registers on the next address
  ([`operations.md`](./operations.md#registrations-when-a-cluster-node-dies)).
- **Native gRPC ingress shares the control-plane port.** If clients
  call services over native gRPC, that port goes behind a load
  balancer too, and the control plane with it; restrict who may
  register with app keys ([`services.md`](./services.md)).

Ports (defaults, `gwag up`):

| Port | Serves | Who connects |
|---|---|---|
| 8080 | GraphQL, REST, Connect, MCP, admin API and UI, `/api/health`, `/api/metrics` | clients through the LB, operators |
| 50090 | control plane (gRPC) and native gRPC ingress | services registering; gRPC clients |
| 14222 | NATS client | event producers and listeners, `ps` bridge clients |
| 14248 | NATS cluster routes | other gateway nodes only |

## Supported deployment setups

| Setup | How | Tested |
|---|---|---|
| Processes on one host | `gwag up` per node, flags as in [`cluster.md`](./cluster.md#run-3-nodes) | The e2e stories (S4, S8, S11, S13, S14) run 1 and 3 nodes this way, behind an in-test load balancer. The failure table below comes from here. |
| Docker Compose | `gwag deploy up --platform docker-compose [--replicas 3]` | Started, healthy, one node restarted ([`deploy.md`](./deploy.md#docker-compose)). Failure modes not run on Compose. |
| Kubernetes, minikube | `gwag deploy export --platform k8s` / `up --platform minikube` | Manifests validated offline only. |
| ECS (Fargate) | `gwag deploy export --platform ecs` | CloudFormation linted only. |
| Embedded in a Go binary | `gateway.StartCluster` + `gateway.New(gateway.WithCluster(...))` | The `gw` package's cluster tests. |

## Measured failures

[`TestS14FailureModes`](../e2e/s14_failure_modes_test.go) and
`TestS14bStandaloneRestart`, gwag at this commit, nats-server v2.15.2,
every process on one 24-thread x86_64 host. Three nodes behind a round-robin load
balancer that probes `/api/health` every 100 ms. Behind them:
`catalog` (proto, 2 instances), `ledger` (OpenAPI, 1 instance), `orders`
(declares one typed event channel). Traffic, each kind back to back
with a 20 ms pause: GraphQL calls to both services, REST and native
gRPC calls to `catalog`, `ps.pub` with a `ps_sub` client, typed event
publishes and a group listener. Defaults everywhere: 30 s registration
TTL, 10 s heartbeat. Kills are SIGKILL. Numbers are from 4 runs of
the gateway as committed (f: 3 runs), except where a row says 5
(`go test -tags e2e ./e2e/ -run TestS14 -v`, the `S14 |` lines).

| Scenario | Keeps working | Fails | Recovery |
|---|---|---|---|
| **a** 1 of 3 nodes killed, restarted 30 s later | Every call sent to the two other nodes (0 failed in 4 runs). `ps_sub` client reconnects through the LB. Typed events: every acked event delivered. | 0 to 2 calls per traffic kind, sent to the killed node before the LB's probe failed (out of rotation 100 ms after the kill). `ps_sub` on the killed node: 0 to 4 events lost while reconnecting. Event publishes: 0 failed in 3 runs, 12 failed in 1 run (+2.0 to +7.7 s, the stream's leader was on the killed node). Registrations held by the killed node move to a survivor; its old entries expire 30 s after the kill. | Restarted node back in LB rotation 0.3 to 3.5 s after start; 0 failed calls on it afterwards. |
| **b** 2 of 3 nodes killed, restarted 51 to 64 s later | The survivor serves GraphQL, REST and native gRPC to every known service: 0 failed calls on it in 4 runs, for the whole outage, past the 30 s registration TTL. `ps.pub` and `ps_sub` on the survivor (core NATS). Admin reads. `/api/health` 200 `serving`. | 1 or 2 calls in flight on the killed nodes. Typed event publishes, from +2.0 s on (107 to 122 per run). Registrations: a new service cannot register (`kv put: context deadline exceeded`). Admin writes: deprecate answered 503 (`cutover freeze: cannot read the freeze record`) or 500 (`persist deprecate to KV: context deadline exceeded`). Heartbeats answer ok but do not refresh the registry. | Restarted nodes serve 3.4 to 8.8 s after start. Survivor reports `jetstream: ok` 4.5 to 14.5 s after the restart; the waiting service registered at the same time. Event publishes again 0.5 to 8.5 s after the restart; acked events delivered, the backlog within 13 s. In 1 of 5 runs (the first, on a build before the fix; the expiry does not depend on it) a service whose registration was held by a killed node (`ledger`) was missing for 6 s after the restart: its entry expired as soon as the stream had a leader again, before the client registered anew (83 failed calls, `Cannot query field "ledger"`). |
| **c** LB down | Not run. Nodes, services and NATS traffic between them are unaffected. | Every client call. | When the LB is back, or at once with a redundant one ([below](#single-points-of-failure)). |
| **d** The only instance of `ledger` killed, restarted after its registration expired | Every other service (0 failed calls). | Every `ledger` call. While registered (27.6 to 29.9 s): `openapi: POST http://.../ledger/v1/echo: ... connect: connection refused`. After the expiry: `Cannot query field "ledger" on type "Mutation"`. | Answers 31 to 44 ms after the instance's restart (it registers again at start). |
| **e** 1 of 2 `catalog` instances killed, restarted after its registration expired | `catalog` on the other instance; every other service. | 0 to 3 calls per run (4 runs), all in flight on the instance at the kill (`connection reset by peer`). | No gap; the restarted instance takes calls when it registers. |
| **f** Standalone node (JetStream on disk) killed, restarted at once | Nothing while it is down. After the restart: the registry replayed from disk routes the service before it heartbeats; the deprecation set before the kill is still there. | Every call until the restart (20 to 24 calls in the 1.1 s the test left it down). | First ok call 115 to 154 ms after start, `/api/health` 200 at 202 ms. The service registered again at its next heartbeat (9.2 s); the old entry expired 29.5 s after the restart. No failed call after the first ok one. |

What the numbers mean for clients:

- A call in flight on a node that is killed fails, and the load
  balancer sends a few more before its probe fails (100 ms here; with
  a 5 s probe, expect up to 5 s of calls to that node to fail). Retry
  idempotent calls on another node.
- An instance that dies fails only the calls in flight on it, when a
  second instance exists: a call refused at connect is retried on the
  other instance, and after 5 failures the dead one is ejected for 10 s
  ([`operations.md`](./operations.md#upstream-timeouts-retries-ejection)).
  With one instance, every call fails, first with the connect error,
  then, once the registration expired (30 s), with `Cannot query field
  "ledger"`: the service is gone from the schema.
- Without a quorum the survivor serves reads; it does not take
  registrations, refresh heartbeats in KV, admin writes or event
  publishes until a second node is back.

### Health without a quorum

A cluster node that has replayed the registry reports in its
`/api/health` body `"jetstream": "ok"`, or `"jetstream": "unavailable"`
with `"degraded": true` when its last peer-entry refresh (every 10 s,
5 s timeout) failed. The status stays 200 `"serving"`; the other
fields (`active_streams`, `node_id`, `cluster_level`, `pubsub`) are
unchanged. The node logs `cluster: JetStream takes no writes` when it
turns and `cluster: JetStream takes writes again` when it is back.

Why 200: the node still routes every service in the registry it holds
(S14 b: no failed call on it). The nodes without a quorum are all in
the same state, so a load balancer that pulled them would turn a write
outage into a full outage. Alert on `"jetstream": "unavailable"`
instead. Check for `status == "serving"` or the HTTP code, not for the
absence of `degraded`. Measured: the field turned `unavailable` 6.0 to
8.8 s after the kills and `ok` again 4.5 to 14.5 s after the restarts.

The cost: a node cut off from the others by a network partition, rather
than left alone by failures, also serves its last known registry, which
can be stale (instances removed on the other side are still called).
Not tested.

## Single points of failure

| Component | Failure | How to remove it |
|---|---|---|
| Load balancer (c) | No client reaches any node; the nodes, services and events between them go on | A redundant pair (VRRP, a cloud LB), or DNS with one record per node and clients that retry the next address. Not run in S14: the in-test balancer is the client. |
| A single gateway node | Everything stops until it is back ([f](#measured-failures)) | 3 nodes. |
| JetStream quorum | Writes stop: registrations, heartbeat refresh, admin writes, KV, events ([b](#measured-failures)) | 3 or 5 nodes on separate hosts and disks; replace a dead node rather than waiting, and `gwag peer forget` one that will not return ([`cluster.md`](./cluster.md#forget-a-dead-peer)). |
| A service with one instance | Its calls fail until it is back ([d](#measured-failures)) | Two or more instances ([e](#measured-failures)). |
| A service's one control-plane address | When that node dies, the service cannot refresh its registration and is dropped 30 s later ([`cluster.md`](./cluster.md#when-a-node-dies)) | `GatewayAddrs` with every node, or a load-balanced control-plane address. |
| One NATS URL in an event client | The client stops when that node dies | Every node's NATS URL. |
| One node's disk (1 node) | Registrations come back by heartbeat; stable and deprecated marks, app keys, external dependents and event streams are lost (not tested) | 3 nodes (R3 copies), or `gwag cluster export` for a copy of the state ([`cutover.md`](./cutover.md#state-copy)). |
| The admin token | Lost token: no admin writes | `--admin-token-file` / `GWAG_ADMIN_TOKEN` from a secret store, the same on every node ([`deploy.md`](./deploy.md#what-every-platform-gets)). |

## Capacity

Pending. A throughput bench of the cluster per node count is planned;
until then [`perf.md`](./perf.md) has the single-gateway numbers on one
machine. Do not size a deployment from the S14 traffic above: it is a
few hundred calls a second, chosen to see failures, not limits.

## Not tested

- 2 and 5 nodes, `--cluster-replicas 5`.
- A network partition (as opposed to dead nodes).
- Any failure on Compose, Kubernetes or ECS.
- A redundant load balancer; DNS-based failover.
- Disk loss of a cluster node.
- Hosts or networks slower than one box: probe intervals, LB
  thresholds and JetStream timings will differ.
