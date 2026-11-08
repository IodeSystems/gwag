# Admin auth + outbound HTTP

## Admin auth (admin token, dev token)

The gateway protects its own admin surface (`/admin/*` and the
self-ingested `admin` GraphQL fields) with two bearer tokens. On boot,
the gateway reads each from `<adminDataDir>/admin-token` and
`<adminDataDir>/dev-token`, or generates a fresh 32-byte one and
persists it. Both are logged to stderr at startup:

```
admin token = ab9089b1...  (persisted to /var/lib/gateway/admin-token)
dev token   = 5c01de77...  (persisted to /var/lib/gateway/dev-token)
```

Wire either as standard `Authorization: Bearer <hex>`. The token
decides the role:

| Role | How | May |
|---|---|---|
| admin | admin token, or an `AdminAuthorizer` OK | every admin route |
| dev | dev token, or an `AdminAuthorizer` OK with `ADMIN_ROLE_DEV` | every admin read (services with owner contacts, graph, usage, instances, peers, injectors, MCP config and schema, external dependents), and `PUT` / `DELETE /admin/external-dependents/{id}` |
| anonymous | no bearer | `whoami`, `listServices`, `servicesStats`, `servicesHistory`, `serviceStats`, `deprecatedStats`, `usageEdges`, `dependencyGraph`: public services only |

Role by operation:

