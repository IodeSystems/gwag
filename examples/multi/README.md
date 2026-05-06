# examples/multi

Three separate processes — gateway, greeter, library — wired together
at runtime via the control plane. Demonstrates dynamic service
registration: the gateway boots empty, services self-register, the
GraphQL schema rebuilds in place, and graceful shutdown deregisters.

## Run

```
$ cd examples/multi
$ ./run.sh
control plane listening on :50090
graphql listening on :8080
greeter gRPC listening on :50051
greeter registered with localhost:50090
library gRPC listening on :50052
library registered with localhost:50090
```

In another terminal:

```
$ curl -sS -d '{"query":"{ greeter { hello(name: \"alice\") { greeting } } library { listBooks(author: \"\") { books { title year } } } }"}' \
       -H 'Content-Type: application/json' http://localhost:8080/graphql
{"data":{"greeter":{"hello":{"greeting":"Hello, alice!"}},
         "library":{"listBooks":{"books":[
           {"title":"Go Programming","year":2015},
           {"title":"The Go Programming Language","year":2015},
           {"title":"Designing Data-Intensive Applications","year":2017}]}}}}
```

GraphiQL at <http://localhost:8080/graphql> in a browser.

## What this demonstrates

**Boot-empty.** Start just the gateway (`go run ./cmd/gateway`) and
introspect — the schema has only the inert `_status` field. No services
have registered yet.

**Self-registration.** Each service binary starts its own gRPC server,
then calls `controlclient.SelfRegister` against the gateway's control
plane (`:50090`). The gateway rebuilds its schema and atomically swaps
in the new one; in-flight requests finish on the old schema, new ones
see the additions.

**Graceful deregister.** SIGTERM either service — its `defer
reg.Close(ctx)` calls Deregister, the gateway rebuilds without it.

**Heartbeat-failure eviction.** SIGKILL a service (no graceful exit) —
the gateway notices missed heartbeats and evicts after `TtlSeconds`
(default 30s). The schema rebuilds the same way.

**Single address, multiple services.** Not exercised in this example,
but supported: one binary that hosts multiple services can register
them all with one Register call, sharing a single gRPC connection back
from the gateway. See `controlplane.v1.RegisterRequest.services` (a
repeated `ServiceBinding`).

## Layout

```
examples/multi/
  cmd/
    gateway/main.go    GraphQL :8080 + control plane :50090
    greeter/main.go    Greeter gRPC :50051, self-registers as "greeter"
    library/main.go    Library gRPC :50052, self-registers as "library"
  protos/              Service definitions
  gen/                 protoc-generated bindings (committed for convenience)
  run.sh               Spawns all three; Ctrl-C cleans up
```

The protos and bindings are only needed by the *services* (which want
typed gRPC servers and want to ship their `FileDescriptor` to the
gateway). The gateway itself imports nothing service-specific — it
parses the descriptor at registration time and dispatches via
`dynamicpb`.

## Cluster mode (3 gateways, KV-backed registry)

`run-cluster.sh` boots three gateways with embedded NATS + JetStream.
Each gateway joins the cluster via `--nats-peer`; the registry KV
bucket replicates across nodes (R bumps monotonically as peers join).
Two greeters at v1 and one at v2 register on different gateways:

```
$ ./run-cluster.sh
Cluster up. GraphQL endpoints:
  n1 → http://localhost:18080/graphql
  n2 → http://localhost:18081/graphql
  n3 → http://localhost:18082/graphql
```

Every gateway dispatches to every greeter, regardless of which gateway
received the registration. The same query against any node returns the
same response:

```
$ for p in 18080 18081 18082; do
    curl -sS -X POST http://localhost:$p/graphql \
      -H 'Content-Type: application/json' \
      -d '{"query":"{ greeter { hello(name:\"world\") { greeting } v2{hello(name:\"v2\"){greeting}} } }"}'
  done
```

Behind the scenes:
- `Register` writes to the `go-api-gateway-registry` KV bucket; it does
  not touch local pool state.
- Every gateway watches that bucket and reconciles its pools on each
  Put/Delete, dialing the advertised addr through its own conn pool.
- Heartbeat re-Puts the entry, refreshing the bucket TTL. A service
  that stops heartbeating without graceful Deregister has its key
  auto-expired by JetStream after `peerTTL` (30s).
- Stream replicas auto-bump 1→2→3 as peers join; `monotonic — never
  shrinks automatically`. Operator-driven shrink is a separate path
  (see `peer forget` once it lands).

## Optional mTLS

Pass `--tls-cert`, `--tls-key`, and `--tls-ca` to enable mutual TLS on
both the NATS cluster routes (gateway-to-gateway) and the gRPC control
plane (service-to-gateway). The same cert pair covers both surfaces;
issue one per node from a shared CA:

```
$ go run ./cmd/gateway \
    --node-name n1 --nats-data /tmp/n1 --nats-peer 127.0.0.1:14249 \
    --tls-cert n1.crt --tls-key n1.key --tls-ca shared-ca.crt
```

The library's `gateway.LoadMTLSConfig(cert, key, ca)` returns a
`*tls.Config` with `RequireAndVerifyClientCert` for true mesh mTLS.
Service binaries pass an equivalent TLS dial option to
`controlclient.SelfRegister(... DialOptions: []grpc.DialOption{...})`.
The gateway's outbound dial to registered services also uses the same
cert when `WithTLS` is set.

## CLI alternative

For static registration without writing Go, the [`go-api-gateway`
binary](../../cmd/go-api-gateway) takes `--proto path=addr` flags. Use
that when service-discovery is out of scope and the proto + addr list
is known at deploy time.
