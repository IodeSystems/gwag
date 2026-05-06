# go-api-gateway

A small Go library for fronting gRPC services with a GraphQL surface.
Hand it a list of `.proto` files and gRPC destinations; it gives back an
`http.Handler` that serves a namespaced GraphQL API. Layer middleware on
top to add auth, rate limiting, transforms, and field hiding without
touching the underlying services.

Status: **early v0.** Unary gRPC calls work end-to-end; `HideAndInject`
strips fields from the public schema and injects them at runtime;
dynamic registration via the control plane lets services self-register
at runtime with graceful deregister and heartbeat-failure eviction;
the [`go-api-gateway` CLI](./cmd/go-api-gateway) ships as a no-Go-needed
entry point for static configs. Streaming and the broader
`SchemaMiddleware` story are stubbed; rough edges throughout.

## Examples

- [`examples/multi`](./examples/multi) — three separate processes
  (gateway + greeter + library) wired via the control plane. The
  schema rebuilds in place as services join and leave.
- [`examples/auth`](./examples/auth) — `HideAndInject[*authpb.Context]`
  hiding auth fields globally and filling them from a registered
  internal service.

## Dynamic registration

Services self-register with the gateway over a gRPC control plane and
heartbeat to stay alive:

```go
import (
    "github.com/iodesystems/go-api-gateway/controlclient"
    greeterv1 "yourrepo/gen/greeter/v1"
)

reg, _ := controlclient.SelfRegister(ctx, controlclient.Options{
    GatewayAddr: "gateway:50090",
    ServiceAddr: "greeter:50051",
    Services: []controlclient.Service{
        {Namespace: "greeter", FileDescriptor: greeterv1.File_greeter_proto},
    },
})
defer reg.Close(ctx) // graceful deregister
```

One Register call can carry many services on one address (multiple
RPCs in one binary). Heartbeats every TTL/3; missed heartbeats past
TTL evict. The control-plane API is in
[`controlplane/v1/control.proto`](./controlplane/v1/control.proto).

## CLI

```
$ go install github.com/iodesystems/go-api-gateway/cmd/go-api-gateway@latest
$ go-api-gateway \
    --proto ./greeter.proto=greeter-svc:50051 \
    --proto ./library.proto=commerce@library-svc:50052 \
    --addr :8080
```

`--proto PATH=[NAMESPACE@]ADDR`, repeatable. Default namespace is the
filename stem; default addr is `:8080`. Insecure dial — wrap in real
TLS via the library API for production.

## Why another gateway

There are plenty of API gateways. This one is a *library*, not a
binary. The intent is small composable tools: this library does
gRPC→GraphQL with a middleware story; it does not own service
discovery, deploy automation, observability backends, or auth policy.
You compose it with whatever you already have.