| Operation | anonymous | dev | admin |
|---|---|---|---|
| `whoami`, `listServices`, `servicesStats`, `servicesHistory`, `serviceStats`, `deprecatedStats`, `usageEdges`, `dependencyGraph` | public only | yes | yes |
| `listPeers`, `listChannels`, `listBindings`, `listInjectors`, `listInstances`, `listExternalDependents`, `mcpList`, `mcpSchemaList`, `mcpSchemaSearch`, `mcpSchemaExpand` | 401 | yes | yes |
| `putExternalDependent`, `deleteExternalDependent` | 401 | yes | yes |
| `forgetPeer`, `signSubscriptionToken`, `retractStable`, `deprecate`, `undeprecate`, `mcpInclude`, `mcpExclude`, `mcpIncludeRemove`, `mcpExcludeRemove`, `mcpSetAutoInclude`, `mcpQuery`, `drain`, `drainInstance`, `rotateDevToken` | 401 | 403 | yes |
| `createAppKey`, `listAppKeys`, `revokeAppKey` (app keys, see [services.md](./services.md#app-keys)) | 401 | 403 | yes |

A wrong bearer is 401 on every route, reads included, so a client
learns at once that its token is stale. `GET /admin/whoami` returns
the role (`anonymous`, `dev`, `admin`); the UI uses it for the role
chip and to disable the controls a role cannot use. An admin route
that is not in the table needs dev for GET and admin otherwise.
`listAppKeys` is the one GET that needs admin.
Routes you wrap in `AdminMiddleware` yourself keep the old rule:
GET / HEAD / OPTIONS open, everything else admin.

**What a reader without a token sees.** Only public services:

- a service is public when its registration sets
  `ServiceBinding.public` (`controlclient.Service.Public`), when it was
  added at boot with the `gateway.AsPublic()` ServiceOption, or when
  the gateway runs with `gateway.WithPublicServices("all")` /
  `--public-services=all` (or names it: `--public-services=a,b`).
  Public is per (namespace, line): each line's latest registration
  decides. Gateway-internal `_*` namespaces are never listed.
- a public service shows its name, lines, instance states, owner team
  and traffic. Owner contact, instance addresses, drain reasons and
  rejected-join counters are blank.
- non-public services are not listed. A declared edge between a public
  and a non-public service is dropped; a public line's sunset reason
  says "a hidden service accepts …" instead of naming it. Traffic into
  a public service from a caller that is not a public service is
  reported under the caller `hidden` (one edge per line), so traffic
  totals stay true.
- external-dependent declarations are not shown at all: no list, no
  nodes, no edges. Their reasons read "an external dependent accepts …".
- `admin_events_watchServices` delivers events for public services
  only, with `addr` blanked. The subscription reads the bearer from the
  WebSocket handshake or from `authorization: "Bearer <hex>"` in the
  `connection_init` payload.

Admin visibility only: calls to services through GraphQL, REST, gRPC
or Connect are not affected, and `/api/schema/*` plus GraphQL
introspection still describe every service.

**Dev token rotation.** `POST /admin/dev-token/rotate` (admin only,
GraphQL `admin.rotateDevToken`) replaces the dev token on that node,
persists it, and returns it; the old one stops working at once.
`gateway.RotateDevToken()` does the same from Go. A token pinned with
`gateway.WithDevToken(...)` cannot be rotated (409). Each node has its
own tokens, like the admin token. To retire a dev token without the
API, delete `<adminDataDir>/dev-token` and restart.

`gwag login --token HEX` stores either token; the gateway decides the
role.

**Destructive reads.** A GET that has side effects or exposes
sensitive data isn't really a safe read. Opt those paths into the
auth gate with `WithDestructiveReads("/admin/export", "/admin/dump/*")`
(exact match, or prefix with a trailing `*`) — their GET/HEAD is then
gated like a write, everywhere the path is reached (including the
`admin_*` GraphQL field that dispatches a GET to it, so a UI reading
that field must present the bearer).

```go
gw := gateway.New(
    gateway.WithAdminDataDir("/var/lib/gateway"),
    gateway.WithPublicServices("all"), // optional: no-token readers see every service
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

For richer authz than two static tokens, register an
`AdminAuthorizer` service at `_admin_auth/v1` (proto in
[`adminauth/v1`](../gw/proto/adminauth/v1)). The middleware consults
it on every request that needs more than anonymous, and on every
request that presents a bearer (reads included):

| Delegate response       | Middleware action                          |
|-------------------------|--------------------------------------------|
| `OK`                    | Accept with `role`: `ADMIN_ROLE_DEV` = dev; `ADMIN_ROLE_ADMIN` or unset = admin |
| `DENIED`                | 401, no fall-through                       |
| `UNAVAILABLE`           | Fall through to the tokens                 |
| `NOT_CONFIGURED`        | Fall through to the tokens                 |
| Transport error / panic | Fall through to the tokens                 |

A delegate written before roles sends no `role`, so its `OK` still
means admin. A role below what the route needs is 403.

The boot token is an unconditional fallback. A delegate that
crashes, mis-deploys, or DOS's cannot lock operators out — only an
explicit `DENIED` short-circuits.

Admin auth is unrelated to outbound auth to OpenAPI backends. For
that, see below. Services authenticate to the control plane with app
keys, not admin tokens ([services.md](./services.md#app-keys)); the
admin token is accepted there too.

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

This works for **proto / gRPC dispatch** too: the same trace headers
are bridged from the inbound request (an HTTP GraphQL/REST ingress, or
the incoming gRPC metadata on the gRPC ingress) onto the outbound gRPC
metadata, again without needing OpenTelemetry. (Auth-header forwarding
via `ForwardHeaders` remains a no-op for gRPC — only the trace headers
bridge.)

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

**OAuth/JWT token exchange.** To preserve the caller's identity but
re-mint the token for the upstream's audience (RFC 8693), use
`TokenExchangeClient`. It reads the inbound bearer (from the HTTP
request or the gRPC ingress metadata), swaps it at `TokenURL` for an
upstream-scoped token, caches the result until shortly before expiry,
and attaches it outbound:

```go
client, err := gateway.TokenExchangeClient(gateway.TokenExchangeConfig{
    TokenURL: "https://idp/oauth/token",
    Audience: "billing",          // + optional Scope / Resource / ClientID+Secret
})
gw.AddOpenAPI(spec, gateway.As("billing"), gateway.To(url),
    gateway.OpenAPIClient(client))
```

It errors the request if no inbound token is present (there's nothing
to exchange). The three identity modes — forward the caller's token
(`ForwardHeaders`), call as the gateway (`ServiceAccountTransport`), or
exchange (`TokenExchangeClient`) — are picked per source.

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
