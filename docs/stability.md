# Stability and SemVer

What 1.x **won't** break, what's allowed to change, and how to read
the per-symbol stability markers in the godoc.

The 1.0 tag will lock the surface described here. Subsequent 1.x
releases are allowed to add but not remove; "experimental" subpackages
are explicitly carved out from the promise.

## TL;DR

| Surface | 1.x promise | Where to look |
|---|---|---|
| Imported names in `github.com/iodesystems/gwag/gw` (default reflection path) | Stable. No renames / removals; new options are additive. | `// Stability: stable` in godoc |
| Imported names in `github.com/iodesystems/gwag/gw/ir` | Stable IR contract. Struct fields may be added; existing field semantics fixed. | `// Stability: stable` in godoc |
| Imported names in `github.com/iodesystems/gwag/gw/gat` | **Experimental.** Tier-2 work (proto ingest, `gwag serve`) may still reshape it. | `// Stability: experimental` in godoc |
| Control-plane proto wire (`controlplane.v1`) | Stable. Field numbers locked; new fields ride on unused tags. | `gw/proto/controlplane/v1/` |
| Admin huma surface (`/admin/*`) | Stable URLs + handlers, additive response fields | `gw/admin_huma.go` |
| MCP tool names and prompt shapes | Experimental. Reflection-driven; subject to schema-naming-policy churn | `gw/mcp_tools.go` |

If a symbol carries no `// Stability:` marker, treat it as
`experimental` — the marker is opt-in for the canonical surface,
not opt-out for everything else. The default if unsure: **assume
experimental, pin to a 1.x.y patch version, send a PR adding the
marker so future you isn't guessing.**

## Definitions

**Stable** — covered by SemVer. A 1.x → 1.y release will not:
1. Remove the symbol.
2. Rename the symbol.
3. Change its signature in a source-incompatible way.
4. Change documented behaviour in a way that breaks the existing
   contract.

Additions are allowed: new methods on a struct, new fields whose
zero value matches prior behaviour, new options. A new option is
*not* a breaking change even if it shifts a default in some lab
benchmark — defaults that aren't opt-out (no `WithoutX` mate) are
not part of the locked surface.

**Experimental** — outside SemVer. May change, rename, or vanish on
any 1.x release. Worth using; not worth wiring deeply into a
production deploy without an upgrade plan. Subpackages and feature
paths whose product spec is still moving live here on purpose.

**Internal** — not exported in 1.x. The `internal/` convention plus
lowercase identifiers do the enforcement. Don't go around it.

## What's stable

**Construction.** `gateway.New(opts ...Option)` plus every `Option`
constructor (`WithCluster`, `WithBackpressure`, `WithSubscriptionAuth`,
…) and `Without*` mate stay shape-compatible. Adding a new `With*`
option does not break callers. Renaming or removing one does — and
won't happen in 1.x.

**Registration entry points.** `(g *Gateway).AddProto`, `AddProtoFS`,
`AddProtoBytes`, `AddOpenAPI`, `AddOpenAPIBytes`, `AddGraphQL`. Each
takes the same shape across 1.x: `(spec, opts ...ServiceOption)`
where `ServiceOption` is the union of `As`, `AsInternal`,
`ForwardHeaders`, `MaxConcurrency`, `MaxConcurrencyPerInstance`,
`OpenAPIClient`, `ProtoImports`, `To`, `Version`. Additions OK;
removals will not happen.

