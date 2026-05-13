# gw/ Public-API Audit — pre-v1

Survey of every exported symbol in `github.com/iodesystems/gwag/gw`.
Goal: decide what to lock for v1.x semver, what to move to `gw/internal/...` or unexport.

External consumers checked:
- `examples/multi/cmd/gateway/` + `examples/auth/` + `examples/sign/`
- `gw/cmd/gwag/` (main.go + up.go)
- `gw/gat/` — confirmed does **not** import `gw`; only `gw/ir`
- `examples/gat/server/` — imports `gw/gat`, not `gw` directly

---

## Summary

| Classification | Count |
|---|---|
| Public | 94 |
| Internal | 25 |
| Keep-Public-Pending-Review | 8 |
| Deprecated | 0 |

*Counts revised on review: `Handler` + `Middleware` moved Public → Internal call was wrong (docs/middleware.md shows them in adopter-visible signatures).*

**Top 10 highest-confidence Internal candidates ("easy wins"):**

1. `BackpressureConfig` / `BackpressureMiddleware` — internal plumbing; zero external callers.
2. `HideTypeRewrite` / `HidePathRewrite` / `NullableTypeRewrite` / `NullablePathRewrite` — concrete rewrite structs; type-asserted inside `gw/` only; adopters use `HideType[T]()`, `InjectType`, `InjectPath`.
3. `HeaderInjector` — internal dispatch struct embedded in `Transform.Headers`; type-asserted inside `gw/` only.
4. `WithAdminAuth` / `IsAdminAuth` — internal middleware helpers used only inside `gw/auth_admin.go`.
5. ~~`Handler` / `Middleware`~~ — re-classified Public on review: `docs/middleware.md` documents the manual `Transform{Runtime: mw}` pattern that requires both types.
6. `StatsSnapshot` / `MethodStatsSnapshot` / `HistoryBucket` / `ServiceHistory` — return types of admin-only methods; wire types for the admin HTTP surface.
7. `SubjectInfo` — return type of `ActiveSubjects()`; only used inside `admin_huma.go`.
8. `SchemaExpand*` / `SchemaList*` / `SchemaSearch*` types (8 structs) — huma wire types for admin MCP endpoints; exposed via methods but not useful standalone.
9. `MCPQueryInput` / `MCPResponseWithEvents` / `MCPEventsBundle` / `MCPChannelEvent` — huma wire shapes for `MCPQuery`; not useful outside admin surface.
10. `InjectorEntry` / `InjectorFrame` / `InjectorLanding` / `InjectorRecord` + `InjectorKind` / `InjectorState` — inventory wire types surfaced only through `InjectorInventory()` inside `admin_huma.go`.

---

## Symbol Table

Methods are grouped under their type. Relative file paths from repo root.

