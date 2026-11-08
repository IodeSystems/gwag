# Registering services

A service reaches the gateway in one of two ways:

- **Control plane.** The service calls the gateway's gRPC control
  plane at startup with its schema and address, then heartbeats. The
  `controlclient` package wraps this. Works against one gateway or a
  cluster, with no gateway restart or config edit.
- **In-process.** The program that embeds the gateway calls
  `AddProto`, `AddOpenAPI`, `AddGraphQL` or `AddMCP` at boot. The
  registration lasts as long as the gateway process.

Versions, deprecation and retirement are in
[`lifecycle.md`](./lifecycle.md). Cluster setup is in
[`cluster.md`](./cluster.md).

## Control plane with controlclient

Import path: `github.com/iodesystems/gwag/v2/gw/controlclient`.

A complete service: a gRPC server, `SelfRegister`, and a drain on
SIGTERM. The proto:

```proto
syntax = "proto3";

package hello.v1;

option go_package = "example.com/hello/gen;hellov1";

// HelloService greets by name.
service HelloService {
  // Hello returns a greeting. Surfaces as Query.hello.hello.
  rpc Hello(HelloRequest) returns (HelloResponse);
}

message HelloRequest {
  // Who to greet.
  string name = 1;
}

message HelloResponse {
  string greeting = 1;
}
```

Generate the stubs (`go.mod` needs `google.golang.org/grpc` and the
gwag module):

```bash
mkdir -p gen
protoc --proto_path=. \
  --go_out=gen --go_opt=paths=source_relative \
  --go-grpc_out=gen --go-grpc_opt=paths=source_relative hello.proto
```

`main.go`:

```go
package main

import (
	"context"
	_ "embed"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/iodesystems/gwag/v2/gw/controlclient"

	hellov1 "example.com/hello/gen"
)

//go:embed hello.proto
var helloProto []byte

type server struct {
	hellov1.UnimplementedHelloServiceServer
}

func (server) Hello(_ context.Context, req *hellov1.HelloRequest) (*hellov1.HelloResponse, error) {
	return &hellov1.HelloResponse{Greeting: "Hello, " + req.GetName() + "!"}, nil
}

func main() {
	listen := flag.String("listen", ":15101", "gRPC listen address")
	advertise := flag.String("advertise", "localhost:15101", "address the gateway dials")
	gateways := flag.String("gateway", "localhost:50090", "control-plane addresses, comma separated")
	flag.Parse()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer()
	hellov1.RegisterHelloServiceServer(srv, server{})
	go func() { log.Fatal(srv.Serve(lis)) }()

	// Ctrl-C: deregister and stop. SIGTERM goes to DrainOnSIGTERM below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Blocks until the gateway is reachable and every dependency is ready.
	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddrs: strings.Split(*gateways, ","),
		ServiceAddr:  *advertise,
		InstanceID:   "hello@" + *advertise,
		Services: []controlclient.Service{{
			Namespace:   "hello",
			Version:     "v1",
			ProtoSource: helloProto,
			Owner:       &controlclient.Owner{Team: "greetings", Contact: "#greetings"},
			Public:      true,
		}},
		DrainOnSIGTERM: true,
		DrainTimeout:   30 * time.Second,
		OnDrained: func() {
			srv.GracefulStop()
			os.Exit(0)
		},
	})
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("registered with %s", reg.GatewayAddr())

	<-ctx.Done()
	_ = reg.Close(context.Background())
	srv.GracefulStop()
}
```

This was built in a scratch module and run against a 3-node cluster
(`gwag up`, see [`cluster.md`](./cluster.md)):

```
$ ./hello --gateway localhost:50090,localhost:50091,localhost:50092
2026/11/07 15:17:03 registered with localhost:50090

$ for p in 18080 18081 18082; do
>   curl -s -X POST localhost:$p/api/graphql -H 'Content-Type: application/json' \
>     -d '{"query":"{ hello { hello(name:\"world\") { greeting } } }"}'; echo
> done
{"data":{"hello":{"hello":{"greeting":"Hello, world!"}}}}
{"data":{"hello":{"hello":{"greeting":"Hello, world!"}}}}
{"data":{"hello":{"hello":{"greeting":"Hello, world!"}}}}
```

The service registered once, with n1; all three nodes answer.

