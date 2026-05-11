# Per-schema codegen spike

Informational sketch for Tier 2 workstream 3. **Don't ship.** This
file exists to make the perf gap concrete and to lock down the
generated-code surface that workstream 3's SDK + driver will target.

## Goal

Project the per-call savings of a reflection-free dispatcher; pin
down the artifact shape so workstream 3 starts with a known target.
Reflection stays the default forever (per Product priorities); this
is the opt-in upgrade path.

## Where reflection lives today

`BenchmarkProtoDispatcher_Dispatch` (post-pooling): **172 allocs/op
@ ~185µs**. The wrapper-side budget — what any codegen can plausibly
reclaim — line-items as:

| Source | per-call cost |
|---|---|
| `acquireDynamicMessage(input)` + `argsToMessage` (range fields, `protoreflect.Value`, list mutator path) | ~3 allocs (header + body + value boxes), reflection on every field |
| `chain(inner)` middleware closures | 0–N depending on user middleware (negligible empty) |
| `pickReplica` + per-instance sem | 0 allocs (atomics) |
| `r.conn.Invoke(ctx, method, req, resp)` — gRPC marshal+unmarshal of `dynamicpb.Message` | ~110 allocs (gRPC internals; protobuf marshal walks descriptors) |
| `messageToMap(resp)` (range fields, `protoreflect.Value` boxes, scalar→any conversions) | ~4 allocs + per-field reflection |
| `releaseDynamicMessage` (×2) | 0 |
| graphql-go executor (only in `_SchemaExec` bench) | the ~800-alloc tail; not addressable from codegen |

OpenAPI side (86 allocs/op @ ~128µs): URL substitution via
`fmt.Sprintf` + `strings.ReplaceAll` per path param, `url.Values`
build + Encode, `http.NewRequestWithContext`, `client.Do`,
`io.ReadAll`, `json.Unmarshal` to `any`. The JSON-decode-to-`any`
tail dominates and is also addressable.

The two reclaimable buckets are: (a) the dynamicpb / protoreflect
walk on each side of `grpc.ClientConn.Invoke`, replaced by direct
`*HelloRequest` / `*HelloResponse` (gRPC marshal still pays the same
proto-message cost, but on a generated message — same bytes either
way), and (b) the `messageToMap` / `json.Unmarshal(_, &any)` →
`map[string]any` shape-build on the response side.

## Artifact shape

One generated package per gateway build, exporting:

```go
// Package gendispatch is generated. Do not edit.
package gendispatch

import (
    "context"

    "github.com/iodesystems/gwag/gw/ir"
    "github.com/iodesystems/gwag/gw/sdk"
)

// Dispatchers returns the static dispatchers this build knows about,
// keyed by ir.SchemaID. Operator passes them into
// gateway.RegisterCodegen alongside the dynamic registrations.
//
// `deps` carries the per-call surface the generated code consumes:
// pickReplica, RecordDispatch, the per-pool/per-instance sems, the
// HTTP client lookup. Workstream 3 freezes that interface.
func Dispatchers(deps sdk.Deps) map[ir.SchemaID]ir.Dispatcher {
    return map[ir.SchemaID]ir.Dispatcher{
        "greeter/v1/hello":           &greeterV1HelloDispatcher{deps: deps},
        "petstore/v1/getPet":         &petstoreV1GetPetDispatcher{deps: deps},
        // ...
    }
}
```

Every entry is a concrete type, not a closure. Keeps the dispatcher
identity stable across schema rebuilds and lets the runtime swap it
into `DispatchRegistry` without re-allocating the field cache /
input descriptor on each rebuild.

## Worked example: proto `greeter.Hello`

Today: `protoDispatcher` with `inputDesc` + `handler`, walks
`fieldInfosFor(d.inputDesc)`, calls `setField` per arg, runs Invoke,
calls `messageToMap` on the response.

Generated:

