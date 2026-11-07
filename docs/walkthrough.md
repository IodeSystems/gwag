# Walkthrough: one registry, three client ecosystems

Clone the repo, run the example, and generate typed clients in
TypeScript (GraphQL and OpenAPI) and Go off the same live registry. Then edit a service,
re-codegen, and watch the change propagate without restarting the
gateway.

Time: 10-15 minutes if you have the codegen tools already installed.

## What you'll need

- Go 1.26+ (the `go` line in `go.mod`; to run the gateway and the example services)
- `curl` (to hit the schema endpoints)
- For codegen — install only the ecosystems you care about:
  - **TS / GraphQL**: `pnpm` (or `npm`) + `graphql-codegen` (pulled in via the example's package.json)
  - **Go / proto**: `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`
  - **TS / OpenAPI**: `pnpm` + `openapi-typescript` and `openapi-fetch`

You don't need all three. Pick the ones your team uses. Each is
independent — adding a new ecosystem is curl + one tool.

## Step 1: Boot the example

```bash
git clone https://github.com/iodesystems/gwag && cd gwag
cd examples/multi && ./run.sh
```

That brings up a gateway on `:8080` (HTTP) + `:50090` (control
plane), plus two example services that self-register over the
control plane:

- `greeter` — a proto service with a single `Hello` RPC.
- `library` — a proto service with `ListBooks` / `GetBook` /
  `AddBook` RPCs.

Both registered as proto, but the wedge below shows them re-emitted
in all three formats simultaneously.

In another terminal, sanity-check the consolidated GraphQL surface:

```bash
curl -s -X POST localhost:8080/api/graphql \
  -H 'Content-Type: application/json' \
  -d '{"query":"{ greeter { hello(name:\"world\") { greeting } } }"}'
# → {"data":{"greeter":{"hello":{"greeting":"Hello, world!"}}}}
```

Open `http://localhost:8080/` in a browser for the admin UI's
schema explorer. (The boot token is logged at gateway startup and
written to `/tmp/gwag-multi/admin-token`; paste it when prompted.)

## Step 2: Codegen three typed clients

### GraphQL → TypeScript (graphql-codegen)

Browser / mobile path. One SDL, typed React hooks (or Apollo Client
or urql — pick the flavor).

```bash
mkdir -p /tmp/walkthrough/ts && cd /tmp/walkthrough/ts
curl 'http://localhost:8080/api/schema/graphql' > schema.graphql

cat > codegen.yml <<'EOF'
schema: ./schema.graphql
documents: ./src/**/*.graphql
generates:
  src/gateway.ts:
    plugins:
      - typescript
      - typescript-operations
EOF

pnpm init -y
pnpm add -D @graphql-codegen/cli @graphql-codegen/typescript @graphql-codegen/typescript-operations
mkdir -p src && echo 'query Hello { greeter { hello(name: "world") { greeting } } }' > src/hello.graphql
pnpm exec graphql-codegen --config codegen.yml
# → src/gateway.ts contains typed Hello query + result types
```

Now your TS service code uses `HelloQuery` / `HelloQueryVariables`
from the generated file — type-safe, refactor-safe, IDE-aware.

### Proto → Go (protoc)

The Go service-to-service path. The export is a binary
FileDescriptorSet; `protoc` reads it directly.

```bash
mkdir -p /tmp/walkthrough/go/gen && cd /tmp/walkthrough/go
curl -fsS 'http://localhost:8080/api/schema/proto?service=greeter' -o greeter.fds

protoc --descriptor_set_in=greeter.fds \
  --go_out=gen --go-grpc_out=gen \
  --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative \
  greeter.proto
# → gen/greeter.pb.go + gen/greeter_grpc.pb.go
```

Call it through the gateway's gRPC ingress, which in `examples/multi`
shares the control-plane port (`:50090`, see `cmd/gateway/main.go`):

```go
conn, _ := grpc.NewClient("localhost:50090", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := greeterv1.NewGreeterServiceClient(conn)
resp, _ := client.Hello(ctx, &greeterv1.HelloRequest{Name: "world"})
// resp.Greeting == "Hello, world!"
```

The gateway translates the call through the IR layer to whichever
upstream serves the namespace. A runnable version with Connect, using
the repo's own module paths, is `examples/clients/go-proto/`.

### OpenAPI → TypeScript (openapi-typescript)

Typed REST calls from Node or the browser. The export wraps each spec
as `{"<ns>": {"<version>": <spec>}}`, so unwrap it first.

```bash
mkdir -p /tmp/walkthrough/oa && cd /tmp/walkthrough/oa
pnpm init && pnpm add openapi-fetch && pnpm add -D openapi-typescript
curl -fsS 'http://localhost:8080/api/schema/openapi?service=greeter' |
  node -e 'const w=JSON.parse(require("fs").readFileSync(0,"utf8"));
           process.stdout.write(JSON.stringify(Object.values(w)[0].v1))' > greeter.json
pnpm exec openapi-typescript greeter.json -o greeter.ts
```

```ts
import createClient from "openapi-fetch";
import type { paths } from "./greeter.ts";

const client = createClient<paths>({ baseUrl: "http://localhost:8080/api/ingress" });
const { data } = await client.POST("/greeter.v1.GreeterService/Hello", { body: { name: "world" } });
```

The gateway synthesizes OpenAPI for proto-origin services, so a REST
client works whatever the upstream speaks. The spec has no `servers`
entry; the base URL is the gateway's REST ingress. Both examples, with
pinned tool versions and a check script, are in `examples/clients/`;
see [`clients.md`](./clients.md).

## Step 3: Edit a service, re-codegen

```bash
# Add a field to greeter.proto:
$EDITOR examples/multi/protos/greeter.proto
# (add: string locale = 2; to HelloRequest)
```

Restart just the greeter service:

```bash
# run.sh runs all three processes in one terminal, so stop the greeter
# by PID (the compiled binary is named greeter), then re-run from
# examples/multi:
kill $(pgrep -x greeter)
go run ./cmd/greeter &
```

The greeter service re-registers over the control plane. The
gateway's schema rebuilds. **No gateway restart needed.**

Re-run any of the codegen steps from Step 2 — the new field is
there. Your TS / Go clients regenerate in seconds.

For the Go side specifically, you can verify without re-codegenning
first:

```bash
curl -s 'http://localhost:8080/api/schema/proto?service=greeter' | \
  protoc --decode_raw 2>&1 | grep -A1 'locale'
```

Edit once; every typed client picks it up on the next codegen.

## Step 4: Add a new service

Write a tiny self-registering service. The control-plane client
takes `.proto` bytes (or `fs.FS` for multi-file layouts). The weather
service below assumes a `weather.proto` next to it and a gRPC server
on `:50053`:

```go
package main

import (
    "context"
    _ "embed"
    "log"

    "github.com/iodesystems/gwag/v2/gw/controlclient"
)

//go:embed weather.proto
var weatherProto []byte

func main() {
    ctx := context.Background()
    // Blocks until the gateway is reachable and every Depends entry is ready.
    reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
        GatewayAddrs: []string{"localhost:50090"}, // list every node of a cluster
        ServiceAddr:  "localhost:50053",
        Services: []controlclient.Service{{
            Namespace:   "weather",
            Version:     "v1",
            ProtoSource: weatherProto,
            Owner:       &controlclient.Owner{Team: "weather", Contact: "#weather"},
        }},
        DrainOnSIGTERM: true, // drain, deregister, exit on SIGTERM
    })
    if err != nil {
        log.Fatal(err)
    }
    defer reg.Close(ctx)

    // ... start your gRPC server on :50053 ...
}
```

Dependencies, version lines, draining and a complete runnable service:
[`services.md`](./services.md).

The moment your service heartbeats in, `Query.weather` appears in
GraphQL, `?service=weather` works on all three schema endpoints,
and your codegen pipelines see it next time they run. No gateway
deploy, no static config edit, no schema review checkout.

Full lifecycle reference (version pinning, `@deprecated`
propagation, retirement): [`lifecycle.md`](./lifecycle.md).

## Step 5: When your upstream isn't proto

The example uses proto services because proto's the canonical
service-to-service format, but the gateway ingests OpenAPI and
downstream GraphQL on equal footing:

```go
// OpenAPI service — point at the spec + the upstream base URL.
gw.AddOpenAPI("./billing-openapi.json",
    gateway.As("billing"),
    gateway.To("https://billing.internal"))

// Downstream GraphQL — stitched into the consolidated schema with
// a namespace prefix.
gw.AddGraphQL("https://legacy-graphql.internal/graphql",
    gateway.As("legacy"))
```

Both produce the same three-format codegen surface. Your TS team
can run graphql-codegen against `?service=billing` even though
billing speaks OpenAPI 3.x upstream; the gateway translates.

Control-plane (self-register) variants exist for both — see
`ServiceBinding.openapi_spec` and `ServiceBinding.graphql_endpoint`
in `gw/proto/controlplane/v1/control.proto`.

Other docs cover operational concerns, auth, versioning, cluster,
pub/sub, embedded mode, and Federation positioning — see
[`../README.md`](../README.md) for the index.
