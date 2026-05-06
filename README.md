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
multi-gateway clusters share a JetStream KV registry over embedded
NATS so any node dispatches to any registered service, with optional
mTLS on cluster routes; the [`go-api-gateway` CLI](./cmd/go-api-gateway)
ships as a no-Go-needed entry point for static configs and operator
peer management. Streaming and the broader `SchemaMiddleware` story
are stubbed; rough edges throughout.

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

## Cluster mode

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
    DataDir:       "/var/lib/go-api-gateway/n1",
})
defer cluster.Close()

gw := gateway.New(gateway.WithCluster(cluster))
```

- **Bootstrap.** The first node in a fresh cluster runs in standalone
  JetStream (R=1) when `Peers` is empty. To scale beyond one node,
  every node — including the seed — must start with at least one
  `Peers` entry; nodes route to each other via NATS gossip.
- **Replicas auto-bump.** As peers join, the registry KV's replica
  count rises monotonically toward `min(peers, 3)`. Killing a peer
  does *not* shrink R automatically; that path is operator-driven via
  `peer forget` (see CLI).
- **Cross-gateway dispatch.** A reconciler on every gateway watches
  the registry KV and dials services it sees, regardless of which
  gateway received the registration.
- **Optional mTLS.** Pass a `*tls.Config` from `gateway.LoadMTLSConfig`
  via `ClusterOptions.TLS` and `gateway.WithTLS` to require mutual TLS
  on both NATS cluster routes and the gateway's outbound gRPC dials.
- **Forget disconnected peers.** `ForgetPeer` (RPC + CLI) drops a
  peer that has TTL-expired and shrinks the registry replica count
  if appropriate. Refuses to forget a still-alive peer.

A runnable 3-gateway demo is in
[`examples/multi/run-cluster.sh`](./examples/multi/run-cluster.sh).

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

For a running cluster, the same binary exposes operator and codegen
subcommands:

```
$ go-api-gateway peer list   --gateway gw1.internal:50090
$ go-api-gateway peer forget --gateway gw1.internal:50090 NODE_ID

$ go-api-gateway services list --gateway gw1.internal:50090
$ go-api-gateway schema fetch  --endpoint https://gw.internal/schema > schema.graphql
$ go-api-gateway schema diff   --from https://prod-gw.internal/schema \
                                --to   https://staging-gw.internal/schema --strict
$ go-api-gateway sign          --gateway gw1.internal:50090 \
                                --channel events.UserEvents.UserCreated.42 --ttl 60
```

- `peer forget` only succeeds against a node whose KV entry has
  expired (safe shrink); `peer list` shows live entries.
- `services list` returns `(namespace, version, hash, replica_count)`
  for cross-cluster parity checks — identical hashes across two
  clusters mean identical proto bytes, the foundation for safe
  promotion.
- `schema fetch` GETs the gateway's `/schema` endpoint as SDL (or
  introspection JSON via `--json`) for client codegen.
- `schema diff --strict` fails CI when a candidate schema would break
  existing consumers.

## Subscriptions (events)

Server-streaming RPCs are auto-promoted to GraphQL subscriptions.
Each `rpc Foo(Filter) returns (stream Event)` becomes a flat field
`<namespace>_<lowerCamel(method)>` on the `Subscription` root, with
the request type's fields as arguments. The gateway also injects
`hmac: String!` and `timestamp: Int!` for HMAC verification.

Transport is the standard `graphql-transport-ws` WebSocket
subprotocol (Apollo, urql, graphql-codegen all speak it). Backing
storage is **NATS pub/sub**, not gRPC streaming — services publish to
NATS subjects derived from the (namespace, method, request fields);
the gateway bridges WebSocket subscribes to NATS subscribes with
in-process fan-out (one NATS sub shared across N WebSockets watching
the same subject).

```proto
service UserEvents {
  rpc UserCreated(UserCreatedFilter) returns (stream UserCreatedEvent);
}
message UserCreatedFilter { string user_id = 1; }
message UserCreatedEvent  { string user_id = 1; string email = 2; }
```

becomes

```graphql
type Subscription {
  userEvents_userCreated(userId: ID!, hmac: String!, timestamp: Int!): UserCreatedEvent
}
```

The publisher service never implements server-streaming gRPC — it
just `nats.Publish()`es to the resolved subject:

```go
event, _ := proto.Marshal(&UserCreatedEvent{UserId: u.ID, Email: u.Email})
natsConn.Publish(fmt.Sprintf("events.UserEvents.UserCreated.%s", u.ID), event)
```

Clients use any graphql-ws library:

```typescript
const { data } = useSubscription(gql`
  subscription($u: ID!, $h: String!, $t: Int!) {
    userEvents_userCreated(userId: $u, hmac: $h, timestamp: $t) {
      userId email
    }
  }
`, { variables: { u, hmac, timestamp } });
```

### HMAC channel auth

Three modes (operator picks at gateway boot):

- `--insecure-subscribe` — bypass verification (dev only).
- `--subscribe-secret <hex>` — gateway holds a shared secret;
  verifies HMAC-SHA256(secret, "<channel>\n<timestamp>") base64 on
  every subscribe. Default `--subscribe-skew 5m`.
- Plus, optionally, a registered `SubscriptionAuthorizer` delegate
  (gRPC service registered under namespace `_events_auth/v1`) that
  the gateway consults at *sign* time. The delegate decides whether
  to authorize signing a token for `(channel, timestamp, ttl)`.

Tokens are minted via the gateway's gRPC `SignSubscriptionToken`:

```
$ go-api-gateway sign --gateway gw1.internal:50090 \
                      --channel events.UserEvents.UserCreated.42 --ttl 60
