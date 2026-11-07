# Cluster mode

Gateways form a cluster through an embedded NATS server with
JetStream. The service registry lives in JetStream KV buckets, so a
service registered with one gateway is routed by all of them. A
reconciler on each node watches the registry and dials every service
it sees.

This page covers running a 3-node cluster, what a load balancer should
probe, what happens when a node dies, and rolling upgrades. Health
states, drain mechanics and the replica table are in
[`operations.md`](./operations.md); this page links there instead of
repeating them.

## Run 3 nodes

Two binaries run a cluster node with the same flags, under different
names for a few of them:

| Setting | `gwag up` | `examples/multi/cmd/gateway` |
|---|---|---|
| GraphQL / HTTP listen | `--addr` (`:8080`) | `--http` (`:8080`) |
| Control-plane gRPC listen | `--control-plane` (`:50090`) | `--control-plane` (`:50090`) |
| NATS client listen | `--nats-listen` (`:14222`) | `--nats-listen` (`:14222`) |
| NATS cluster route listen | `--nats-cluster` (`:14248`) | `--nats-cluster` (`:14248`) |
| Peers, repeatable | `--nats-peer HOST:PORT` | `--nats-peer HOST:PORT` |
| Data directory | `--data-dir` | `--nats-data` |
| Node name | `--node-name` (default: control-plane address) | `--node-name` (same default) |
| Cluster name | `--nats-cluster-name` (default `gwag`) | `--nats-cluster-name` |
| Environment label | `--environment` | `--environment` |
| Replica policy | `--cluster-replicas` (`odd`) | `--cluster-replicas` (`odd`) |
| Drain delay and bound | `--shutdown-delay` (5s), `--shutdown-timeout` | same |
| mTLS | none | `--tls-cert`, `--tls-key`, `--tls-ca` |

`gwag up` also serves the admin UI and writes a login to
`.gw/credentials.json`; pass `--no-context-write` on nodes that should
not. The example gateway needs `--nats-data` to enable cluster mode at
all. `--environment` is a label shown by the admin API and UI; it does
not change behaviour.

Three nodes on one host, `gwag up`. Each node lists the other two as
peers:

```bash
gwag up --node-name n1 --addr :18080 --control-plane :50090 \
  --nats-listen :14222 --nats-cluster :14248 --data-dir /var/lib/gwag/n1 \
  --nats-peer 127.0.0.1:14249 --nats-peer 127.0.0.1:14250 \
  --environment demo --no-context-write

gwag up --node-name n2 --addr :18081 --control-plane :50091 \
  --nats-listen :14223 --nats-cluster :14249 --data-dir /var/lib/gwag/n2 \
  --nats-peer 127.0.0.1:14248 --nats-peer 127.0.0.1:14250 \
  --environment demo --no-context-write

gwag up --node-name n3 --addr :18082 --control-plane :50092 \
  --nats-listen :14224 --nats-cluster :14250 --data-dir /var/lib/gwag/n3 \
  --nats-peer 127.0.0.1:14248 --nats-peer 127.0.0.1:14249 \
  --environment demo --no-context-write
```

On separate hosts, keep the default ports and put each other node's
`host:14248` in `--nats-peer`. Start order does not matter; the three
above were started together. Within about ten seconds:

```
$ curl -s -w ' %{http_code}\n' localhost:18080/api/health
{"active_streams":0,"node_id":"NAIW5CIN...","pubsub":"nats","status":"serving"}
 200

$ gwag peer list --gateway localhost:50090
NODE_ID                                                    NAME       JOINED
NAIW5CINQ3LSRVPLGRZKCEFAPCANIH5G4YNWL3KHXPBGPPER7PUW2G7K   n1         2026-11-07T15:16:00-08:00
NBCMQ6KJFAI2MM2YRKDJO3S3EVHEBVPK5N7CJHW2UY5KSR63SF4SITNM   n3         2026-11-07T15:16:00-08:00
ND4CWNE52RNBG3FYOV34EKIJL4DVHUUSBGBKWYSSJT63YCZ42DKPA55Y   n2         2026-11-07T15:16:00-08:00

$ curl -s localhost:18080/api/metrics | grep '^go_api_gateway_cluster_peers'
go_api_gateway_cluster_peers 3
```

