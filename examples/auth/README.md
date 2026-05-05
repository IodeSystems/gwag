# examples/auth

The canonical case the library API is shaped around: hide auth fields
globally, fill them from a registered auth service, hide that service
from the external schema.

## What's here

- `protos/auth.proto` — `AuthService.Resolve(token) → Context`.
- `protos/user.proto` — `UserService.GetMe(GetMeRequest)` where
  `GetMeRequest` embeds `auth.v1.Context`.
- `gen/` — generated Go bindings (`protoc-gen-go` +
  `protoc-gen-go-grpc`).
- `main.go` — wires both services in-process via bufconn, configures
  the gateway, declares `HideAndInject[*authv1.Context]`.

## What works today

`go build ./...` and `go vet ./...` pass: the example compiles against
the API surface in [`../../gateway.go`](../../gateway.go).

`go run .` exits early at the dispatch line with a TODO message,
because the library implementation behind the API is pending. The
example is the *design pin*: it locks the public API in place, and
implementing the library means making this `main.go` actually serve
GraphQL without changing a line above the `gw.Handler()` call.

## What it would do once the library is implemented

```
$ curl -H 'Authorization: Bearer alice' \
       -d '{"query": "query { user { getMe { id name tenantId } } }"}' \
       http://localhost:8080/graphql

{"data":{"user":{"getMe":{"id":"u_alice","name":"Demo User","tenantId":"t_demo"}}}}
```

External schema introspection would *not* show:

- the `auth` namespace (registered as internal — planned via `AsInternal()`)
- the `auth` field on `GetMeRequest` (stripped by `HideAndInject`'s schema half)
- the `Context` type (no public fields reference it)

Every `GetMe` request would trigger one `AuthService.Resolve` call,
cached on the request context. Multiple RPCs in one GraphQL query
share the resolution.

## Regenerating bindings

```
protoc -I protos \
  --go_out=. --go_opt=module=github.com/iodesystems/go-api-gateway/examples/auth \
  --go-grpc_out=. --go-grpc_opt=module=github.com/iodesystems/go-api-gateway/examples/auth \
  protos/auth.proto protos/user.proto
```

Requires `protoc-gen-go` and `protoc-gen-go-grpc` on `PATH`.
