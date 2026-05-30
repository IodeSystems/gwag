# Admin auth + outbound HTTP

## Admin auth (boot token)

The gateway protects its own admin surface (`/admin/*` writes,
`admin_*` GraphQL mutations) with a bearer token. On boot, the
gateway either reads an existing token from
`<adminDataDir>/admin-token` or generates a fresh 32-byte one and
persists it. The token is logged to stderr at startup:

```
admin token = ab9089b1...  (persisted to /var/lib/gateway/admin-token)
```

Wire it as standard `Authorization: Bearer <hex>`. Reads (GETs on
`/api/admin/*`, `admin_listPeers` / `admin_listServices` queries)
are public so the UI works unauthenticated; writes require the token.

```go
gw := gateway.New(
    gateway.WithAdminDataDir("/var/lib/gateway"),
)
adminMux, adminSpec, _ := gw.AdminHumaRouter()
mux.Handle("/api/admin/", http.StripPrefix("/api", gw.AdminMiddleware(adminMux)))

// admin_* GraphQL mutations dispatch through /api/admin/*; the
// inbound Authorization header is forwarded automatically, so one
// bearer covers both surfaces.
gw.AddOpenAPIBytes(adminSpec,
    gateway.As("admin"),
    gateway.To("http://localhost:8080/api"))
```

## Pluggable AdminAuthorizer delegate

For richer authz than a single static token, register an
`AdminAuthorizer` service at `_admin_auth/v1` (proto in
[`adminauth/v1`](../gw/proto/adminauth/v1)). The middleware consults
it on every protected request:

| Delegate response       | Middleware action                          |
|-------------------------|--------------------------------------------|
| `OK`                    | Accept                                     |
| `DENIED`                | 401, no fall-through                       |
| `UNAVAILABLE`           | Fall through to boot token                 |
| `NOT_CONFIGURED`        | Fall through to boot token                 |
| Transport error / panic | Fall through to boot token                 |

The boot token is an unconditional fallback. A delegate that
crashes, mis-deploys, or DOS's cannot lock operators out — only an
explicit `DENIED` short-circuits.

Admin auth is unrelated to outbound auth to OpenAPI backends. For
that, see below.

## Outbound HTTP transport for OpenAPI dispatch

By default, `Authorization` is forwarded from the inbound GraphQL
request to the outbound OpenAPI dispatch (the caller's token reaches
the upstream verbatim). Override the allowlist per source with
`gateway.ForwardHeaders(...)`.

**Trace propagation.** Distributed-tracing / correlation headers —
W3C `traceparent` / `tracestate` / `baggage`, B3 (`b3`, `x-b3-*`),
`x-request-id`, and the AWS / GCP trace headers — ride along
automatically with any active forwarding; you don't list them in
`ForwardHeaders`. When an OpenTelemetry `TracerProvider` is wired
(`WithTracer`), the gateway's own span context is injected as
`traceparent` after forwarding, so the upstream sees the gateway as
the parent span; without a provider, the inbound trace headers pass
through unchanged. An empty `ForwardHeaders()` opts out of everything,
trace headers included.

**Service-account auth.** When the gateway should call the upstream as
*itself* rather than forwarding the caller's token, use the built-in
`ServiceAccountTransport` (or the `ServiceAccountClient` shortcut) and
install it via the client below:

```go
src := gateway.StaticToken(os.Getenv("SA_TOKEN")) // or a refreshing TokenSource
gw.AddOpenAPI("https://billing.internal/openapi.json",
    gateway.As("billing"), gateway.To("https://billing.internal"),
    gateway.OpenAPIClient(gateway.ServiceAccountClient(src)), // Authorization: Bearer <token>
)
```

`ServiceAccountTransport{Token, Header, Scheme, Base}` customizes the
header (default `Authorization`), scheme (default `Bearer`; `""` writes
the raw token for a custom header like `X-Api-Key`), and underlying
transport.

For anything else — mTLS, signed-URL rewriting, retry/timeout policy,
a hand-rolled token-exchange `RoundTripper` — supply your own
`*http.Client`:

```go
// Gateway-wide default — used by every OpenAPI source unless
// overridden per-source.
gw := gateway.New(gateway.WithOpenAPIClient(&http.Client{
    Transport: customRoundTripper,
    Timeout:   10 * time.Second,
}))

// Per-source override — beats the gateway-wide default.
gw.AddOpenAPI("https://billing.internal/openapi.json",
    gateway.As("billing"),
    gateway.To("https://billing.internal"),
    gateway.OpenAPIClient(billingClient),  // custom mTLS to this one backend
)
```

When neither is set, dispatches use `http.DefaultClient`.
