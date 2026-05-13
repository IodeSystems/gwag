# gat — embedded GraphQL + gRPC for huma services

`gat` (GraphQL API Translator) is gwag's embedded sibling: a
single-server, in-process translator that turns a [huma](https://huma.rocks/)
HTTP service into REST + GraphQL + gRPC on **one** port. No NATS, no
cluster, no admin endpoints, no MCP — just typed surfaces emitted
from the same handlers huma already runs.

Import:

```go
import "github.com/iodesystems/gwag/gw/gat"
```

A runnable end-to-end demo with React/TypeScript codegen lives at
[`examples/gat/`](../examples/gat/README.md).

## When to reach for gat (vs full gwag)

gat fits one Go binary serving its own huma operations — single
process, no NATS, no cluster, no runtime control-plane registration,
no admin UI. ~250 deps vs ~498.

Use full gwag when multiple service binaries register at runtime,
when you need NATS-backed pub/sub or cluster KV, or when the admin
UI / MCP tools / self-introspecting admin OpenAPI is load-bearing.

## Mental model

Huma is the source of truth. You register each operation with huma
as you always have — except you call `gat.Register` instead of
`huma.Register` for the operations you want surfaced via GraphQL
and/or gRPC. `gat.Register` is a paired drop-in:

```go
func Register[I, O any](
    api huma.API,
    g *gat.Gateway,
    op huma.Operation,
    handler func(context.Context, *I) (*O, error),
)
```

It calls `huma.Register(api, op, handler)` (so REST keeps working
exactly as before) AND captures the handler reference on `g` so gat
can dispatch GraphQL and gRPC requests to it in-process. No loopback
HTTP, no network hops, no second handler implementation.

After all operations are registered, you wire gat's two mount points:

```go
gat.RegisterHuma(api, g, "/api")    // GraphQL + 3 schema views
gat.RegisterGRPC(mux, g, "/api/grpc") // connect-go handlers
```

`RegisterHuma` finalizes the gateway: it reads huma's accumulated
`OpenAPI` document, ingests it via the same IR pipeline as full
gwag, wires the captured handlers as in-process dispatchers, builds
the GraphQL schema, and mounts the GraphQL + schema-view endpoints
as huma operations.

`RegisterGRPC` adds [connect-go](https://connectrpc.com/) handlers
per operation. Each operation gets a path of the canonical Connect /
gRPC shape: `{prefix}/{Service.FullName}/{MethodName}`. Wire-protocol
coverage is what connect-go gives: grpc-go clients, connect-go
clients, and grpc-web clients all hit the same handler.

## Routes

For a `gat.RegisterHuma(api, g, "/api")` + `gat.RegisterGRPC(mux, g, "/api/grpc")`
setup, the endpoints are:

| URL                                            | What                                        |
| ---------------------------------------------- | ------------------------------------------- |
| `POST /api/graphql`                            | GraphQL queries + mutations                 |
| `GET  /api/schema/graphql`                     | SDL (default), `?format=json` introspection |
| `GET  /api/schema/proto`                       | `descriptorpb.FileDescriptorSet` (binary)   |
| `GET  /api/schema/openapi`                     | Re-emitted OpenAPI document                 |
| `POST /api/grpc/{Service.FullName}/{Method}`   | Connect / gRPC / gRPC-Web                   |

Plus huma's own routes — REST per the operation paths, and huma's
default `/openapi.json` for huma's spec.

## Incremental adoption

`gat.Register` is opt-in per operation. Plain `huma.Register`
operations keep serving REST and don't appear in the GraphQL schema
or proto descriptor set, so you can migrate handler-by-handler.

## `gwag serve` — CLI shortcut

For the single-upstream case (one OpenAPI spec or one `.proto`, one
backend), the `gwag` CLI ships a `serve` subcommand that wires this
package without any Go scaffolding:

```bash
# Front an OpenAPI spec.
gwag serve --openapi spec.yaml --to http://localhost:8081

# Front a remote gRPC service.
gwag serve --proto greeter.proto --to localhost:50051

# Mount under a prefix, override the GraphQL namespace, change listen addr.
gwag serve --openapi spec.yaml --to http://localhost:8081 \
           --addr :9090 --prefix /api --namespace pets
```

The serve command boots gat (no NATS, no admin, no cluster) and
mounts `/graphql` plus `/schema/{graphql,proto,openapi}` on a plain
`http.ServeMux`. For richer needs (auth, metrics, multiple sources)
write Go directly against this package — `gwag serve` is just the
zero-friction entry point.

## Mounting on a plain mux

`gat.RegisterHTTP(mux, g, prefix)` is the huma-free counterpart of
`RegisterHuma`. Mount on any `*http.ServeMux` (or anything satisfying
`gat.HandleMux`) after `gat.New(regs...)` has built the gateway:

```go
g, _ := gat.New(regs...)
mux := http.NewServeMux()
if err := gat.RegisterHTTP(mux, g, "/api"); err != nil {
    log.Fatal(err)
}
http.ListenAndServe(":8080", mux)
```

Endpoints: `POST /api/graphql`, `GET /api/schema/graphql` (SDL or
`?format=json` introspection), `GET /api/schema/proto` (FDS),
`GET /api/schema/openapi`. `gwag serve` uses exactly this path.

## Front a remote gRPC service from a .proto

The huma-paired flow above hosts handlers in the same Go binary as
gat. The companion `gat.ProtoFile` / `gat.ProtoSource` helpers point
gat at a *remote* gRPC service instead — gat compiles the `.proto`,
dials the target, and dispatches GraphQL queries to the upstream via
`grpc.ClientConn.Invoke` per call.

```go
regs, err := gat.ProtoFile("greeter.proto", "localhost:50051")
if err != nil { log.Fatal(err) }
g, err := gat.New(regs...)
if err != nil { log.Fatal(err) }
http.Handle("/graphql", g.Handler())
```

The bytes variant accepts an embedded `.proto`:

```go
//go:embed greeter.proto
var greeterProto []byte

regs, _ := gat.ProtoSource("greeter.proto", greeterProto, nil, "localhost:50051")
g, _ := gat.New(regs...)
```

Namespace / version derive from the proto package by default —
`greeter.v1` → namespace `greeter` + version `v1` (the trailing `vN`
segment is recognised); `pets` → namespace `pets` + version `v1`.
Override per-service after the helper returns:

```go
regs[0].Service.Namespace = "external"
regs[0].Service.Version = "v2"
```

The default transport is insecure (no TLS). For mTLS or other dial
credentials, dial yourself and supply custom dispatchers via
`ServiceRegistration.Dispatchers` — see `gat.New`'s BYO-IR pattern.

This path is unary-only. Server-streaming RPCs (GraphQL
subscriptions) aren't supported by gat; bring full gwag if you need
them.

## Dispatch internals

Each captured operation gets an `ir.Dispatcher` that:

1. Receives the canonical args map (`map[string]any` keyed by IR Arg
   name) from the graphql-go runtime or from a Connect/gRPC request
   (after `protojson` round-trip).
2. Allocates a fresh `*I` via `reflect.New(inputType)`.
3. Walks the IR's `op.Args` and binds each arg's value into the
   matching huma input field by tag (`path:"id"`, `query:"limit"`,
   `header:"X-Foo"`, `cookie:"sid"`) or, for body args, JSON-
   unmarshals into the `Body` field.
4. Calls the captured handler.
5. Extracts `Body` from the typed output (or returns the whole
   struct if there is no `Body` field).

The reflection-based binding is JSON-shaped, so anything huma's
codec already accepts at the REST boundary also works at the GraphQL
and gRPC boundaries.

## Schema endpoints

Three views over the same IR projection:

- `/api/schema/graphql` → `graphql-codegen`, typed-document-node,
  gql.tada
- `/api/schema/proto` → `buf`, `ts-proto`, `protobuf-es`
- `/api/schema/openapi` (or huma's `/openapi.json`) →
  `openapi-typescript`, `openapi-fetch`, `kubb`, `orval`

## Constraints (current shape)

- One huma API per gat gateway. Multi-huma is doable (BYO-IR path)
  but not the recommended path.
- Subscriptions over WebSocket are not in gat. The pub/sub stack
  lives in full gwag (`gw/subscriptions.go`).
- gat doesn't manage auth. Whatever middleware the adopter wraps
  the huma router with applies to gat's GraphQL/gRPC routes too,
  since they ride on the same handler chain.