### Options

`controlclient.Options`:

| Field | Meaning |
|---|---|
| `GatewayAddr` | Control-plane `host:port` of one node, or of a load balancer in front of the cluster. |
| `GatewayAddrs` | More control-plane addresses of the same cluster, tried after `GatewayAddr` (which may be empty). Pass all of them. |
| `ServiceAddr` | Where the gateway dials this service: `host:port` for gRPC, the base URL for OpenAPI. Passed verbatim, so it must be reachable from the gateway, not only from the service. |
| `Services` | One or more bindings (below). All share `ServiceAddr`. |
| `InstanceID` | Free-form name shown in listings and used by `gwag drain`. |
| `TTL` | How long the gateway keeps the registration without a heartbeat. 0 uses the server default, 30 s. |
| `HeartbeatInterval` | Default `TTL/3`, or 10 s when `TTL` is 0. |
| `ReadyPollInterval` | Poll rate while a binding waits on dependencies. Default 1 s. |
| `DrainOnSIGTERM` | Catch SIGTERM, drain, deregister. Off by default; see Draining. |
| `DrainTimeout`, `DrainPollInterval` | Bound (default 60 s) and poll rate (default 500 ms) of that drain. |
| `OnDrained` | Runs after the SIGTERM drain. When nil, SIGTERM is raised again with the default handler. |
| `BuildTag` | Set it in release builds (`-ldflags "-X main.buildTag=v1.2.3"`). `SelfRegister` then refuses a `unstable` version. |
| `DialOptions` | Replaces the default insecure dial to the gateway, for TLS. |
| `Logger` | Receives non-fatal events. Default `log.Printf`. |

`controlclient.Service`, one per binding:

| Field | Meaning |
|---|---|
| `Namespace` | GraphQL namespace. Empty falls back to the proto filename stem or the OpenAPI title. |
| `Version` | `v1`, `v2`, ..., or `unstable`. Empty is `v1`. |
| `ProtoSource` | Raw bytes of the entry `.proto`. Embed it with `//go:embed`. |
| `ProtoImports`, or `ProtoFS` and `ProtoEntry` | Other `.proto` files the entry imports, keyed by import path; or an `fs.FS` holding them all. Well-known `google/protobuf/*` imports resolve on their own. |
| `OpenAPISpec` | Raw OpenAPI 3.x JSON or YAML. `ServiceAddr` is then the upstream base URL. |
| `GraphQLEndpoint` | URL of a downstream GraphQL service. The gateway introspects it. `ServiceAddr` is ignored. |
| `MaxConcurrency`, `MaxConcurrencyPerInstance` | Dispatch caps for this binding. 0 uses the gateway default. |
| `Depends` | Services this one calls through the gateway, with the lines it ships clients for. |
| `Owner` | `Team` and `Contact`, free-form. |
| `Public` | Show the binding to admin readers without a token. Admin visibility only; calls are not affected. |
| `Subscribes` | Event channels this binding listens on (`Channel` key, consumer `Group`). Counted like `Depends` on the service that declares each channel. |

Exactly one of `ProtoSource`/`ProtoFS`, `OpenAPISpec` and
`GraphQLEndpoint` per `Service`; the other combinations are refused.

### Two versions in one binary

Pass one `Service` per version. A binary serving a v1 and a v2 of the
same namespace, here with two OpenAPI specs behind one HTTP server:

```go
Services: []controlclient.Service{
	{Namespace: "pinger", Version: "v1", OpenAPISpec: specV1},
	{Namespace: "pinger", Version: "v2", OpenAPISpec: specV2},
},
```

Result on a single `gwag up` gateway:

```
$ curl -s localhost:18300/api/graphql -H 'Content-Type: application/json' \
    -d '{"query":"{ pinger { v1 { ping(who:\"a\") { pong } } v2 { ping(who:\"a\") { pong at } } } }"}'
{"data":{"pinger":{"v1":{"ping":{"pong":"a"}},"v2":{"ping":{"pong":"a","at":null}}}}}
```

The latest version is also flat under the namespace
(`{ pinger { ping ... } }`); older versions appear under `v1`, `v2`,
marked `@deprecated`. Proto services work the same way with
`ProtoSource` per version. Lifecycle rules (`unstable` swaps, `vN` is
locked once registered) are in [`lifecycle.md`](./lifecycle.md).