It also intentionally does not require codegen for the simplest path.
Drop in `.proto` files and run; reach for a sibling protoc plugin
([`grpc-graphql-gateway`](https://github.com/iodesystems/grpc-graphql-gateway))
when you want typed handles for power-user middleware.

## The thirty-second tour

```go
gw := gateway.New()

gw.AddProto("./protos/auth.proto", gateway.To("authsvc:50051"))
gw.AddProto("./protos/user.proto", gateway.To("usersvc:50051"))
// namespaces default to "auth" and "user" (filename stems)

http.ListenAndServe(":8080", gw.Handler())
```

GraphQL surface:

```graphql
query {
  auth { ... }   # auth.proto's RPCs
  user { ... }   # user.proto's RPCs
}
```

Override the default namespace, or share a connection pool:

```go
conn, _ := grpc.NewClient("billing.svc.cluster:50051", opts...)
gw.AddProto("./protos/billing.proto",
    gateway.As("commerce"),
    gateway.To(conn),
)
```

## Middleware

One primitive shape, three idioms — same `next()` chain you've seen in
every Go middleware library, applied at two layers.

```go
mw := func(next gateway.Handler) gateway.Handler {
    return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
        // pre — filter or transform request
        resp, err := next(ctx, req)
        if err != nil { return nil, err }
        // post — transform response
        return resp, nil
    }
}
```

- **Observer**: call `next`, return its result, do something on the side (log, metric, trace).
- **Filter**: return an error without calling `next` (auth, rate limit, allow-list). Use `gateway.Reject(code, msg)` so the gateway can map to the right GraphQL error.
- **Transform**: wrap `next` and mutate input or output.

Streaming uses the same shape over `iter.Seq2[T, error]`:

```go
type StreamHandler func(ctx context.Context, in iter.Seq2[T, error]) iter.Seq2[T, error]
```

Reads as `for req, err := range in { ... yield(...) }`. Errors flow
inline; cancellation rides `ctx`. Two interfaces (unary +
stream) because forcing `iter.Seq2` on single-shot RPCs is annoying.

## Two layers, paired declarations

There are *two* middleware pipelines:

1. **Schema** — runs once at gateway boot, rewrites the GraphQL schema (hide types, hide fields, rename, restrict).
2. **Runtime** — runs per request, transforms requests and responses.

They are separate pipelines because they run at different times, but
they often need to stay in sync. Hiding `userID` from the external
schema is meaningless without a runtime hook to fill it from
context, and vice versa. The library bundles paired rules into a single
declaration so the two halves can't drift:

```go
type Pair struct {
    Schema  SchemaMiddleware
    Runtime Middleware
    Stream  StreamMiddleware
}

gw.Use(gateway.HideAndInject[*authpb.Context](authResolver))
```

`HideAndInject` returns a `Pair` whose `Schema` half strips every
field of type `*authpb.Context` from the external schema and whose
`Runtime` half populates those fields by calling `authResolver(ctx)`.
One declaration; the invariant is enforced by construction.

Single-purpose middleware (logging, rate limit) just fills one half of
`Pair` and no-ops the other.

## The auth case end-to-end

The shape that drove the API: globally hide auth fields, fill them from
a registered auth service, and hide that service from the external
schema too. See [`examples/auth`](./examples/auth) for runnable code; the
shape is:

```go
gw := gateway.New()

// Internal: not exposed in the GraphQL surface, but callable by hooks.
gw.AddProto("./protos/auth.proto",
    gateway.To(authConn),
    gateway.AsInternal(), // (planned) hide from public schema
)

// Public services.
gw.AddProto("./protos/user.proto", gateway.To(userConn))

// Pair the schema rule with the runtime resolver.
gw.Use(gateway.HideAndInject[*authpb.Context](func(ctx context.Context) (*authpb.Context, error) {
    token := bearerFromContext(ctx)
    return authClient.Resolve(ctx, &authpb.ResolveRequest{Token: token})
}))

http.ListenAndServe(":8080", gw.Handler())
```

External GraphQL surface contains no `auth` namespace and no
`AuthContext` type. Internally, every RPC whose input embeds
`AuthContext` gets it filled from one cached call to the auth service
per request.

## Design notes

- **Runtime proto parsing.** `.proto` files are parsed at boot via
  `bufbuild/protocompile` + `jhump/protoreflect`; gRPC calls go out via
  `dynamicpb`. No codegen step required for the simplest path.
- **Path-based identity.** Namespaces default to filename stems;
  collisions across registered files are an error, not silent
  overwrite.
- **Static destinations first.** `To(addr string)` and
  `To(conn *grpc.ClientConn)` ship on day one. A `Resolver` interface
  for service-discovery shaped destinations (NATS, DNS) is intentionally
  deferred until something pulls on it.
- **Two registries.** Public schema view vs internal callable registry.
  Internal-only services live in the callable registry but not the
  external schema; hooks (auth resolver, etc.) call them.
- **Caching is library-side.** A naive auth resolver gets called once
  per field per request; the library memoises per-(request, type) so
  users don't reinvent it. Distributed cache backends are pluggable
  later but the API doesn't preclude them.
- **`Reject(code, msg)` for short-circuits.** Plain errors are mapped
  to opaque internal errors; typed rejections become the right GraphQL
  error code (and gRPC status when bridged outbound).

## What's not in here

- Service discovery / dynamic registration. (Library is composable with
  whatever you already use.)
- Rolling deploy / hot reload. (Out of scope; gateway can be run
  blue/green like any other binary.)
- Multi-protocol ingest (OpenAPI, gRPC-Web, etc.). On the roadmap, not
  on day one.
- Observability backends. The middleware shape is the integration
  point; pick your tracer and write a five-line `Pair`.

## License

MIT. See [LICENSE](./LICENSE).