The example gateway takes the same shape. A runnable version is
[`examples/multi/run-cluster.sh`](../examples/multi/run-cluster.sh).

```bash
gateway --node-name n1 --http :18080 --control-plane :50090 \
  --nats-listen :14222 --nats-cluster :14248 --nats-data /var/lib/gwag/n1 \
  --nats-peer 127.0.0.1:14249 --nats-peer 127.0.0.1:14250
```

### Bootstrap rule

A node with no `--nats-peer` runs standalone JetStream at one replica,
and cannot be scaled out later. To get a cluster, start every node,
the first one included, with at least one peer entry. Listing all the
other nodes on each node, as above, needs no designated seed.

### Embedded in Go

The flags map to `ClusterOptions`:

```go
cluster, err := gateway.StartCluster(gateway.ClusterOptions{
    NodeName:      "n1",
    ClientListen:  ":14222",
    ClusterListen: ":14248",
    Peers:         []string{"127.0.0.1:14249", "127.0.0.1:14250"},
    DataDir:       "/var/lib/gwag/n1",
})
if err != nil { log.Fatal(err) }
defer cluster.Close()

gw := gateway.New(
    gateway.WithCluster(cluster),
    gateway.WithClusterReplicas(gateway.ClusterReplicasOdd),
)
```

### mTLS

`gateway.LoadMTLSConfig(cert, key, ca)` returns a config that requires
and verifies client certificates. Pass it to `gateway.WithTLS`; it
covers the NATS cluster routes and outbound gRPC dials from the
gateway. Use one CA and a cert valid for both server and client use
(`extendedKeyUsage=serverAuth,clientAuth`) whose SANs match the
addresses peers dial. The example gateway wires it with
`--tls-cert`, `--tls-key` and `--tls-ca`; `gwag up` has no TLS flags.

Run here with one shared cert, `subjectAltName=IP:127.0.0.1`, three
example-gateway nodes formed the cluster (`/api/health` 200 on each,
`cluster_peers 3`). Not tested: a node with a cert from another CA, and
a service dialling a control plane that requires TLS.

### Replicas