### Start gate: Depends

`Depends` names services this binding calls through the gateway:

```go
Services: []controlclient.Service{{
	Namespace:   "orders",
	Version:     "v1",
	ProtoSource: ordersProto,
	Depends: []controlclient.Dependency{
		{Service: "inventory", Lines: []string{"v1", "v2"}},
	},
}},
```

`Lines` are `vN` or `unstable`, at least one per dependency. The
gateway keeps the binding out of the schema and out of rotation until
each dependency has a ready instance on one of those lines.
`SelfRegister` blocks until then, polling every `ReadyPollInterval`,
and unblocks on `ctx` ending (it then deregisters and returns the
error). Pass a context with a deadline to bound the wait. While the
gateway is unreachable, `SelfRegister` retries with backoff instead of
failing; a refused registration (bad proto, dependency cycle) returns
at once.

Run with the dependency absent, then started:

```
$ ./orders          # depends on hello(v1|v2); hello not running
$ curl -s localhost:8080/api/graphql ... '{ orders { get(id:"1") { id } } }'
{"data":null,"errors":[{"message":"Cannot query field \"orders\" on type \"Query\"."...}]}
$ ./hello           # in another terminal
2026/11/07 15:22:37 controlclient: waiting for dependencies: orders/v1 needs hello(v1|v2)
2026/11/07 15:22:42 controlclient: dependencies present, starting
2026/11/07 15:22:42 orders/v1 ready
```

After `SelfRegister` returns, `reg.Bindings()` reports the state of
each binding and `reg.PresentLines("hello")` lists the lines of a
dependency that have a ready instance: the lines your client code can
call. `Depends` also feeds the dependency graph and the drain check
below.

### Start gate: Subscribes