### Constants

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `DelegatedCallerIDTokenHeader` | const | `gw/caller_id_delegated.go` | — | Public | Wire field name; clients need it to set the header. |
| `DelegatedCallerIDTokenMetadata` | const | `gw/caller_id_delegated.go` | — | Public | gRPC metadata key companion to above. |
| `HMACCallerIDTimestampHeader` | const | `gw/caller_id_hmac.go` | — | Public | Wire field name for HMAC caller-id flow. |
| `HMACCallerIDKidHeader` | const | `gw/caller_id_hmac.go` | — | Public | Kid header for key rotation. |
| `HMACCallerIDSignatureHeader` | const | `gw/caller_id_hmac.go` | — | Public | Signature header. |
| `HMACCallerIDTimestampMetadata` | const | `gw/caller_id_hmac.go` | — | Public | gRPC metadata twin. |
| `HMACCallerIDKidMetadata` | const | `gw/caller_id_hmac.go` | — | Public | gRPC metadata twin. |
| `HMACCallerIDSignatureMetadata` | const | `gw/caller_id_hmac.go` | — | Public | gRPC metadata twin. |
| `OtherCallerID` | const | `gw/caller_id.go` | — | Public | Overflow bucket label; operators scrape it from Prometheus. |
| `PublicCallerIDHeader` | const | `gw/caller_id.go` | — | Public | Header read by `WithCallerIDPublic`. |
| `PublicCallerIDMetadata` | const | `gw/caller_id.go` | — | Public | gRPC metadata twin. |
| `SchemaOpQuery` / `SchemaOpMutation` / `SchemaOpSubscription` | const | `gw/mcp_tools.go` | — | Internal | Stable string values used only inside `admin_huma.go` / MCP handlers; not useful to adopters. |
| `CodeUnauthenticated` | const | `gw/gateway.go` | `examples/auth` | Public | Used in `Reject()` calls in adopter code. |
| `CodePermissionDenied` | const | `gw/gateway.go` | — | Public | Part of the `Code` enum; lock for v1. |
| `CodeResourceExhausted` | const | `gw/gateway.go` | — | Public | Part of the `Code` enum. |
| `CodeInvalidArgument` | const | `gw/gateway.go` | — | Public | Part of the `Code` enum. |
| `CodeNotFound` | const | `gw/gateway.go` | — | Public | Part of the `Code` enum. |
| `CodeInternal` | const | `gw/gateway.go` | — | Public | Part of the `Code` enum. |
| `ChannelAuthOpen` / `ChannelAuthHMAC` / `ChannelAuthDelegate` | const | `gw/auth_channel.go` | — | Public | Required arguments to `WithChannelAuth`; documented in pubsub.md. |
| `InjectorKindType` / `InjectorKindPath` / `InjectorKindHeader` | const | `gw/inject_inventory.go` | — | Internal | String labels inside inventory wire type; no adopter need. |
| `InjectorStateActive` / `InjectorStateDormant` | const | `gw/inject_inventory.go` | — | Internal | Wire labels for admin inventory endpoint only. |

### Variables

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `DefaultBackpressure` | var | `gw/gateway.go` | `gw/cmd/gwag` | Public | Used as flag defaults in gwag binary. |
| `NewIRTypeBuilder` | var | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Re-export from `gw/ir`; used internally only today; see boundary cases. |

### Top-level functions

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `BackpressureMiddleware` | func | `gw/backpressure.go` | — | Internal | Internal pool/dispatcher wiring; zero external callers. |
| `HTTPRequestFromContext` | func | `gw/gateway.go` | `examples/multi`, `examples/sign` | Public | Canonical way to get inbound HTTP request in resolvers. |
| `HideType[T]` | func | `gw/transforms.go` | — | Public | Primary schema-rewrite API; used indirectly via `InjectType`. |
| `InjectHeader` | func | `gw/inject.go` | `examples/multi`, `gw/cmd/gwag` | Public | Core injector API. |
| `InjectPath` | func | `gw/inject.go` | `examples/multi`, `gw/cmd/gwag` | Public | Core injector API. |
| `InjectType[T]` | func | `gw/inject.go` | `examples/auth` | Public | Core injector API. |
| `IsAdminAuth` | func | `gw/auth_admin.go` | — | Internal | Only used inside `gw/`; not needed by adopter middleware. |
| `LoadMTLSConfig` | func | `gw/cluster.go` | `examples/multi`, `gw/cmd/gwag` | Public | Wiring helper for mTLS. |
| `New` | func | `gw/gateway.go` | all examples, `gw/cmd/gwag` | Public | Primary constructor. |
| `PublishCallerRevoked` | func | `gw/caller_id_delegated.go` | — | Public | Delegate-side revocation helper; part of the Delegated caller-id API. |
| `Reject` | func | `gw/gateway.go` | `examples/auth`, `gw/cmd/gwag` | Public | Resolver error constructor; used in adopter code. |
| `RenderGraphQLRuntime` | func | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Re-export; only used inside `gw/`; see boundary cases. |
| `RenderGraphQLRuntimeFields` | func | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Same as above. |
| `SignCallerIDToken` | func | `gw/caller_id_hmac.go` | — | Public | Client-side HMAC signer; symmetric with gateway verifier. |
| `SignCallerIDTokenWithKid` | func | `gw/caller_id_hmac.go` | — | Public | Key-rotation-aware signer. |
| `SignSubscribeToken` | func | `gw/auth_subscribe.go` | — | Public | Client-side subscription token minter. |
| `SignSubscribeTokenWithKid` | func | `gw/auth_subscribe.go` | `examples/multi`, `gw/cmd/gwag` | Public | Key-rotation-aware minter; used in examples. |
| `UIHandler` | func | `gw/ui_handler.go` | `examples/multi`, `gw/cmd/gwag` | Public | SPA serving helper; canonical wiring reference uses it. |
| `WithAdminAuth` | func | `gw/auth_admin.go` | — | Internal | Only stamped onto ctx inside `AdminMiddleware`; no adopter need. |
| `WithHTTPRequest` | func | `gw/gateway.go` | — | Internal | Only called inside gateway's own HTTP ingress handler. |
| `Hide` | func | `gw/inject.go` | `examples/multi`, `gw/cmd/gwag` | Public | `InjectOption` constructor; used in wiring. |
| `Nullable` | func | `gw/inject.go` | `gw/cmd/gwag` | Public | `InjectOption` constructor. |

