# Generating clients

Register a service once; generate a typed client from whichever export
fits the caller's language. Runnable versions of both examples below
live in [`examples/clients/`](../examples/clients).

## Export endpoints

| Endpoint | Returns | Feed it to |
|---|---|---|
| `GET /api/schema/graphql` | SDL text | graphql-codegen, Apollo, urql |
| `GET /api/schema/proto` | binary `FileDescriptorSet` (`?format=sdl` for `.proto` text in JSON) | `protoc`, buf, ts-proto |
| `GET /api/schema/openapi` | JSON `{"<ns>": {"<version>": <OpenAPI 3.x spec>}}` | openapi-typescript, openapi-generator |

`?service=ns[:ver][,ns[:ver]...]` narrows each export. Without it you
get every non-internal service. `/api/schema/openapi` also takes
`?openapi=3.0|3.1`.

The exports are open: no token needed. Hide a service from clients
with `AsInternal` (namespaces starting with `_` are never exported);
who may call it is an application concern.

Two properties of the exports that matter to generators:

- The proto export is binary, not `.proto` text. `protoc` reads it with
  `--descriptor_set_in`.
- The OpenAPI export wraps each spec in a namespace/version object.
  Unwrap it before handing it to a generator. It has no `servers`
  entry; the paths are the REST ingress paths, so set the base URL to
  `<gateway>/api/ingress` in the client.

## Proto to Go

[`examples/clients/go-proto/`](../examples/clients/go-proto) is a
package in the root module, so it needs no `go.mod` or `replace`, and
it uses grpc-go and connect-go, which are already dependencies.

`gen.sh` fetches the export and runs the repo's `protoc` toolchain
(see AGENTS.md, Proto Generation):

```bash
curl -fsS 'localhost:8080/api/schema/proto?service=greeter,library' -o gateway.fds
protoc --descriptor_set_in=gateway.fds \
  --go_out=gen --go-grpc_out=gen \
  --go_opt=module=$PKG/gen --go-grpc_opt=module=$PKG/gen \
  --go_opt=Mgreeter.proto=$PKG/gen/greeter/v1\;greeterv1 \
  --go-grpc_opt=Mgreeter.proto=$PKG/gen/greeter/v1\;greeterv1 \
  greeter.proto
```

The `M` options replace the `go_package` the service declared with a
path in your own module. The script does this for `library.proto` too.

Call through the gRPC ingress (in `examples/multi` it shares the
control-plane port, `:50090`):

```go
conn, _ := grpc.NewClient("localhost:50090",
    grpc.WithTransportCredentials(insecure.NewCredentials()))
greeter := greeterv1.NewGreeterServiceClient(conn)
resp, _ := greeter.Hello(ctx, &greeterv1.HelloRequest{Name: "grpc"})
```

Or through Connect at `/api/connect/`. The generated messages are
enough; no connect codegen plugin is needed:

```go
hello := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](
    http.DefaultClient,
    "http://localhost:8080/api/connect/greeter.v1.GreeterService/Hello",
    connect.WithProtoJSON())
resp, _ := hello.CallUnary(ctx, connect.NewRequest(&greeterv1.HelloRequest{Name: "connect"}))
```

The same call over GraphQL: `{ greeter { hello(name: "world") { greeting } } }`.

## OpenAPI to TypeScript

[`examples/clients/ts-openapi/`](../examples/clients/ts-openapi) uses
`openapi-typescript` for types and `openapi-fetch` for calls. The script
runs under Node 24 with no loader (`node src/main.ts`).

```bash
curl -fsS 'localhost:8080/api/schema/openapi?service=greeter' |
  node -e 'const w=JSON.parse(require("fs").readFileSync(0,"utf8"));
           process.stdout.write(JSON.stringify(Object.values(w)[0].v1))' > greeter.json
pnpm exec openapi-typescript greeter.json -o src/gen/greeter.ts
```

```ts
import createClient from "openapi-fetch";
import type { paths } from "./gen/greeter.ts";

const greeter = createClient<paths>({ baseUrl: "http://localhost:8080/api/ingress" });
const { data } = await greeter.POST("/greeter.v1.GreeterService/Hello", {
  body: { name: "openapi" },
});
console.log(data?.greeting);
```

Proto-origin services get a synthesized OpenAPI spec, so this works for
any registered service whatever it speaks upstream. proto3 fields have
no `required` in the spec, so every property is optional in the
generated types.

## Event code

`gwag gen` writes typed publish and listen code for event channels
declared with `(gwag.ps.v1.binding)`, in Go, TypeScript and Python. It
reads `.proto` files, not the gateway's exports, and runs the
generators your project pins. See [`events.md`](./events.md).

## Run the examples

```bash
examples/clients/check.sh
```

Boots the `examples/multi` gateway, greeter and library on ports in the
18480 / 50480 range, regenerates both clients, runs them, asserts the
output, and stops everything. Needs `protoc` with `protoc-gen-go` and
`protoc-gen-go-grpc`, Node 24, pnpm and curl. It is not part of any CI.
To run against your own gateway: `GATEWAY=http://host:port
examples/clients/go-proto/gen.sh`, then `pnpm gen` in `ts-openapi/`.

## Regenerating on schema change

Commit the generated code. When a service changes, rerun `gen.sh` and
review the diff. In CI, run `gwag schema diff --from <committed snapshot> --to <live
gateway> --strict` to fail the build when the schema changed in a way
that breaks existing callers, and regenerate only after that gate
passes. See
[`lifecycle.md`](./lifecycle.md) for the diff semantics and the tier
model.

## GraphQL to TypeScript

For browsers: fetch `/api/schema/graphql` and run graphql-codegen with
the `typescript` and `typescript-operations` plugins. A worked setup is
in [`walkthrough.md`](./walkthrough.md#step-2-codegen-three-typed-clients).