`Subscribes` names the event channels this binding listens on, by
channel key (`<proto package>.<channel name>`, see
[`pubsub.md`](./pubsub.md#named-channels)) and consumer group.
Generated event code reports them.

```go
Subscribes: []controlclient.Subscription{
	{Channel: "orders.v1.OrderEvents", Group: "billing"},
},
```

Each entry waits like a dependency on the line whose proto declares
the channel. While no registered service declares it, the binding
waits; the log names it: `billing/v1 needs channel
orders.v1.OrderEvents (not declared yet)`. A channel the binding's own
proto declares does not wait. Subscriptions do not count in the drain
check.

### Draining

A drain stops new requests to one instance, waits for in-flight
requests, and holds while a dependent would be left without a covering
instance.

From the service, on SIGTERM: set `DrainOnSIGTERM: true`. The client
calls Drain, waits for `safe_to_stop` (at most `DrainTimeout`),
deregisters, then runs `OnDrained`. In the example `OnDrained` stops
the gRPC server and exits. A service with its own SIGTERM handler
leaves `DrainOnSIGTERM` off and calls `reg.Drain(ctx)` then
`reg.Close(ctx)` itself. Observed on SIGTERM:

```
2026/11/07 15:22:16 controlclient: drain: DRAIN_STATE_DRAINING inflight=0
```

and the instance was gone from `gwag drain --list` afterwards.

From an operator, `gwag drain`:

```
$ gwag drain --list --gateway localhost:50090
REGISTRATION_ID            INSTANCE_ID              ADDR                     LINES
6381ed3362829d9457e9f867   hello@localhost:15101    localhost:15101          hello/v1

$ gwag drain 'hello@localhost:15101' --wait --gateway localhost:50090
6381ed3362829d9457e9f867 hello@localhost:15101 draining inflight=0 lines=hello/v1
6381ed3362829d9457e9f867 hello@localhost:15101 safe_to_stop inflight=0 lines=hello/v1

$ curl -s localhost:18300/api/graphql ... '{ hello { hello(name:"x") { greeting } } }'
{"data":{"hello":{"hello":null}},"errors":[{"message":"gateway: no live replicas for hello/v1",...}]}

$ gwag drain 'hello@localhost:15101' --cancel --gateway localhost:50090
6381ed3362829d9457e9f867 hello@localhost:15101 serving inflight=0 lines=hello/v1
```

`INSTANCE` is a registration ID or an `InstanceID`. Without `--wait`
the command prints the state and exits; `--wait` polls until
`safe_to_stop` or `--timeout` (default 5 m). `--cancel` withdraws the
drain. With one replica, a drain leaves the service with no live
replica, as the error above shows; drain one replica of several to
roll a deploy.

### Failover

With `GatewayAddrs`, a heartbeat that cannot reach its gateway moves
the registration to the next address, with a new registration ID.
Observed after the gateway holding the registration was killed:

```
15:18:23 controlclient: heartbeat to localhost:50090: rpc error: code = Unavailable ... connection refused
15:18:23 controlclient: registered on gateway localhost:50091 instead of localhost:50090 (registration 4536853a3cab3668a7a09a86)
```

`reg.GatewayAddr()` and `reg.ID()` report where it lives now. Cluster
consequences: [`cluster.md`](./cluster.md#when-a-node-dies).

### Admin auth

The examples above registered against `gwag up` with no token. The
admin roles gate the admin API; they are documented in
[`admin-auth.md`](./admin-auth.md), including what a reader without a
token sees of a `Public` service. Whether to expose the control-plane
port beyond the services that need it is a network decision; it is not
authenticated in these runs.

## In-process registration

A program that embeds the gateway registers upstreams at boot:

```go
gw := gateway.New()
defer gw.Close()

// gRPC upstream: .proto on disk, dispatch target host:port.
if err := gw.AddProto("hello.proto",
	gateway.To("localhost:15101"),
	gateway.As("hello"),
	gateway.Version("v1"),
	gateway.AsPublic(),
); err != nil {
	log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/graphql", gw.Handler())
log.Fatal(http.ListenAndServe(":18200", mux))
```

with the `hello` service from above running:

```
$ curl -s localhost:18200/graphql -H 'Content-Type: application/json' \
    -d '{"query":"{ hello { hello(name:\"world\") { greeting } } }"}'
{"data":{"hello":{"hello":{"greeting":"Hello, world!"}}}}
```

The other three kinds (compiled, not run against live upstreams):

```go
// OpenAPI: spec file or URL; the upstream base URL goes through To.
gw.AddOpenAPI("./billing-openapi.json",
	gateway.As("billing"),
	gateway.To("https://billing.internal"),
	gateway.ForwardHeaders("Authorization"))

// Downstream GraphQL: introspected at registration.
gw.AddGraphQL("https://legacy.internal/graphql", gateway.As("legacy"))

// MCP server: each tool becomes a Mutation. Transports: MCPStdio, MCPHTTP, MCPSSE.
gw.AddMCP(gateway.MCPHTTP, "https://mcp.example.com", gateway.As("tools"))
```

`AddProtoBytes`, `AddProtoFS` and `AddOpenAPIBytes` take bytes or an
`fs.FS` instead of a path.

`ServiceOption`s:

| Option | Effect |
|---|---|
| `To(dest)` | gRPC `host:port` or `*grpc.ClientConn`; for OpenAPI, the base URL. |
| `As(ns)` | GraphQL namespace. |
| `Version(v)` | `vN` or `N`; default `v1`. |
| `AsInternal()` | Callable from hooks, hidden from the public schema. Namespaces starting with `_` are internal already. |
| `AsPublic()` | Show to admin readers without a token. Admin visibility only. `WithPublicServices` does it gateway-wide. |
| `ForwardHeaders(...)` | Inbound headers sent to OpenAPI upstreams. Default `Authorization`. No effect on gRPC. |
| `OpenAPIClient(c)` | `*http.Client` for OpenAPI dispatch. |
| `ProtoImports(m)` | Imported `.proto` bytes for `AddProtoBytes`. |
| `MaxConcurrency(n)`, `MaxConcurrencyPerInstance(n)` | Dispatch caps. |
| `Timeout(d)` | Per-call timeout for this upstream. |

In-process registration has no heartbeat, no start gate and no drain:
the registration exists while the process runs.

## Related

- [`cluster.md`](./cluster.md) — multi-node gateways.
- [`lifecycle.md`](./lifecycle.md) — versions, deprecation, retirement, external dependents.
- [`admin-auth.md`](./admin-auth.md) — roles and public services.
- [`operations.md`](./operations.md) — health, drain, backpressure.
- [`mcp.md`](./mcp.md) — MCP ingest.