### Option functions (`func(*config)` — all in `gw/gateway.go` unless noted)

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `WithAdminDataDir` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Admin token persistence directory. |
| `WithAdminToken` | func | `gw/gateway.go` | — | Public | Explicit admin token override; documented boot option. |
| `WithAllowTier` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Version-tier policy gate; core config. |
| `WithBackpressure` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Concurrency/backpressure tuning. |
| `WithCallerHeaders` | func | `gw/gateway.go` | — | Public | Forwarded header allowlist for caller-id extraction. |
| `WithCallerIDDelegated` | func | `gw/caller_id_delegated.go` | — | Public | Delegated caller-id option. |
| `WithCallerIDEnforce` | func | `gw/caller_id.go` | — | Public | Fail-closed mode for caller-id. |
| `WithCallerIDExtractor` | func | `gw/caller_id.go` | — | Public | Custom extractor seam; part of the planned caller-id ladder. |
| `WithCallerIDHMAC` | func | `gw/caller_id_hmac.go` | — | Public | HMAC caller-id option. |
| `WithCallerIDMetricsTopK` | func | `gw/caller_id.go` | — | Public | Label-cardinality cap for Prometheus metrics. |
| `WithCallerIDPublic` | func | `gw/caller_id.go` | — | Public | Plain-header caller-id option. |
| `WithChannelAuth` | func | `gw/auth_channel.go` | — | Public | Per-pattern pub/sub auth tier; used in docs/pubsub.md examples. |
| `WithChannelBinding` | func | `gw/channel_bindings.go` | — | Public | Channel-to-proto-message binding. |
| `WithChannelBindingEnforce` | func | `gw/channel_bindings.go` | — | Public | Strict-shape enforcement option. |
| `WithCluster` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Cluster attachment. |
| `WithDocCacheMaxQueryBytes` | func | `gw/gateway.go` | — | Public | Tuning option for doc cache. |
| `WithDocCacheSize` | func | `gw/gateway.go` | — | Public | Tuning option. |
| `WithDocNormalization` | func | `gw/gateway.go` | — | Public | Query normalization toggle. |
| `WithGRPCConnPoolSize` | func | `gw/gateway.go` | — | Public | gRPC connection pool size. |
| `WithMetrics` | func | `gw/gateway.go` | — | Public | Metrics delegate injection point. |
| `WithOpenAPIClient` | func | `gw/gateway.go` | — | Public | Gateway-wide HTTP client override. |
| `WithPprof` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | pprof enable option. |
| `WithQuota` | func | `gw/quota.go` | — | Public | Per-caller quota gate option. |
| `WithQuotaEnforce` | func | `gw/quota.go` | — | Public | Fail-closed quota mode. |
| `WithRequestLog` | func | `gw/gateway.go` | — | Public | Per-request JSON log writer. |
| `WithSignerSecret` | func | `gw/gateway.go` | `examples/multi`, `examples/sign`, `gw/cmd/gwag` | Public | gRPC sign endpoint bearer. |
| `WithStrictPayloadTypes` | func | `gw/gateway.go` | — | Public | Strict payload-type enforcement. |
| `WithSubscriptionAuth` | func | `gw/gateway.go` | `examples/multi`, `examples/sign` | Public | HMAC subscription auth config. |
| `WithTLS` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | mTLS for outbound gRPC. |
| `WithoutBackpressure` | func | `gw/gateway.go` | — | Public | Dev/test convenience. |
| `WithoutDocCache` | func | `gw/gateway.go` | — | Public | Memory-constrained config. |
| `WithoutGraphiQL` | func | `gw/gateway.go` | — | Public | Disable GraphiQL UI. |
| `WithoutMetrics` | func | `gw/gateway.go` | — | Public | Metrics-free mode. |
| `WithoutSubscriptionAuth` | func | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Insecure dev mode. |