hmac=md6l2SVJ...
ts=1778092482
```

Or signed locally if you hold the secret yourself:

```
$ go-api-gateway sign --secret <hex> --channel events.... --ttl 60
```

Verification outcomes are surfaced as `SubscribeAuthCode` (`OK`,
`TOO_OLD`, `SIGNATURE_MISMATCH`, `MALFORMED`, `DENIED`, `UNAVAILABLE`,
`NOT_CONFIGURED`, `UNKNOWN_KID`). The code lands in
`go_api_gateway_subscribe_auth_total{code=...}` and in the WebSocket
error frame's `extensions.subscribeAuthCode`.

Client-streaming and bidi RPCs aren't promoted — they're filtered
with a registration-time warning so operators can see what's hidden.

## Backpressure & metrics

Each `(namespace, version)` pool has its own dispatch concurrency caps
and per-dispatch wait budget. Slow services back up *their own* pool
without blocking dispatches to other pools — a sluggish `auth`
service does not gate `library` requests. Subscriptions have a
*separate* slot from unary so long-lived streams don't crowd queries.

Defaults (override via `gateway.WithBackpressure(...)`):

```go
DefaultBackpressure = BackpressureOptions{
    MaxInflight: 256,             // per-pool concurrent unary dispatches
    MaxStreams:  64,              // per-pool active subscription streams
    MaxWaitTime: 10 * time.Second, // wait budget; exceeded → fast-reject
}
```

A dispatch that cannot acquire its pool's slot within `MaxWaitTime`
fails with `Reject(ResourceExhausted, "could not acquire slot in N")`
— this is the "you can't even get a slot" backoff. The "external
request pool" is the emergent set of all currently-waiting dispatches
across the gateway; it has no separate flat cap (which would couple
unrelated requests). Visibility comes from the per-pool metrics.

Every dispatch is timed by default. `gw.MetricsHandler()` exposes:

```
go_api_gateway_dispatch_duration_seconds{namespace,version,method,code}
go_api_gateway_pool_queue_dwell_seconds{namespace,version,method,kind}
go_api_gateway_pool_backoff_total{namespace,version,method,kind,reason}
go_api_gateway_pool_queue_depth{namespace,version,kind}            (gauge)
go_api_gateway_pool_streams_inflight{namespace,version}            (gauge)
go_api_gateway_subscribe_auth_total{namespace,version,method,code}
```

- `code` (dispatch) is `ok` on success, the gRPC status string on
  failure, or a `Reject` code when middleware short-circuits.
- `kind` is `unary` or `stream` — splits queries from subscriptions
  on the same backpressure metrics.
- `code` (subscribe_auth) is the `SubscribeAuthCode` enum
  (`SUBSCRIBE_AUTH_CODE_OK`, `..._TOO_OLD`, etc.).

Mount alongside the GraphQL endpoint:

```go
mux := http.NewServeMux()
mux.Handle("/graphql", gw.Handler())
mux.Handle("/schema", gw.SchemaHandler())
mux.Handle("/metrics", gw.MetricsHandler())
```

Override or disable:

```go
gw := gateway.New(gateway.WithoutMetrics())            // disable
gw := gateway.New(gateway.WithMetrics(myCustomSink))   // plug in your own
```

## Promotion path

Cross-cluster promotion is the combination of these tools:

1. Each cluster carries an `--environment` label (`dev`, `staging`,
   `prod`). The label becomes part of the NATS cluster name so two
   environments cannot accidentally federate.
2. `services list` exposes hashes; CI diffs the hash sets between two
   environments to confirm the bytes match for every `(ns, ver)` you
   intend to promote.
3. `/schema` exposes the SDL; `schema diff --strict` is the
   client-perspective gate — additions are fine, removals/required-arg
   changes fail the build.
4. The version system (multiple `vN` per namespace) lets you stage a
   new version alongside the old one, migrate clients gradually, then
   drain the old version.

Single-cluster drift is already prevented by the canonical hash gate
in the pool: a replica with a mismatched proto can't join an existing
`(ns, ver)` pool.

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

- External service discovery (Consul, etcd, k8s service watching). The
  built-in story is "services self-register over the gRPC control plane,
  and gateways share a JetStream KV registry"; bridge to other systems
  via the same control plane.
- Rolling deploy / hot reload. (Out of scope; gateway can be run
  blue/green like any other binary, or scaled by adding peers.)
- Multi-protocol ingest (OpenAPI, gRPC-Web, etc.). On the roadmap, not
  on day one.
- Observability backends. The middleware shape is the integration
  point; pick your tracer and write a five-line `Pair`.
- Kubernetes / docker-compose example deployments. The cluster code
  is the building block; packaging follows.

## License

MIT. See [LICENSE](./LICENSE).
