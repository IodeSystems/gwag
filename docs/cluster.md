# Cluster mode

A gateway can embed a NATS server with JetStream and form a cluster
with peer gateways. The service registry moves into a JetStream KV
bucket so any gateway can dispatch to any service registered with any
other gateway:

```go
cluster, _ := gateway.StartCluster(gateway.ClusterOptions{
    NodeName:      "n1",
    ClientListen:  ":14222",
    ClusterListen: ":14248",
    Peers:         []string{"127.0.0.1:14249"},
    DataDir:       "/var/lib/gwag/n1",
})
defer cluster.Close()

gw := gateway.New(gateway.WithCluster(cluster))
```

- **Bootstrap.** First node in a fresh cluster runs in standalone
  JetStream (R=1) when `Peers` is empty. To scale beyond one node,
  every node — including the seed — must start with at least one
  `Peers` entry.
- **Replicas auto-bump.** As peers join, the registry KV's replica
  count rises monotonically toward `min(peers, 3)`. Killing a peer
  does *not* shrink R automatically; that path is operator-driven
  via `peer forget` (see CLI).
- **Cross-gateway dispatch.** A reconciler on every gateway watches
  the registry KV and dials services it sees, regardless of which
  gateway received the registration.
- **Optional mTLS.** `gateway.LoadMTLSConfig` + `ClusterOptions.TLS`
  + `gateway.WithTLS` requires mutual TLS on both NATS cluster
  routes and outbound gRPC dials.
- **Forget disconnected peers.** `ForgetPeer` (RPC + CLI) drops a
  peer that has TTL-expired and shrinks the registry replica count
  if appropriate. Refuses to forget a still-alive peer.

A runnable 3-gateway demo is in
[`examples/multi/run-cluster.sh`](../examples/multi/run-cluster.sh).