**HTTP handlers.** Every `*Gateway` method returning an
`http.Handler` (`Handler()`, `IngressHandler()`, `SchemaHandler()`,
`SchemaProtoHandler()`, `SchemaOpenAPIHandler()`, `MetricsHandler()`,
`HealthHandler()`, `MCPHandler()`, `PprofMux()`) returns the same
contract: a handler you can mount under any prefix. The route table
in [`docs/architecture.md#http-routing-surface`](./architecture.md#http-routing-surface)
is the example wiring, not the constraint.

**Wire formats.** `controlplane.v1.ServiceBinding` field numbers are
locked. `proto_source` ships raw `.proto` bytes; `openapi_spec`
ships raw OpenAPI JSON/YAML bytes; both are parsed on the gateway
side using the same toolchain (`bufbuild/protocompile` with
`SourceInfoStandard`; `getkin/kin-openapi`). The earlier "ship a
compiled FileDescriptorSet" path was retired pre-1.0. A future kind
(e.g. WSDL) would land as a new oneof variant alongside, not as a
replacement.

**IR data shape.** Every type in `gw/ir` that represents the
intermediate representation (`Service`, `Operation`, `OperationGroup`,
`Type`, `Field`, `Arg`, `EnumValue`, `ChannelBinding`, `MapType`,
`TypeRef`) is stable in 1.x. Field additions land on unused
positions; field removals don't happen. The `Kind`, `OpKind`,
`TypeKind`, `ScalarKind` enums each carry a `*Unknown` zero variant
so a new variant is also additive.

**Ingest + render pipeline.** `IngestProto`, `IngestOpenAPI`,
`IngestGraphQL`, `RenderProtoFiles`, `RenderOpenAPI`, `RenderGraphQL`,
`RenderGraphQLRuntime`, `RenderGraphQLRuntimeFields`,
`PopulateSchemaIDs`, `MakeSchemaID`, `ParseSelectors`, `Filter`,
`HideInternal`, `Hides`. Same shape across 1.x; bug fixes that change
the *output* of these functions (e.g. SDL formatting) are not
considered API breaks — see the SDL note below.

**Middleware primitives.** `Transform.Schema` (`[]SchemaRewrite`),
`Transform.Runtime` (`Middleware`), `Handler` (`func(ctx, req)
(resp, error)`), `Middleware` (`func(next Handler) Handler`),
`InjectType[T]`, `InjectPath`, `InjectHeader`, `HideType[T]`,
`Hide`, `Nullable`. The `Transform.Headers` and `Transform.Inventory`
fields are intentionally unexported; construct via the factory funcs.

## What's allowed to change in 1.x

**SDL formatting and field ordering.** The `RenderGraphQL` /
`RenderGraphQLRuntime` output is a *valid* SDL / `graphql.Schema`,
not a stable byte-for-byte artifact. We may reorder fields, adjust
whitespace, or evolve description rendering. If your CI diffs SDL
against a golden file, expect drift on minor bumps.

**OpenAPI re-emit details.** `RenderOpenAPI` produces the OpenAPI 3.x
projection of an IR service. Identifier casing, summary text, and
auxiliary `x-*` extensions may evolve as the upstream OpenAPI tooling
matures. The `paths`/`components/schemas` shape (the parts an
actual client cares about) stays consistent.

**Proto FDS layout.** `RenderProtoFiles` projects each IR service
into a `FileDescriptorSet`. Service / message names are stable;
file paths inside the FDS are derived from namespace/version and may
be renormalised. Use the message FQN, not the file path, when
diff-ing.

**Metric label sets.** `go_api_gateway_*` Prometheus metrics carry
the labels documented in `docs/operations.md`. Adding a label is
additive (existing Grafana queries still work). Renaming or removing
a label is a 2.x change. New metrics (entirely new metric names) are
additive.

**Defaults.** Backpressure defaults, doc-cache sizes, NATS cluster
defaults: these are tuning knobs, not contracts. A 1.x release may
raise (or lower) a default if measurement shows it; if your deploy
depends on a specific number, set it explicitly via the
corresponding `With*` option.

**Boot-time logging.** Log line shapes from the gateway's startup
are not API. Anyone scraping startup logs to learn the token /
bind address should use `gw.AdminToken()` / `gw.AdminTokenHex()` /
the `<adminDataDir>/admin-token` file (set via `WithAdminDataDir`)
instead.

## What's experimental

**`gw/gat`** — the embedded GraphQL translator. The current API
captures the huma + GraphQL + connect-go-gRPC paired pattern, plus
the `gwag serve` / `RegisterHTTP` / proto-ingest paths. Treat gat
as the unstable testbed for "what if gwag were embedded?" — the
shape may shift on a minor.

**Codegen + plugin dispatch paths.** Not shipped. When they do,
they'll ride on the existing `DispatchRegistry` surface in `gw/ir`
plus a thin `gw.RegisterCodegen` / `gw.RegisterPlugin` entry.
Expected to land as `experimental` and graduate to `stable` after
one minor of shake-out.

**MCP tool naming and prompt shapes.** The `mcp__gwag__*` tool
names and the prompt patterns embedded in the MCP corpus depend on
the IR's identifier policy (lowerCamel for proto, identity
otherwise). If we change the policy — say, projecting proto field
names to `snake_case` — the tool names move with it. MCP integrators
who care about wire-format stability should pin to a 1.x.y patch.

**Caller-id token TTL semantics.** Today `ttlSeconds` arguments to
`SignCallerIDToken` / `SignSubscribeToken` are informational; the
gateway's `SkewWindow` bounds replay. A future version may pin
tokens to an explicit expiry rather than a wall-clock window. The
function signatures stay stable; the behavioural contract on
`ttlSeconds` is the experimental part.

## What's not part of the surface at all

**Internal packages and lowercase identifiers.** `gw/internal/...`
plus any lowercase function / type. Don't import via `unsafe`
reflection tricks; don't rely on observable side-effects of internal
state. These will change without notice.

**Tests, benchmarks, examples.** `gw/*_test.go`, `bench/*`,
`perf/*`, `examples/*`. Useful as templates, not as API. The
example wiring in `examples/multi/cmd/gateway/main.go` is the
canonical reference, but the wiring is example code, not a library
constraint.

**Build- or runtime-internal artifacts.** `bin/build`,
`bin/bench`, `bench/.run/`, `perf/.run/`. Tooling for maintainers
and adopters; their flags and layout are not part of the import
surface.

**Generated proto Go files.** `gw/proto/*/v1/*.pb.go` are
regenerated from the `.proto`. The Go types they emit are stable
*because* the underlying proto is stable; if you find yourself
diffing the generated file, diff the `.proto` instead.

## How to add a stability marker

Single line in the godoc, after the existing description:

```go
// WithBackpressure sets the per-pool concurrency policy. Pass the
// zero value to disable backpressure entirely.
//
// Stability: stable
func WithBackpressure(b BackpressureOptions) Option { … }
```

Two valid values: `stable` and `experimental`. No other text on
that line.

If you're adding a new exported symbol in a PR that touches the
canonical reflection path, mark it `stable`. Otherwise (gat,
codegen, plugin, new MCP shapes) mark it `experimental`.

## Versioning policy

This module follows SemVer. Pre-1.0 was "main is the truth, no
tags"; 1.0 onwards is annotated tags + `CHANGELOG.md`.

- **MAJOR** (`2.0.0`) — drops or renames stable symbols, changes
  wire format, or moves a stable symbol to experimental.
- **MINOR** (`1.y.0`) — adds stable symbols, promotes
  experimental → stable, adds wire fields on unused proto positions,
  adds metrics, raises defaults that don't require an opt-out.
- **PATCH** (`1.x.y`) — bug fixes, doc fixes, internal performance
  improvements. No surface change.

Experimental → stable promotion happens on a MINOR. Removing an
experimental symbol is also a MINOR (it was never under the SemVer
contract). Both should be noted in `CHANGELOG.md` under the
release's `Changed` section.
