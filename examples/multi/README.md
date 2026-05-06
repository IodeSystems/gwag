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

## CLI alternative

For static registration without writing Go, the [`go-api-gateway`
binary](../../cmd/go-api-gateway) takes `--proto path=addr` flags. Use
that when service-discovery is out of scope and the proto + addr list
is known at deploy time.