### ServiceOption functions (`func(*serviceConfig)` — all in `gw/gateway.go` unless noted)

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `As` | func | `gw/gateway.go` | `examples/multi`, `examples/auth`, `gw/cmd/gwag` | Public | Namespace override; core service registration. |
| `AsInternal` | func | `gw/gateway.go` | `examples/auth`, `examples/multi` | Public | Hide-from-schema option. |
| `ForwardHeaders` | func | `gw/gateway.go` | — | Public | Per-source header forward allowlist. |
| `MaxConcurrency` | func | `gw/gateway.go` | — | Public | Per-service concurrency cap. |
| `MaxConcurrencyPerInstance` | func | `gw/gateway.go` | — | Public | Per-replica concurrency cap. |
| `OpenAPIClient` | func | `gw/gateway.go` | — | Public | Per-source HTTP client override. |
| `ProtoImports` | func | `gw/gateway.go` | — | Public | Multi-file proto import map. |
| `To` | func | `gw/gateway.go` | `examples/multi`, `examples/auth`, `examples/sign`, `gw/cmd/gwag` | Public | gRPC destination wiring. |
| `Version` | func | `gw/gateway.go` | — | Public | Registration version pin. |

### Types

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `AllowedTiers` | type | `gw/gateway.go` | — | Public | Struct embedded in gateway config; needed for future extension. |
| `BackpressureConfig` | type | `gw/backpressure.go` | — | Internal | Internal per-dispatcher knob bag; only constructed inside `gw/`; `BackpressureOptions` is the public API. |
| `BackpressureOptions` | type | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Public config struct; used in examples. |
| `CallerIDDelegatedOptions` | type | `gw/caller_id_delegated.go` | — | Public | Option struct for `WithCallerIDDelegated`. |
| `CallerIDExtractor` | type | `gw/caller_id.go` | — | Public | Custom extractor function type; part of the seam. |
| `CallerIDHMACOptions` | type | `gw/caller_id_hmac.go` | — | Public | Option struct for `WithCallerIDHMAC`. |
| `ChannelAuthTier` | type | `gw/auth_channel.go` | — | Public | Enum type for `WithChannelAuth`; needed alongside constants. |
| `Cluster` | type | `gw/cluster.go` | `examples/multi`, `gw/cmd/gwag` | Public | Embedded NATS cluster handle. |
| `ClusterOptions` | type | `gw/cluster.go` | `examples/multi`, `gw/cmd/gwag` | Public | Config struct for `StartCluster`. |
| `Code` | type | `gw/gateway.go` | `examples/auth` | Public | Error code enum used with `Reject`. |
| `Gateway` | type | `gw/gateway.go` | all examples, `gw/cmd/gwag` | Public | Primary library type. |
| `Handler` | type | `gw/gateway.go` | docs/middleware.md | Public | Function-type for `Transform.Runtime` middleware chain; `docs/middleware.md` shows adopters typing `func(next gateway.Handler) gateway.Handler {...}` directly. |
| `HeaderInjector` | type | `gw/gateway.go` | — | Internal | Internal dispatch struct; type-asserted inside `gw/` only; in `Transform.Headers` but adopters never construct it directly. |
| `HidePathRewrite` | type | `gw/transforms.go` | — | Internal | Concrete rewrite returned by `InjectPath`; type-asserted inside `gw/`; adopters use `InjectPath`. |
| `HideTypeRewrite` | type | `gw/transforms.go` | — | Internal | Concrete rewrite returned by `HideType[T]`; same pattern. |
| `HistoryBucket` | type | `gw/stats.go` | — | Internal | Return type of `History()`; only consumed by `admin_huma.go`. |
| `InjectOption` | type | `gw/inject.go` | — | Public | Interface for `Hide`/`Nullable`; adopters pass these to `InjectType`/`InjectPath`/`InjectHeader`. |
| `InjectorEntry` | type | `gw/inject_inventory.go` | — | Internal | Return type of `InjectorInventory()`; only consumed by `admin_huma.go`. |
| `InjectorFrame` | type | `gw/inject_inventory.go` | — | Internal | Inventory wire type; `admin_huma.go` only. |
| `InjectorKind` | type | `gw/inject_inventory.go` | — | Internal | Inventory wire type. |
| `InjectorLanding` | type | `gw/inject_inventory.go` | — | Internal | Inventory wire type. |
| `InjectorRecord` | type | `gw/inject_inventory.go` | — | Internal | Embedded in `Transform.Inventory`; only read inside `gw/`. |
| `InjectorState` | type | `gw/inject_inventory.go` | — | Internal | Inventory wire type. |
| `InternalProtoHandler` | type | `gw/internal_proto.go` | — | Keep-Public-Pending-Review | See boundary cases. |
| `InternalProtoSubscriptionHandler` | type | `gw/internal_proto.go` | — | Keep-Public-Pending-Review | See boundary cases. |
| `IRTypeBuilder` | type | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Re-export alias from `gw/ir`; see boundary cases. |
| `IRTypeBuilderOptions` | type | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Re-export alias from `gw/ir`. |
| `IRTypeNaming` | type | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Re-export alias from `gw/ir`. |
| `MCPChannelEvent` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPQuery` response; no adopter use. |
| `MCPConfig` | type | `gw/mcp_config.go` | — | Public | MCP allowlist config; used via `SetMCPConfig` / `MCPConfigSnapshot`; adopters may query or set it. |
| `MCPEventsBundle` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPQuery` response. |
| `MCPQueryInput` | type | `gw/mcp_tools.go` | — | Internal | Input struct for `MCPQuery`; only used inside admin HTTP handler. |
| `MCPResponseWithEvents` | type | `gw/mcp_tools.go` | — | Internal | Response type for `MCPQuery`; only consumed by admin handler. |
| `MethodStatsSnapshot` | type | `gw/stats.go` | — | Internal | Return type of `Snapshot()`; only consumed by `admin_huma.go`. |
| `Metrics` | type | `gw/metrics.go` | — | Public | Interface for `WithMetrics`; adopters implement it. |
| `Middleware` | type | `gw/gateway.go` | docs/middleware.md | Public | Type of `Transform.Runtime`; `docs/middleware.md` documents the manual `Transform{Runtime: mw}` pattern as canonical for custom middleware. Lock for v1. |
| `NullablePathRewrite` | type | `gw/transforms.go` | — | Internal | Concrete rewrite returned by `InjectPath(Nullable(true))`; type-asserted inside `gw/` only. |
| `NullableTypeRewrite` | type | `gw/transforms.go` | — | Internal | Concrete rewrite returned by `InjectType(Nullable(true))`. |
| `Option` | type | `gw/gateway.go` | all examples, `gw/cmd/gwag` | Public | Primary config function type. |
| `QuotaOptions` | type | `gw/quota.go` | — | Public | Option struct for `WithQuota`. |
| `RuntimeOptions` | type | `gw/render_graphql_runtime.go` | — | Keep-Public-Pending-Review | Config for `RenderGraphQLRuntime`; see boundary cases. |
| `SchemaExpandEnumValue` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPSchemaExpand` response. |
| `SchemaExpandField` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPSchemaExpand` response. |
| `SchemaExpandOp` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPSchemaExpand` response. |
| `SchemaExpandResult` | type | `gw/mcp_tools.go` | — | Internal | Return type of `MCPSchemaExpand`; only consumed by admin handler. |
| `SchemaExpandType` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type for `MCPSchemaExpand` response. |
| `SchemaListEntry` | type | `gw/mcp_tools.go` | — | Internal | Return element of `MCPSchemaList`; only consumed by admin handler. |
| `SchemaOpKind` | type | `gw/mcp_tools.go` | — | Internal | Enum string type used only inside MCP admin surface. |
| `SchemaRewrite` | type | `gw/gateway.go` | — | Public | Interface in `Transform.Schema`; adopters pass values of this type via `HideType[T]`. |
| `SchemaSearchArg` | type | `gw/mcp_tools.go` | — | Internal | Huma wire type embedded in `SchemaSearchEntry`. |
| `SchemaSearchEntry` | type | `gw/mcp_tools.go` | — | Internal | Return element of `MCPSchemaSearch`; only consumed by admin handler. |
| `SchemaSearchInput` | type | `gw/mcp_tools.go` | — | Internal | Input struct for `MCPSchemaSearch`; only used inside admin handler. |
| `ServiceHistory` | type | `gw/stats.go` | — | Internal | Return type of `History()`; only consumed by `admin_huma.go`. |
| `ServiceOption` | type | `gw/gateway.go` | all examples, `gw/cmd/gwag` | Public | Per-registration config function type. |
| `StatsSnapshot` | type | `gw/stats.go` | — | Internal | Embedded in `MethodStatsSnapshot`; admin-surface only. |
| `SubjectInfo` | type | `gw/broker.go` | — | Internal | Return element of `ActiveSubjects()`; only consumed by `admin_huma.go`. |
| `SubscriptionAuthOptions` | type | `gw/gateway.go` | `examples/multi`, `examples/sign` | Public | Option struct for `WithSubscriptionAuth`. |
| `Transform` | type | `gw/gateway.go` | — | Public | Bundle type returned by `InjectType`/`InjectPath`/`InjectHeader`; adopters pass it to `gw.Use()`. |

### `*Gateway` methods

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `(g *Gateway) ActiveSubjects` | method | `gw/broker.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) AddAdminEvents` | method | `gw/admin_events.go` | `examples/multi`, `gw/cmd/gwag` | Public | Wiring step for admin event subscriptions. |
| `(g *Gateway) AddGraphQL` | method | `gw/graphql_register.go` | — | Public | Service registration API. |
| `(g *Gateway) AddOpenAPI` | method | `gw/openapi_register.go` | — | Public | Service registration API. |
| `(g *Gateway) AddOpenAPIBytes` | method | `gw/openapi_register.go` | `examples/multi`, `gw/cmd/gwag` | Public | Service registration API. |
| `(g *Gateway) AddProto` | method | `gw/loader.go` | `examples/auth`, `gw/cmd/gwag` | Public | Service registration API. |
| `(g *Gateway) AddProtoBytes` | method | `gw/loader.go` | — | Public | Service registration API. |
| `(g *Gateway) AddProtoFS` | method | `gw/loader.go` | — | Public | Service registration API. |
| `(g *Gateway) AdminHumaRouter` | method | `gw/admin_huma.go` | `examples/multi`, `gw/cmd/gwag` | Public | Wiring: returns the admin HTTP mux and OpenAPI spec. |
| `(g *Gateway) AdminMiddleware` | method | `gw/auth_admin.go` | `examples/multi`, `gw/cmd/gwag` | Public | Admin bearer-check middleware. |
| `(g *Gateway) AdminToken` | method | `gw/gateway.go` | — | Public | Raw token bytes; documented admin surface. |
| `(g *Gateway) AdminTokenHex` | method | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Hex-encoded admin token for display. |
| `(g *Gateway) AdminTokenPath` | method | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Path where token was persisted. |
| `(g *Gateway) Close` | method | `gw/gateway.go` | `examples/sign` | Public | Shutdown. |
| `(g *Gateway) Cluster` | method | `gw/cluster.go` | — | Public | Returns attached cluster; needed for graceful shutdown wiring. |
| `(g *Gateway) ControlPlane` | method | `gw/control.go` | `examples/multi`, `examples/sign`, `gw/cmd/gwag` | Public | Returns the gRPC control-plane server for registration. |
| `(g *Gateway) Drain` | method | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Graceful drain. |
| `(g *Gateway) GRPCUnknownHandler` | method | `gw/grpc_ingress.go` | `examples/multi`, `gw/cmd/gwag` | Public | Canonical gRPC passthrough wiring. |
| `(g *Gateway) Handler` | method | `gw/http_ingress.go` | all examples, `gw/cmd/gwag` | Public | Primary GraphQL HTTP handler. |
| `(g *Gateway) HealthHandler` | method | `gw/health.go` | `examples/multi`, `gw/cmd/gwag` | Public | `/health` handler. |
| `(g *Gateway) History` | method | `gw/stats.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) IngressHandler` | method | `gw/http_ingress.go` | `examples/multi`, `gw/cmd/gwag` | Public | HTTP ingress under `/ingress/`. |
| `(g *Gateway) InjectorInventory` | method | `gw/inject_inventory.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) IsDraining` | method | `gw/gateway.go` | — | Public | Drain-state probe; operators may use in health checks. |
| `(g *Gateway) MCPAllows` | method | `gw/mcp_config.go` | — | Internal | Only called inside `gw/mcp_tools.go` and `admin_huma.go`. |
| `(g *Gateway) MCPConfigSnapshot` | method | `gw/mcp_config.go` | — | Internal | Only called inside `admin_huma.go`; not part of operator wiring. |
| `(g *Gateway) MCPHandler` | method | `gw/mcp_server.go` | `examples/multi`, `gw/cmd/gwag` | Public | MCP SSE endpoint handler; used in canonical wiring. |
| `(g *Gateway) MCPQuery` | method | `gw/mcp_tools.go` | — | Internal | Only called inside `admin_huma.go`; not operator-facing. |
| `(g *Gateway) MCPSchemaExpand` | method | `gw/mcp_tools.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) MCPSchemaList` | method | `gw/mcp_tools.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) MCPSchemaSearch` | method | `gw/mcp_tools.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) MetricsHandler` | method | `gw/metrics.go` | `examples/multi`, `gw/cmd/gwag` | Public | `/metrics` Prometheus handler. |
| `(g *Gateway) PprofMux` | method | `gw/gateway.go` | `examples/multi`, `gw/cmd/gwag` | Public | Returns pprof mux if `WithPprof` was set. |
| `(g *Gateway) SchemaHandler` | method | `gw/schema.go` | `examples/multi`, `gw/cmd/gwag` | Public | GraphQL SDL endpoint. |
| `(g *Gateway) SchemaOpenAPIHandler` | method | `gw/schema.go` | `examples/multi`, `gw/cmd/gwag` | Public | OpenAPI spec export endpoint. |
| `(g *Gateway) SchemaProtoHandler` | method | `gw/schema.go` | `examples/multi`, `gw/cmd/gwag` | Public | Proto FDS export endpoint. |
| `(g *Gateway) SetMCPConfig` | method | `gw/mcp_config.go` | — | Internal | Only called inside `admin_huma.go` mutation handlers. |
| `(g *Gateway) Snapshot` | method | `gw/stats.go` | — | Internal | Only called inside `admin_huma.go`. |
| `(g *Gateway) Use` | method | `gw/gateway.go` | `examples/multi`, `examples/auth`, `gw/cmd/gwag` | Public | Core transform registration method. |

### `*Cluster` methods

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `(c *Cluster) Close` | method | `gw/cluster.go` | — | Public | Cluster shutdown; needed for graceful wiring. |
| `(c *Cluster) WaitForJetStream` | method | `gw/cluster.go` | — | Public | Readiness probe used at startup. |

### Standalone functions / methods on value types

| Symbol | Kind | File | External users | Classification | Notes |
|---|---|---|---|---|---|
| `StartCluster` | func | `gw/cluster.go` | `examples/multi`, `gw/cmd/gwag` | Public | Cluster constructor. |
| `(t ChannelAuthTier) String` | method | `gw/auth_channel.go` | — | Public | Stringer; comes with the enum. |
| `(c Code) String` | method | `gw/gateway.go` | — | Public | Stringer. |
| `(f InjectorFrame) String` | method | `gw/inject_inventory.go` | — | Internal | Stringer on internal inventory type. |

---

## Boundary Cases

### `InternalProtoHandler` / `InternalProtoSubscriptionHandler` (`gw/internal_proto.go`)
These are the callback types for in-process proto service registration — services the gateway hosts internally (admin, quota, pubsub auth delegates). The name says "internal" but the types are exported and they're the extension points for `addInternalProtoSlotLocked`. Today no adopter wires these directly (the only callers are inside `gw/`). **Decision needed:** is in-process service hosting a v1 feature (making these Public), or is it an implementation detail that should move to `gw/internal/`? If plan §Admin or future "first-party plugin" docs promise this surface, lock it; otherwise unexport.

### `RenderGraphQLRuntime` / `RenderGraphQLRuntimeFields` / `RuntimeOptions` (`gw/render_graphql_runtime.go`)
These are re-exports from `gw/ir`. The `gat` package (the embedded-translator) calls `ir.RenderGraphQLRuntime` directly — it bypasses `gw/` entirely. No external adopter of `gw/` has been observed calling these. They exist for the case where a power user wants to drive schema rendering outside a full `Gateway`. If that use-case is intentional (documented in gat.md / codegen spike), keep Public. If not, make Internal or drop the re-exports and have callers import `gw/ir` directly.

### `IRTypeBuilder` / `IRTypeBuilderOptions` / `IRTypeNaming` / `NewIRTypeBuilder` (`gw/render_graphql_runtime.go`)
Type aliases re-exported from `gw/ir` with an explicit "backward compatibility" comment. No external adopter of `gw/` uses these today; `gw/gat` imports `gw/ir` directly. These should be demoted to Internal (the backward-compat comment is aspirational, not historical) unless there is a known adopter who imports `gw` specifically for these aliases.

### `Transform` struct fields: `Inventory []InjectorRecord` and `Headers []HeaderInjector`
`Transform` is fully Public (adopters receive it from `InjectType`/etc and pass it to `Use`). The two struct fields `Inventory` and `Headers` embed Internal types (`InjectorRecord`, `HeaderInjector`). Adopters never construct or read these fields directly — but they're in a Public struct. Making the types unexported would require making these fields unexported too (breaking the struct), or keeping the types exported while classifying them Internal. One path: make `Transform` opaque (unexport the fields, keep the struct public). Needs maintainer ruling.

### `MCPAllows` / `MCPConfigSnapshot` / `SetMCPConfig` / `MCPQuery` / `MCPSchemaExpand` / `MCPSchemaList` / `MCPSchemaSearch`
These are `*Gateway` methods that power the admin HTTP surface — they're called only by `admin_huma.go`, not by adopters wiring a gateway. The question is whether to keep them Public (allowing an adopter to call MCP logic directly, e.g. from a custom HTTP handler) or to drive them via the admin router only. `SetMCPConfig` in particular has cluster write-through semantics — if an adopter is building a custom admin UI, they need it. Lean Public but classify as admin-surface, not core-wiring, to set expectations.

### `BackpressureConfig` / `BackpressureMiddleware`
These are exported but used only internally to wire per-pool dispatch. The public config surface is `BackpressureOptions` (passed to `WithBackpressure`). `BackpressureConfig` / `BackpressureMiddleware` are the internal plumbing and should move to `gw/internal/`. The only risk is if an adopter has built a custom dispatcher; check issue tracker / changelog before moving.
