# Cluster mode

Gateways form a cluster through an embedded NATS server with
JetStream. The service registry lives in JetStream KV buckets, so a
service registered with one gateway is routed by all of them. A
reconciler on each node watches the registry and dials every service
it sees.

This page covers running a 3-node cluster, what a load balancer should
probe, what happens when a node dies, feature levels, and upgrades
(rolling restart, replica swap, rollback). Health
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

$ gwag peer list --gateway localhost:51091     # a later run, ports 2808x/5109x
NODE_ID                                                    NAME       JOINED                     LEVELS  VERSION
NABY6XWLRI3IKXKG2LHVZWJPKKARA22NQFY4ITZYDFNEH7QO7VXXSZBS   n3         2026-11-08T05:56:20-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty
NBLJNPKR57EMPKU2PPDRPQ6SRGRUGH4AACJFFLQSVLWY2AEBWXVCWJ2K   n1         2026-11-08T05:56:20-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty
NCQS4OY6UGUBQWJ7TXP2ODGVJDQ45NJKZL4IS5E3LLPQNEBUFBH7RQ2F   n2         2026-11-08T05:56:20-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty

cluster feature level: 1
jetstream meta: leader=n1 size=3 offline=none

$ curl -s localhost:18080/api/metrics | grep '^go_api_gateway_cluster_peers'
go_api_gateway_cluster_peers 3
```

This version's health body also carries `"cluster_level":1` on
cluster nodes. LEVELS, VERSION and the two lines under the table are
explained in [Feature levels](#feature-levels).

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

A node started with `--nats-peer` entries does not run alone while
those peers are down. NATS sizes the JetStream meta group from the
route list (at least 2), so one node has no quorum: there is no meta
leader and every JetStream call fails with `JetStream system
temporarily unavailable` (503, err_code 10008). The gateway stays
`starting`. Measured on nats-server v2.15.2: a node started with two
peers that were not up had no leader after 20 s; when the second node
started, a leader was elected within 0.2 s and buckets could be
created; with the third, a bucket went from R1 to R3.

So there is no "single node, then 3" path. Start the first node with
its future peers listed and start a second node before the first has
to serve. A node already running without `--nats-peer` keeps its state
in a standalone JetStream; restarting it with peers is not tested, and
the gateway has no state export or import.

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
| 503 | `incompatible` | The binary cannot run at the cluster's feature level; `error` says why. See [Feature levels](#feature-levels). |

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
$ gwag peer forget NBLJNPKR...WCWJ2K --gateway localhost:51094
removed=false new_replicas=3 meta_removed=true
```

(Four nodes, one stopped.) `removed=false` here: the peers entry had
already expired, so there was nothing to delete. `meta_removed=true`:
the peer also left the JetStream meta group, so its stream replicas
are placed on live nodes and it no longer counts in the meta quorum
(`false` when this node never saw the peer's name, or a live node has
the same name). `new_replicas` is the consequence to plan for: with 3
nodes left it stays 3; with 2 left, `odd` gives 1.
The replica count never drops by itself when a peer is lost; `forget`
re-applies the policy to the remaining peers. Under `odd`, 2 remaining
nodes give R1, so the buckets move to one node and lose their
redundancy. Under `--cluster-replicas=auto` the same call gives R2.
Replace the lost node first if you want to stay at R3. A node that
restarts gets a new node ID; its old ID expires within 30 s and needs
no `forget`.

## Feature levels

Nodes of two versions run side by side during an upgrade. A change to
state the nodes share (a KV value format, a bucket or stream config)
can break the older ones, so such a change is gated on the cluster's
feature level, an integer in the `gwag-cluster` KV bucket.

- Each binary runs at a range of levels and publishes it, with its
  version, in its peers entry. This version: 0-1. A gateway older than
  feature levels (v2.11.0 and before) publishes none and counts as
  0-0.
- A new cluster starts at the highest level its first node supports. A
  node of this version that joins nodes without level support starts
  the cluster at 0.
- A node whose range does not contain the active level does not start
  cluster tracking. Its `/api/health` answers 503 with
  `"status":"incompatible"` and an `"error"` that names both levels,
  and it logs the same line on every retry. A gateway older than
  feature levels cannot check, so it must not be started once the level
  was raised.
- The level goes up only by an operator call, and never goes down.

Level 1 turns subject delete markers on for `gwag-registry` and
`gwag-peers`. NATS refuses a replica-count update without markers on a
bucket that has them, and v2.11.0 sends such updates. At level 0 an
expired key leaves each node by the sweeps, 30-45 s after it expired,
instead of by its marker.

