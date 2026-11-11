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
| system | a system key (`gws_...`, `gwag key create --system`) | `POST /admin/replay/verify`, `exportClusterState`, `importClusterState`, `getClusterMirror`, `startClusterMirror`, `stopClusterMirror` only (cluster to cluster, see [cutover.md](./cutover.md#system-keys)) |

Role by operation:

| Operation | anonymous | dev | admin | system key |
|---|---|---|---|---|
| `whoami`, `listServices`, `servicesStats`, `servicesHistory`, `serviceStats`, `deprecatedStats`, `usageEdges`, `dependencyGraph` | public only | yes | yes | 403 |
| `listPeers`, `listChannels`, `listBindings`, `listInjectors`, `listInstances`, `listExternalDependents`, `mcpList`, `mcpSchemaList`, `mcpSchemaSearch`, `mcpSchemaExpand`, `getClusterFreeze` | 401 | yes | yes | 403 |
| `putExternalDependent`, `deleteExternalDependent`, `reviewExternalDependent`, `listNotifications`, `dismissNotification` | 401 | yes | yes | 403 |
| `forgetPeer`, `evacuatePeer`, `finalizeCluster`, `signSubscriptionToken`, `retractStable`, `deprecate`, `undeprecate`, `mcpInclude`, `mcpExclude`, `mcpIncludeRemove`, `mcpExcludeRemove`, `mcpSetAutoInclude`, `mcpQuery`, `drain`, `drainInstance`, `rotateDevToken`, `revokeAdminSessions` | 401 | 403 | yes | 403 |
| `createAppKey`, `createSystemKey`, `listAppKeys`, `revokeAppKey` (app and system keys, see [services.md](./services.md#app-keys)) | 401 | 403 | yes | 403 |
| `freezeCluster`, `unfreezeCluster` (cutover freeze) | 401 | 403 | yes | 403 |
| `listReplayDiffs` (replay verification, see [cutover.md](./cutover.md)) | 401 | 403 | yes | 403 |
| `getSwitchover` (switchover, see [cutover.md](./cutover.md#switchover)) | 401 | yes | yes | 403 |
| `setSwitchover`, `clearSwitchover` | 401 | 403 | yes | 403 |
| `POST /admin/replay/verify`, `exportClusterState`, `importClusterState` | 401 | 403 | yes | yes |
| `getClusterMirror` (event stream mirror, see [cutover.md](./cutover.md#event-stream-mirror)) | 401 | yes | yes | yes |
| `startClusterMirror`, `stopClusterMirror` | 401 | 403 | yes | yes |

A system key is checked only by `AdminMiddleware`: the
`AdminAuthorizer` and `AppKeyAuthorizer` delegates never see one, and
the control plane, the Schema Registry and the `admin` GraphQL fields
refuse it. A system key on any other route is 403; an expired or
revoked one is 401.

A wrong bearer is 401 on every route, reads included, so a client
learns at once that its token is stale. `GET /admin/whoami` returns
the role (`anonymous`, `dev`, `admin`); the UI uses it for the role
chip and to disable the controls a role cannot use. An admin route
that is not in the table needs dev for GET and admin otherwise.
`listAppKeys`, `listReplayDiffs` and `exportClusterState` are the GETs
that need admin.
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

## SSO

People can sign in to the admin UI with an OpenID Connect provider
instead of pasting a token. The session is a cookie; the role comes
from rules on the email and groups the provider sends. Tokens keep
working, and a request that presents a bearer is checked by the bearer
alone.

```
GWAG_SSO_CLIENT_SECRET=... gwag up \
  --sso-issuer https://idp.example.com/realms/ops \
  --sso-client-id gwag \
  --sso-rule group:gwag-admins=admin \
  --sso-rule domain:example.com=dev
```

| Flag | Default | |
|---|---|---|
| `--sso-issuer` | (off) | Issuer URL; discovery at `<issuer>/.well-known/openid-configuration`. A trailing slash difference is tolerated. |
| `--sso-client-id` | | Client registered at the provider. |
| `--sso-client-secret-env` | `GWAG_SSO_CLIENT_SECRET` | Variable that holds the secret. Unset or empty: a public client, PKCE only. |
| `--sso-redirect-url` | from the request | Callback registered at the provider. Default: `/auth/callback` on the address the browser used; `X-Forwarded-Proto` and `X-Forwarded-Host` from a proxy are honoured. |
| `--sso-scopes` | `openid,email,profile` | Add `groups` (or what the provider calls it) for group rules. |
| `--sso-groups-claim` | `groups` | Claim that lists the groups, read from the ID token, else from userinfo. |
| `--sso-rule` | | `KIND:VALUE=ROLE`, repeatable, first match wins. Kinds: `email:alice@example.com`, `domain:example.com` (exact; not subdomains), `group:NAME` (case-sensitive). Roles: `admin`, `dev`. |
| `--sso-allow-unverified-email` | off | Admit an email without `email_verified`. |
| `--sso-session-ttl` | `12h` | Session lifetime. |
| `--sso-cli-token-ttl` | `12h` | Lifetime of a `gwag login --sso` bearer; capped by `--sso-session-ttl`. |

The example gateway (`examples/multi`) takes the same flags. From Go:
`oidc.New(oidc.Config{...})` in `gw/oidc`, then
`gateway.WithAdminSession(sso)`, `sso.Mount(mux, gw)`, and
`gateway.ForwardHeaders(gateway.AdminForwardHeaders...)` on the admin
self-ingest (without it, `admin` GraphQL fields called with a cookie
reach `/admin/*` as anonymous). The gateway core does not import
`gw/oidc`; go-oidc and x/oauth2 are linked only into binaries that do.

**Routes.** `GET /auth/login` starts the flow, `GET /auth/callback`
ends it, `POST /auth/logout` clears the cookie (same-origin only),
`GET /auth/whoami` (public) returns
`{"sso":true,"authenticated":true,"email":"...","role":"admin","via":"sso"}`,
with `"error":"sso unavailable"` when SSO cannot be used now. Bearer tokens are not
reported there; `GET /api/admin/whoami` reports the role for both.

**Flow.** Authorization code with PKCE (S256), state and nonce. The
flow state is a signed cookie for 10 minutes on `/auth/`. The ID token
is checked for signature (JWKS), issuer, audience, expiry (1 min skew),
nonce and an issued-at not in the future. Missing email in the ID
token: userinfo is asked, and its answer is used only when its `sub`
is the token's.

**Sign-in refused** (a page says why, no cookie is set) when the email
is missing, when it is not verified (unless
`--sso-allow-unverified-email`), or when no rule matches.

**Session.** Cookie `gwag_session`: HMAC-SHA256 over
`{email, groups, issued, expiry}`, `HttpOnly`, `SameSite=Lax`, `Path=/`,
`Secure` on TLS, behind a proxy sending `X-Forwarded-Proto: https`, or
with an https redirect URL. Only the groups that a rule names are kept,
so a provider that sends hundreds stays under the 4 KB cookie limit;
a group added to the rules later needs a fresh sign-in. The rules are
evaluated on every request, so a changed rule applies at once. The
subject is recorded as `admin:alice@example.com` on the writes that
keep an audit identity.

**CSRF.** A request authenticated by the cookie is accepted on GET and
HEAD. Any other method needs `Sec-Fetch-Site: same-origin`; a missing
header is refused (403), so a non-browser client must use a bearer. A
WebSocket upgrade (the `admin_events` subscription) needs an `Origin`
whose host is the request `Host` or the first `X-Forwarded-Host`. The
`admin` GraphQL fields forward `Cookie` and `Sec-Fetch-Site` to
`/admin/*`, so the same rule applies to them. A mutation sent as a
cross-site GET is refused there too.

**Revoke.** `POST /admin/sessions/revoke` `{"subject":"alice@example.com"}`
(admin, GraphQL `admin.revokeAdminSessions`) refuses every session of
that email issued until now, on every node. `"*"` revokes everyone's.
Signing in again works.

**Shared state.** The signing key and the revocation cutoffs live in
the KV bucket `gwag-admin-sessions` (no TTL), so a cookie minted on one
node passes on every other. The bucket exists from
[cluster feature level](./cluster.md#feature-levels) 2. Below it,
SSO is off: `/auth/login` answers 503 with the reason and
`/auth/whoami` answers `"error":"sso unavailable"` (the reason is in
the gateway log). Run `gwag cluster finalize` once
every node runs this version. Standalone (no cluster), the key is
`<adminDataDir>/session-key` and the cutoffs
`<adminDataDir>/admin-sessions.json` (in memory without a data dir).
To end every session, revoke `"*"`.

**Not covered.** The Schema Registry REST routes and the control plane
take bearers only (a `gwag login --sso` bearer works on the control
plane; see below).

### CLI sign-in (`gwag login --sso`)

```
gwag login --sso                       # the current login
gwag login --sso --name corp --gateway gw.example.com:50090 --endpoint https://gw.example.com
gwag login --sso --no-browser          # print the URL only
```

The CLI signs in through the browser and saves a bearer in the login,
as `--token` does. It prints the email, role and expiry. Without
`--gateway` it updates the login `--name` names, else the current one.
On a gateway without SSO (`/auth/whoami` is 404) it fails and says so.

Loopback redirect (RFC 8252):

1. The CLI listens on `127.0.0.1:0`, picks a random `state` and a
   verifier, and opens
   `/auth/cli?port=P&state=S&challenge=C`, C = base64url(SHA-256(verifier)).
   `--no-browser` prints the URL and waits; open it on the same machine
   (or forward the port with `ssh -L P:127.0.0.1:P`).
2. `/auth/cli` checks the parameters (port 1024-65535, state 1-128
   base64url characters, challenge 43) and runs the normal sign-in, with
   them in the flow cookie.
3. The callback sets no cookie. It stores a one-time code (60 s, single
   use, in the `gwag-admin-sessions` bucket, so any node can take it)
   and redirects to `http://127.0.0.1:P/callback?code=...&state=S`.
   The host is always `127.0.0.1`; nothing in the request names it.
4. The CLI posts `{"code","verifier"}` to `POST /auth/cli/token` and gets
   `{"token","email","role","expires_at"}`. The first attempt spends
   the code, right verifier or not.

The bearer is `gwo_` + the signed session payload (`email`, groups,
issued, expiry), signed for a different purpose than the cookie: a
bearer does not pass as a cookie, nor a cookie as a bearer. Sent as
`Authorization: Bearer gwo_...`, it is checked by the SSO only, never
by the AdminAuthorizer delegate. The role is evaluated per request
from the rules, and `revokeAdminSessions` ends it like a cookie. It is
a header, so the cookie CSRF rule does not apply. It works where an
admin bearer works: `/api/admin/*`, admin GraphQL fields, the control
plane.

A process on the same machine that listens on the port the CLI chose
could take the code; it would still need the verifier, which stays in
the CLI.

### Provider setup

Register a web application client with the redirect URI
`https://<gateway host>/auth/callback` (or what `--sso-redirect-url`
says).

- **Keycloak.** Client `gwag`, client authentication on, standard flow
  on, valid redirect URI `https://gw.example.com/auth/callback`. Issuer
  `https://kc.example.com/realms/<realm>`. For group rules, add a
  "Group Membership" mapper (token claim name `groups`, full group path
  off to get `gwag-admins` rather than `/gwag-admins`).
- **Google.** OAuth client of type "Web application", authorized
  redirect URI as above. Issuer `https://accounts.google.com`. Google
  sends no groups claim: use `email:` and `domain:` rules. Every Gmail
  account can sign in at Google, so a rule decides who gets in.
- **Microsoft Entra ID.** App registration, platform "Web", redirect URI
  as above, a client secret. Issuer
  `https://login.microsoftonline.com/<tenant id>/v2.0`. For group
  rules, token configuration → add groups claim; Entra sends group
  object ids, so the rule is `group:<object id>=admin`. A user in more
  than 200 groups gets no groups claim in the token (Entra's overage
  rule); assign the app to groups to keep it under the limit.

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