```go
type greeterV1HelloDispatcher struct {
    deps sdk.Deps
    pool sdk.PoolHandle // resolved at first Dispatch via deps.Pool("greeter","v1")
}

func (d *greeterV1HelloDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
    // 1. Decode args directly — no fieldInfo loop, no protoreflect.
    var req greeterv1.HelloRequest
    if v, ok := args["name"]; ok {
        s, ok := v.(string)
        if !ok {
            return nil, sdk.ArgTypeError("name", "string", v)
        }
        req.Name = s
    }

    // 2. Backpressure + replica pick + Invoke. Same control flow as
    //    newProtoInvocationHandler, just with a typed conn so gRPC
    //    can use the generated Marshal/Unmarshal directly.
    pool := d.pool
    if pool == nil {
        pool = d.deps.Pool("greeter", "v1")
        d.pool = pool
    }
    r, releaseSvc, err := pool.AcquireUnary(ctx, "/greeter.GreeterService/Hello")
    if err != nil {
        return nil, err
    }
    defer releaseSvc()

    releaseInst, err := r.AcquireInstance(ctx, "/greeter.GreeterService/Hello")
    if err != nil {
        return nil, err
    }
    defer releaseInst()

    var resp greeterv1.HelloResponse
    start := d.deps.Now()
    err = r.Conn().Invoke(ctx, "/greeter.GreeterService/Hello", &req, &resp)
    d.deps.RecordDispatch("greeter", "v1", "/greeter.GreeterService/Hello", d.deps.Since(start), err)
    if err != nil {
        return nil, err
    }

    // 3. Encode response directly. No messageToMap walk — we know
    //    the field set at codegen time.
    return map[string]any{
        "greeting": resp.Greeting,
    }, nil
}
```

What disappears vs reflection path:

- `acquireDynamicMessage(input)` / `releaseDynamicMessage` (×2) →
  zero pool churn; `req`/`resp` are stack-allocated (gRPC may copy
  for marshal but the `dynamicpb.Message` + `protoreflect.Value`
  boxes are gone).
- `argsToMessage` field-info walk → straight type-asserted assigns,
  one per declared arg.
- `messageToMap` field-info walk → fixed map literal, sized at
  codegen time.
- `protoreflect.Value` boxing on every scalar (input + output) →
  gone; direct field access.

What stays:

- `grpc.ClientConn.Invoke` (~110 allocs of the 174 today) — same
  marshal cost on a generated message vs a `dynamicpb.Message`; the
  proto wire size is identical. Generated marshal *is* slightly
  faster than reflection-based but most of those 110 allocs are
  gRPC stream / metadata / bufpool plumbing, not message walking.
- Backpressure metric paths (`Queueing.Add(±1)`, `RecordDispatch`)
  — already cheap, not worth specializing.