```
$ gwag peer list --gateway localhost:51094
NODE_ID                                                    NAME       JOINED                     LEVELS  VERSION
NABY6XWLRI3IKXKG2LHVZWJPKKARA22NQFY4ITZYDFNEH7QO7VXXSZBS   n3         2026-11-08T05:56:20-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty
NBZNX6HUN4CEB3OUTDNINIFH6X5PD5XA2YMLKHW7WKX3YFZFLOMED5J2   n4         2026-11-08T05:56:33-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty
NCQS4OY6UGUBQWJ7TXP2ODGVJDQ45NJKZL4IS5E3LLPQNEBUFBH7RQ2F   n2         2026-11-08T05:56:20-08:00  0-1     v2.11.1-0.20261108072306-afca24f66349+dirty

cluster feature level: 1
jetstream meta: leader=n2 size=3 offline=none
```

A v2.11.0 node shows `0-0  (no level support)`. The `jetstream meta`
line is the answering node's view of the JetStream meta group: its
leader, its size, and members it reports offline. A stopped node stays
a member until it is forgotten. The meta group needs size/2+1 members
up for any stream create or replica change. The same data is in
`GET /api/admin/peers` (`version`, `minLevel`, `maxLevel`,
`activeLevel`, `metaLeader`, `metaSize`, `metaOffline`), on the admin
UI peers page, and in the metric
`cluster_feature_level{kind="active"|"min"|"max"}`.

### Raising the level

```
gwag cluster finalize --gateway <node> [--level N] [--dry-run]
```

Without `--level` it raises to the highest level the answering node's
binary supports. It is refused (`FailedPrecondition`, admin API 409)
while a live peer's maximum is below the target; the error lists each
one as `NAME VERSION max N`. `--dry-run` prints the same list and
changes nothing. Admin API: `POST /api/admin/cluster/finalize`, body
`{"level": N, "dryRun": true}`.

`--cluster-auto-finalize` (`gwag up`, example gateway;
`gateway.WithClusterAutoFinalize()`) raises the level on its own as
soon as every live peer supports a higher one. It is off by default
and I recommend leaving it off: the raise ends the rollback window, and
"every live peer" does not count a node that is restarting at that
moment. Use it where nobody would roll back anyway (development, a
cluster that is only ever replaced).

### Adding a level (contributors)

`gw/cluster_level.go`: append a `clusterFeature{Level: N+1, Name,
Apply}` to `clusterFeatures`, gate the new writes on
`g.clusterFeatureOn(N+1)`, and keep reading the old format. `Apply`
converts existing state when the level is reached; it runs on every
node and must be idempotent. `applyDeleteMarkers` (level 1) is the
example. Raise `clusterLevelMin` only when the reader of the old format
is removed. Add a CHANGELOG Migration entry that says which release
must finalize first.

## Upgrades

Two ways: restart the nodes in place, one at a time, or swap in new
nodes and remove the old ones (replica swap). Both run two versions
side by side for a while and end with `gwag cluster finalize`.

### Rolling restart

One node at a time. With `odd` and 3 nodes (R3), never take down two.

1. Check all nodes are up: `gwag peer list --gateway <any>` shows 3,
   `jetstream meta: ... offline=none`, every `/api/health` is 200.
2. Send SIGTERM to the node. It reports `draining`, waits
   `--shutdown-delay`, finishes in-flight requests, closes. Services
   registered on it move to another address through `GatewayAddrs`
   within their heartbeat (10 s by default, inside the 30 s TTL).
3. Start the new binary with the same flags and the same data
   directory.
4. Wait for `/api/health` to return 200. The node reports `starting`
   until it replays the registry, and `incompatible` if its binary
   cannot run at the cluster's level: put the old binary back.
5. Confirm `gwag peer list` shows 3 and `cluster_peers` is 3 on every
   node, then move to the next.
6. When every node runs the new binary, `gwag cluster finalize`.

Tested: n2 was stopped with SIGTERM and restarted (health 200 again
after about a second, three peers listed), then n1 after a `kill -9`
(new node ID, three peers, queries answered). Not tested: a rolling
restart from v2.11.0 to this version. A v2.11.0 gateway has no
graceful drain: SIGTERM cuts its in-flight requests, so take it out of
the load balancer first.

### Replica swap

Three new ("green") nodes join three old ("blue") ones, JetStream moves
every replica onto green, and blue is removed. Six nodes run for a
while, so plan for the extra hosts. The replica policy `odd` keeps R3
at six nodes.

