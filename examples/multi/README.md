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

**Parameter / header injection.** `cmd/gateway/main.go` registers two
`gw.Use(...)` injectors before services come in:

- `InjectPath("greeter.Hello.name", …, Hide(false), Nullable(true))`
  flips `name` to optional in the external schema and defaults it to
  the caller's IP when omitted (`X-Forwarded-For` → `X-Real-IP` →
  `RemoteAddr`). Send a value and it passes through.
- `InjectHeader("X-Source-IP", …)` stamps the caller's IP on every
  outbound dispatch — gRPC metadata for proto pools, HTTP header for
  OpenAPI sources. The greeter logs `x-source-ip=…` whenever the
  metadata lands.

Try it:

```
$ curl -sS -d '{"query":"{ greeter { hello { greeting } } }"}' \
       -H 'Content-Type: application/json' \
       -H 'X-Forwarded-For: 192.0.2.42' \
       http://localhost:8080/api/graphql
{"data":{"greeter":{"hello":{"greeting":"Hello, 192.0.2.42!"}}}}
```

The third flavor — hide-and-fill via `InjectType[*authpb.Context]` —
lives in [`examples/auth`](../auth) so the gateway here stays generic
(no service-specific proto imports).

## MCP integration (worked example)

The gateway publishes its registered GraphQL surface to LLM agents
via the Model Context Protocol over Streamable HTTP at `/mcp`,
gated by the admin bearer. Four tools land:

| Tool            | Purpose |
|---|---|
| `schema_list`   | Every op the operator's allowlist exposes (path + kind + namespace + first-line doc). |
| `schema_search` | Filter by dot-segmented path glob + regex over op name / arg names / doc body. |
| `schema_expand` | Full structured definition of one op or type, plus its transitive type closure. |
| `query`         | Execute a GraphQL operation in-process. Result wrapped as `ResponseWithEvents { response, events }`. |

The allowlist is operator-curated. `cmd/gateway/main.go` seeds the
example surface at boot:

```go
gateway.WithMCPInclude("greeter.**", "library.**", "admin.**")
```

Default-deny — agents only see what's explicitly included. Toggle
`auto_include=true` (via WithMCPAutoInclude or
`/api/admin/mcp/auto-include`) for "every public leaf minus the
exclude list" mode. Internal `_*` namespaces are filtered first
either way.

`./run.sh` persists the admin token to `/tmp/gwag-multi/admin-token`
(via `--admin-data-dir`). Then:

```
$ cd examples/multi
$ go run ./cmd/mcp-demo
using admin token from /tmp/gwag-multi/admin-token (64 hex chars)

--- step 1 — retune the MCP allowlist (idempotent — already seeded at boot) ---
  → POST /api/admin/mcp/include path=greeter.** ok
  → POST /api/admin/mcp/include path=library.** ok
  → MCPConfig: {"auto_include":false,"include":["greeter.**","library.**","admin.**"],"exclude":[]}

--- step 2 — open MCP client at http://localhost:8080/mcp ---
  → server: gwag 1.0.0

--- step 3 — tools/list ---
  → schema_list — List every operation exposed via the MCP surface…
  → schema_search — Filter the MCP-allowed operation surface.
  → schema_expand — Return the structured definition of one op or type…
  → query — Execute a GraphQL operation against the gateway in-process.

--- step 4 — schema_list ---
  [{"path":"greeter.hello", "kind":"Query", "namespace":"greeter", ...}, ...]
…
```

The demo chains all four tools: list → search → expand → query. See
[`cmd/mcp-demo/main.go`](./cmd/mcp-demo/main.go) for the full
sequence; it's a useful template for wiring your own MCP-aware agent
against the gateway.

Retune the allowlist at runtime via the admin OpenAPI routes:

```
$ TOKEN=$(cat /tmp/gwag-multi/admin-token)
$ curl -sS -H "Authorization: Bearer $TOKEN" \
       -H 'Content-Type: application/json' \
       -d '{"path":"greeter.**"}' \
       http://localhost:8080/api/admin/mcp/include
$ curl -sS -H "Authorization: Bearer $TOKEN" \
       -H 'Content-Type: application/json' \
       -d '{"auto_include":true}' \
       http://localhost:8080/api/admin/mcp/auto-include
$ curl -sS http://localhost:8080/api/admin/mcp   # always public for inspection
```

The same `admin_mcp_*` mutations are also surfaced as GraphQL fields
(dogfooded via the huma → OpenAPI → self-ingest path), so any GraphQL
client can curate the surface without separate REST calls.

Subscriptions are not exposed as MCP tools in v1.

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
typed gRPC servers and ship the raw `.proto` source bytes to the
gateway via `controlclient.Service.ProtoSource`). The gateway itself
imports nothing service-specific — it compiles the `.proto` via
`protocompile` at registration time (preserving comments) and
dispatches via `dynamicpb`.

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
- `Register` writes to the `gwag-registry` KV bucket; it does
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

For static registration without writing Go, the [`gwag`
binary](../../cmd/gwag) takes `--proto path=addr` flags. Use
that when service-discovery is out of scope and the proto + addr list
is known at deploy time.