`--cluster-replicas` sets the replica count of the KV buckets:
`odd` (default), `auto`, or a fixed 1 to 5. The table, and why `odd`
is the default, are in
[`operations.md`](./operations.md#cluster-kv-replicas). In short, 3
nodes run at R3 and survive one node loss; 2 nodes run at R1 under
`odd`, so the buckets live on one node.

## Load balancer probe

Probe `/api/health` on the HTTP port (`gwag up` and the example
gateway both mount it there; `gw.HealthHandler()` mounts it wherever
you choose). It answers:

| Status | Body `status` | Meaning |
|---|---|---|
| 200 | `serving` | Take traffic. |
| 503 | `starting` | Cluster node that has not replayed the registry from JetStream KV. Its schema would miss services. It turns ready on its own; there is no timeout. |
| 503 | `draining` | SIGTERM or `Drain` received. Pull the node. |

Observed on a node sent SIGTERM (default `--shutdown-delay 5s`, probe
every 0.5 s):

```
15:17:46.4 200 "status":"serving"
15:17:46.9 503 "status":"draining"      <- SIGTERM at 15:17:46.8
 ...        503 "status":"draining"      (for the 5 s delay)
 then       connection refused
```

I did not see `starting` here: with an empty registry the replay
finishes before the first probe. It appears with a populated registry
or an unreachable JetStream; see
[`operations.md`](./operations.md#health--graceful-drain).

On SIGTERM the node flips health to 503, waits `--shutdown-delay`,
closes the listener and waits up to `--shutdown-timeout` (default 30s)
for in-flight requests. Set `--shutdown-delay` to at least probe
interval times unhealthy threshold, so the load balancer has removed
the node before the listener closes. Worst case from signal to exit:
delay + 2 x timeout. A second signal exits at once. In Kubernetes use
`/api/health` as the readiness probe, and a `startupProbe` or a TCP
liveness probe for the `starting` window.

## When a node dies

Test: three nodes, a service registered with node n1 and given all
three control-plane addresses. n1 was killed with `kill -9`.

- **Routing stays up.** Queries sent to n3 returned the correct answer
  at every probe, 3 s apart, through and after the kill.
- **Service client fails over.** The service's heartbeat to n1 failed;
  7 s after the kill it registered on n2 with a new registration ID:

  ```
  15:18:23 controlclient: heartbeat to localhost:50090: rpc error: code = Unavailable desc = ...connection refused
  15:18:23 controlclient: registered on gateway localhost:50091 instead of localhost:50090 (registration 4536853a3cab3668a7a09a86)
  ```

  This needs more than one address: `Options.GatewayAddrs` in
  [`controlclient`](./services.md), or a load balancer address.
- **Registrations expire.** A registration lives on the node that
  accepted it and expires 30 s after its last heartbeat, on every node
  (delete markers). A service with one address and a dead gateway is
  dropped by every node after the TTL, and registers again when the
  gateway returns. Details, including the 10-20 s lag on a node whose
  registry watch ran on the dead node:
  [`operations.md`](./operations.md#registrations-when-a-cluster-node-dies).
- **Peer list.** The dead peer stays in `peer list` until its 30 s
  entry expires. `cluster_peers` fell from 3 to 2 once the NATS route
  closed.
- **Quorum.** With 3 nodes at R3, losing one keeps a JetStream quorum.

## Forget a dead peer

A peer that expired and will not return can be forgotten. The call is
refused for a live peer:

```
$ gwag peer forget NBCMQ6KJ...SITNM --gateway localhost:50091
ForgetPeer: rpc error: code = Unknown desc = controlplane: peer NBCMQ6KJ...SITNM is still alive — wait for TTL (30s) to expire
```

and for an expired one:

```
$ gwag peer forget NAIW5CIN...PUW2G7K --gateway localhost:50091
removed=false new_replicas=1
```

`removed=false` here: the peers entry had already expired, so there was
nothing to delete. `new_replicas=1` is the consequence to plan for.
The replica count never drops by itself when a peer is lost; `forget`
re-applies the policy to the remaining peers. Under `odd`, 2 remaining
nodes give R1, so the buckets move to one node and lose their
redundancy. Under `--cluster-replicas=auto` the same call gives R2.
Replace the lost node first if you want to stay at R3. A node that
restarts gets a new node ID; its old ID expires within 30 s and needs
no `forget`.

## Rolling upgrade

One node at a time. With `odd` and 3 nodes (R3), never take down two.

1. Check all nodes are up: `gwag peer list --gateway <any>` shows 3,
   every `/api/health` is 200.
2. Send SIGTERM to the node. It reports `draining`, waits
   `--shutdown-delay`, finishes in-flight requests, closes. Services
   registered on it move to another address through `GatewayAddrs`
   within their heartbeat (10 s by default, inside the 30 s TTL).
3. Start the new binary with the same flags and the same data
   directory.
4. Wait for `/api/health` to return 200. The node reports `starting`
   until it replays the registry.
5. Confirm `gwag peer list` shows 3 and `cluster_peers` is 3 on every
   node, then move to the next.

Tested: n2 was stopped with SIGTERM and restarted (health 200 again
after about a second, three peers listed), then n1 after a `kill -9`
(new node ID, three peers, queries answered). Not tested: swapping in a
different binary version.

### Mixed versions

A rolling upgrade runs mixed versions for a while. The CHANGELOG
Migration entries under `Unreleased` list what to know; read them before
an upgrade across those changes. Two affect clusters:

- Once a node of the new version has turned delete markers on for the
  `gwag-registry` and `gwag-peers` buckets, an older node's
  replica-count update of those buckets is refused by NATS. The older
  node logs nothing and retries on the next peer change. The buckets
  stay at the replica count the newer nodes set. Upgrade every node
  before relying on an older node to raise replicas.
- A 2-node cluster now runs R1, not R2. Existing buckets at R2 stay
  until `ForgetPeer` or a fixed policy changes them.

See [`CHANGELOG.md`](../CHANGELOG.md).

## Related

- [`services.md`](./services.md) — registering services against a cluster.
- [`operations.md`](./operations.md) — drain, replicas, metrics.
- [`admin-auth.md`](./admin-auth.md) — admin roles on the control plane.