- The map[string]any return shape — graphql-go's executor still
  consumes a map. (Future: a typed-resolver path could skip even
  this, but that's a graphql-go-side change, not codegen.)

Realistic projection: **~25–30 allocs reclaimed per call, ~10–15µs
saved.** Not a 10× win — the gRPC client's own bookkeeping is most
of the floor. The `_SchemaExec` bench will see a smaller relative
delta because the graphql-go executor is the long pole there.

## Worked example: OpenAPI `petstore.getPet`

Today: `dispatchOpenAPI` reads `op.Parameters` per call, builds
`url.Values`, calls `fmt.Sprintf("%v", v)` per param, JSON-marshals
the body if any, decodes the response into `any` via
`json.Unmarshal`.

Generated (`GET /pet/{petId}`, query `expand`, no body):

```go
type petstoreV1GetPetDispatcher struct {
    deps sdk.Deps
    src  sdk.OpenAPISourceHandle
}

func (d *petstoreV1GetPetDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
    if d.src == nil {
        d.src = d.deps.OpenAPISource("petstore", "v1")
    }

    // Build URL. Path placeholders + declared query params are
    // known at codegen time — no fmt.Sprintf, no url.Values map.
    var pathBuf [64]byte
    path := append(pathBuf[:0], "/pet/"...)
    if v, ok := args["petId"]; ok {
        path = sdk.AppendPathParam(path, v) // type-switched, escaped
    }

    var qb sdk.QueryBuilder
    if v, ok := args["expand"]; ok {
        qb.AddString("expand", v)
    }

    // Request build, dispatch, decode. Decode goes into the typed
    // response (HumaPet struct generated from the OpenAPI schema)
    // and walks once into the canonical map shape — no
    // intermediate any tree.
    r, release, err := d.src.AcquireUnary(ctx, "GET /pet/{petId}")
    if err != nil {
        return nil, err
    }
    defer release()

    req, err := sdk.NewRequest(ctx, "GET", r.BaseURL(), path, qb.Encode(), nil)
    if err != nil {
        return nil, err
    }
    sdk.ForwardHeaders(ctx, req, d.src.ForwardHeaders())
    req.Header.Set("Accept", "application/json")

    start := d.deps.Now()
    resp, err := r.Client().Do(req)
    d.deps.RecordDispatch("petstore", "v1", "GET /pet/{petId}", d.deps.Since(start), err)
    if err != nil {
        return nil, sdk.OpenAPIRequestError(err)
    }
    defer resp.Body.Close()

    var out petv1.Pet
    if err := sdk.DecodeJSON(resp, &out); err != nil {
        return nil, err
    }
    return map[string]any{
        "id":   out.ID,
        "name": out.Name,
        // ...
    }, nil
}
```

Reclaimed: `url.Values` map + `Encode`, the `fmt.Sprintf("%v", v)`
per param, `json.Unmarshal(bytes, &any)` walking into a recursive
`map[string]any`. The generated path decodes directly into a typed
struct once, then projects to the canonical shape.

Projection: **~20 allocs reclaimed, ~15–20µs saved.** OpenAPI's
floor is `http.Client.Do` (TCP + TLS handling, response buffering)
which we don't address.

## SDK contract (input to workstream 3)

The generated code reaches into the gateway via a stable, minimal
interface. Workstream 3's first todo is to lock this down; the spike
proposes:

```go
package sdk

type Deps interface {
    Pool(namespace, version string) PoolHandle
    OpenAPISource(namespace, version string) OpenAPISourceHandle
    RecordDispatch(namespace, version, label string, dur time.Duration, err error)
    Now() time.Time
    Since(start time.Time) time.Duration
}

type PoolHandle interface {
    AcquireUnary(ctx context.Context, label string) (Replica, func(), error)
    // server-streaming: Subscribe(ctx, label) (<-chan proto.Message, func(), error)
}

type Replica interface {
    Conn() grpc.ClientConnInterface
    AcquireInstance(ctx context.Context, label string) (func(), error)
}

type OpenAPISourceHandle interface {
    AcquireUnary(ctx context.Context, label string) (OpenAPIReplica, func(), error)
    ForwardHeaders() []string
}

type OpenAPIReplica interface {
    Client() *http.Client
    BaseURL() string
}

// Helpers: ArgTypeError, AppendPathParam, QueryBuilder, NewRequest,
// ForwardHeaders, DecodeJSON, OpenAPIRequestError. All are
// non-generic, alloc-conscious, and exported with bench coverage.
```

Boundaries:

- The SDK is purely the *call* surface. Schema build / IR walking
  / registry mutation stay in `gw` proper; codegen only consumes
  the per-op artifacts.
- No `proto.Message` / `protoreflect.Descriptor` ever crosses the
  SDK boundary on the hot path. Generated code holds typed
  `*greeterv1.HelloRequest` directly.
- `grpc.ClientConnInterface` is the one external dependency that
  leaks through `Replica.Conn()`. Acceptable — it's stable, and
  the alternative is wrapping every gRPC method twice.
- Versioned via `gw/sdk` package version (Go semver on the module
  if/when this stabilizes). The codegen template emits a
  `// generated for sdk vX.Y` header; mismatch fails fast at load.

## Registration & precedence

```go
// In the operator's main.go, alongside AddProto / AddOpenAPI:
gw.RegisterCodegen(gendispatch.Dispatchers(gw.SDK()))
```

`RegisterCodegen` slots into `*ir.DispatchRegistry`. Resolution
order at schema rebuild:

1. **Codegen entry** — `Dispatchers()[id]` exists → use it.
2. **Reflection entry** — fall through to the dispatcher created by
   `proto_register` / `openapi_register` from the live pool/source.

Per-service fallback, not all-or-nothing. A gateway that registers a
proto service via the control plane (no codegen) and another via
`AddProto` (with codegen) gets both — the unknown one falls back to
reflection automatically. The control-plane reconciler doesn't need
to know codegen exists.

## What stays reflection forever

- **Downstream GraphQL ingest (`AddGraphQL`).** The forwarding
  resolver re-emits the inbound selection set as a string and
  forwards it; codegen has nothing to specialize. Mirror types are
  build-time work, not hot-path.
- **Dynamic registrations via control plane.** Service set unknown
  at build time. The plugin supervisor (workstream 4) bridges this
  with the same artifact, runtime-compiled.
- **Hide / Inject middleware.** Operates on canonical args by name
  and mutates a `map[string]any`. Generated code receives
  pre-mutated args; injection happens before Dispatch. No change.

## Open questions surfaced by the spike

1. **List + nested-object decode.** The HelloRequest example is
   scalar-only. `[]any → []*FooMessage` and
   `map[string]any → *NestedMessage` need helpers in the SDK and a
   recursive emit in the codegen template. Sketch but don't bench.
2. **Oneofs.** Proto oneof discriminator → graphql union. Today's
   `setField` handles this via descriptor walking; generated code
   needs a switch on the oneof variant arg names. Tractable, ~20
   lines per oneof.
3. **Default values.** `gw/diff.go` recently grew to catch default
   transitions; codegen needs to respect IR `Arg.Default` so the
   generated decoder applies defaults the same way the reflection
   path does. Open: do we apply defaults pre- or post-injection?
4. **Generated message dependency.** The codegen output imports the
   operator's `*.pb.go`. Means the codegen driver needs the same
   import paths the operator's `go.mod` uses — straightforward for
   `AddProtoDescriptor(greeterv1.File_greeter_proto)` (the import
   path is right there) but trickier for control-plane–only
   bindings. Plugin supervisor will need to compile against the
   same module graph; defer that to workstream 4.
5. **Stable `RegisterCodegen` semantics under hot-reload.** If the
   registry rebuilds (pool churn) but the codegen entry hasn't
   changed, we should reuse the dispatcher pointer. Add a
   `Replace(id, d)` op vs `Set(id, d)` to make the distinction
   explicit.
6. **Subscriptions.** Server-streaming codegen is more involved
   (the dispatcher returns `<-chan any`, not `(any, error)`). The
   reflection path's `protoSubscriptionDispatcher` currently goes
   straight to NATS pub/sub for fan-out; codegen's win is on the
   per-message encode/decode of streamed events. Sketch parity, no
   bench.

## What workstream 3 inherits from this spike

- **SDK surface** (above) — freeze before writing the driver.
- **Generated artifact shape** — one type per op, package-level
  `Dispatchers(deps) map[ir.SchemaID]ir.Dispatcher`.
- **Precedence rule** — codegen entries shadow reflection entries
  per-id; everything else falls through.
- **Realistic perf budget** — proto ~25–30 allocs / ~10–15µs;
  OpenAPI ~20 allocs / ~15–20µs. Worth doing, but not a
  reflection→codegen step-function. Frame opt-in materials around
  "remove gateway-side overhead" rather than "make it fast."

## What this spike intentionally does not do

- Build a working codegen driver (workstream 3, ~3-4 days).
- Modify any runtime code (the existing dispatchers are unchanged).
- Bench anything beyond the projections above; the actual
  benchmark belongs in workstream 3 alongside the worked example.
- Define the plugin supervisor's compile/load loop (workstream 4).