1. Check blue as in the rolling restart, step 1.
2. Start green nodes one at a time. Give each a new `--node-name` and
   data directory, and list every running node in `--nats-peer`. Before
   starting the next, wait until `gwag peer list --gateway <new node>`
   lists every node and shows `jetstream meta: leader=<a blue node or
   an earlier green one> size=<running nodes>`.

   Do not start several new nodes at once. Fresh JetStream servers that
   start together can elect a meta leader among themselves: with three
   fresh nodes started together and the three blue routes, that
   happened in 6 of 8 runs; with all six routes listed, in 2 of 3
   (nats-server v2.15.2, in-process). The fresh group won, and the blue
   buckets disappeared from the meta view (`stream not found`). Started
   one at a time, the new node joined the blue group in every run (7
   in-process runs, and every e2e run, which checks it).
3. Put green into the load balancer once each answers 200.
4. Give the services the green control planes (`GatewayAddrs` in
   `controlclient`), so a registration on a blue node moves to green
   when blue stops answering.
5. For each blue node, one at a time:
   1. Take it out of the load balancer and wait for its in-flight
      requests.
   2. Move its replicas to the other nodes:

      ```
      gwag peer evacuate --gateway <green node> --wait 2m <blue NODE_ID>
      ```

      It sends NATS `$JS.API.SERVER.EVACUATE` again every second until
      no stream has a replica on the node and every replica is current,
      then prints `peer=<name> done=true`. It resends because a replica
      raise in between (a new bucket going from R1 to R3) can place a
      replica back on the node. Exit code 3 means not done in time; run
      it again.
   3. SIGTERM the blue node.
   4. When its peers entry has expired (30 s), forget it:

      ```
      $ gwag peer forget --gateway <green node> <blue NODE_ID>
      removed=false new_replicas=3 meta_removed=true
      ```

      `meta_removed=true`: the node left the JetStream meta group
      (`$JS.API.SERVER.REMOVE`). Until then a stopped node still counts
      in the meta quorum: with six members, three stopped blue nodes
      leave no quorum, and stream creates and replica changes fail.
   5. Check `gwag peer list` on a green node: one node fewer, `jetstream
      meta: ... offline=none`.

   One at a time also matters for placement: an evacuated node is kept
   out of a stream only until that stream is back at full size, so a
   later evacuation can pick a blue node that has not been removed yet.
6. `gwag cluster finalize --gateway <green node>`. It is refused while
   any blue node is live.

`gwag peer evacuate` and the meta removal need a node of this version:
the JetStream server APIs answer only in the NATS system account, and
only this version's nodes have a system account user (a random
password, used in-process). Blue nodes running v2.11.0 cannot do either.

Tested: `TestS13ReplicaSwap` (e2e): blue on v2.11.0, green at HEAD,
four load workers through a health-checking load balancer, a service
registered on b1 that moves to green, events published into a
JetStream stream of a named channel the whole time, and an app key
created on green. Result: 33410 GraphQL calls, 0 failed; 4036 events
acked, all in the stream, none twice; stable marks and the app key
unchanged; every stream at R3 on green; level 0 until finalize, then 1
with delete markers on. Not tested: a swap between two versions that
both have feature levels (that is the next release), and a swap with
`--cluster-replicas` other than `odd`.

### Rollback window

Before `gwag cluster finalize`, the old binary can come back:

- Rolling restart: restart nodes with the old binary, same way.
- Replica swap: remove the green nodes the way step 5 removes blue
  ones. The last node of the new version leaves the meta group itself,
  because the old nodes cannot remove it:

  ```
  gwag peer evacuate --gateway <last green> --leave --wait 2m <its NODE_ID>
  ```

  `--leave` removes it from the meta group once no stream is on it
  (`left=true`); its JetStream turns off, so stop it right after.

After the raise, a binary whose maximum is below the new level reports
`incompatible` and does not join. A v2.11.0 binary cannot check and
would run against buckets it cannot update: do not start one.

Tested: `TestS13bRollbackBeforeFinalize` (e2e): two HEAD nodes joined
a v2.11.0 cluster and were removed (g2 forgotten through g1, g1 with
`--leave`) under load: 20934 calls, 0 failed; level still 0; streams
back on blue only; a stream create on the blue cluster worked
afterwards (meta quorum); a new service registered on a blue node and
every blue node routed it.

### Mixed versions

The CHANGELOG Migration entries under `Unreleased` list what to know
before an upgrade across them. For clusters:

- Delete markers on `gwag-registry` and `gwag-peers` are level 1; see
  above.
- A 2-node cluster now runs R1, not R2. Existing buckets at R2 stay
  until `ForgetPeer` or a fixed policy changes them.

See [`CHANGELOG.md`](../CHANGELOG.md).

## Related

- [`services.md`](./services.md) — registering services against a cluster.
- [`operations.md`](./operations.md) — drain, replicas, metrics.
- [`admin-auth.md`](./admin-auth.md) — admin roles on the control plane.
