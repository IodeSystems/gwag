# gat example — one huma source, three typed surfaces

This example shows how `gat` turns a plain [huma](https://huma.rocks/) HTTP service
into REST + GraphQL + gRPC simultaneously, with no extra ports and no
parallel schemas to keep in sync. Write your handlers once. Different
clients pick whichever surface fits them.

## Layout

```
examples/gat/
├── server/main.go     # huma + gat server (Go)
└── ui/                # React + Vite + graphql-codegen UI
```

## Run it

In one terminal — the Go server:

```sh
cd examples/gat
go run ./server                  # listens on :8080
```

In another — the UI (after `pnpm install` and `pnpm gen`):

```sh
cd examples/gat/ui
pnpm install
pnpm gen                         # codegen.ts hits localhost:8080/api/schema/graphql
pnpm dev                         # Vite on :5173, proxies /api to :8080
```

Open <http://localhost:5173>. The UI uses **typed** React hooks
generated from the live GraphQL schema. Edit `App.tsx` to ask for a
new field — TypeScript will reject the change until you run `pnpm gen`
again or the field exists on the server.

## The three surfaces

The server's `main.go` registers each operation **once** via
`gat.Register`:

```go
gat.Register(api, g, huma.Operation{
    OperationID: "getProject",
    Method:      http.MethodGet,
    Path:        "/projects/{id}",
}, getProject)  // func(ctx, *GetProjectInput) (*GetProjectOutput, error)
```

That single call surfaces the operation on all of:

| Surface  | URL                                                                     | Wire format       |
| -------- | ----------------------------------------------------------------------- | ----------------- |
| REST     | `GET /projects/{id}`                                                    | huma JSON         |
| GraphQL  | `POST /api/graphql` `{ projects { getProject(id: "alpha") { id name } } }` | GraphQL/JSON      |
| gRPC     | `POST /api/grpc/projects.v1.Service/getProject`                         | Connect/gRPC/Web  |

The wiring at the bottom of `main()`:

```go
gat.RegisterHuma(api, g, "/api")    // GraphQL + 3 schema views
gat.RegisterGRPC(mux, g, "/api/grpc") // connect-go handlers
```

Try them:

```sh
# REST
curl localhost:8080/projects/alpha

# GraphQL
curl -X POST localhost:8080/api/graphql -H 'Content-Type: application/json' \
  -d '{"query":"{ projects { getProject(id: \"alpha\") { id name tags } } }"}'

# gRPC via Connect-JSON
curl -X POST localhost:8080/api/grpc/projects.v1.Service/getProject \
  -H 'Content-Type: application/json' -d '{"id":"alpha"}'
```

## Schema endpoints (codegen-friendly)

| URL                       | Content                                |
| ------------------------- | -------------------------------------- |
| `/api/schema/graphql`     | SDL (default) or introspection JSON    |
| `/api/schema/proto`       | FileDescriptorSet (binary)             |
| `/api/schema/openapi`     | Re-emitted OpenAPI document            |
| `/openapi.json` (huma)    | Huma's own OpenAPI surface             |

Codegen consumers:

- **graphql-codegen** (this UI) points at `/api/schema/graphql`.
- **ts-proto / protobuf-es** point at `/api/schema/proto`.
- **openapi-typescript** points at huma's `/openapi.json` if you
  prefer the REST/OpenAPI codegen path for some clients.

You can mix and match: SPA UI on GraphQL for field selection,
service-to-service clients on gRPC for strict types and
cross-language reuse, external integrations on OpenAPI.

## Why this matters

If your service today is "huma + openapi-typescript on the UI," you
have one source of truth (huma) and one type-safe surface (the
OpenAPI codegen). The pain points come from OpenAPI itself:

- Flat `paths["/projects/{id}"]["get"]["responses"]` types are awkward to use.
- No field selection — list views ship every field including bodies.
- No multi-resource aggregation in one round-trip.
- No live schema validation against your written queries.

gat keeps your huma handlers as the source of truth and adds a typed
GraphQL surface alongside, so your UI keeps the strict types but
gains field selection, aggregation, and codegen-time query
validation. No second schema to maintain, no second server.

## Incremental adoption

You don't have to pair every operation at once. Use `gat.Register`
for the operations you want surfaced via GraphQL/gRPC; use plain
`huma.Register` for the rest. The unpaired ops keep working as REST
and stay out of gat's view.

```go
gat.Register(api, g, fancyOp, fancyHandler)        // GraphQL + gRPC + REST
huma.Register(api, plainOp, plainHandler)          // REST only — unchanged
```
