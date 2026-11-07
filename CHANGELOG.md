# Changelog

Notable changes to gwag, conventional-commits-derived but human-edited.
One section per release; `Unreleased` at the top while main moves.

The version contract is in [`docs/stability.md`](./docs/stability.md):
SemVer over the symbols marked `// Stability: stable`, additive
changes on MINOR, drops on MAJOR.

## Unreleased

### Added
- gat: a failed huma input check (constraint or Resolver) reaches GraphQL with `extensions.fields: [{path, message}]` next to `status` / `code`, one entry per huma `ErrorDetail`. `path` is in GraphQL argument terms: `body.name` (the body arg is `body`), a bare `limit` for a query / path / header / cookie param, list indexes as dot segments (`body.items.1.name`). Errors without details carry no `fields`.
- Admin op `usageEdges` (`GET /admin/services/usage?window=1m|1h|24h&namespace=&version=`): active dependencies by method for every service. One edge per (caller, namespace, version, method) with count, okCount and lastSeenUnixMs, plus a per-(namespace, version) rollup that lists registered services with no traffic. Per-node, in-memory; caller identity comes from `WithCallerIDExtractor` / `WithCallerHeaders`.
- Control plane: service manifest fields on `ServiceBinding`: `depends` (field 10, `repeated Dependency {service, lines}`, the provider namespaces and lines a binding calls) and `owner` (field 11, `Owner {team, contact}`). Optional; a Register without them works as before. Stored on the (namespace, version) slot, replicated through the registry KV, and reported in `ServiceInfo.depends` / `ServiceInfo.owner` (fields 6, 7). The latest registration on a slot is the one reported. A `depends` entry with an empty service, no lines, a line that is not `vN` / `unstable`, or a repeated service rejects the Register.
- `controlclient.Service.Depends` / `.Owner` (`controlclient.Dependency`, `controlclient.Owner`) send the manifest from `SelfRegister`.
- `WithEnvironment(name)` and `--environment` (`gwag`, `gwag up`, example gateway): a label naming the environment (cluster) the gateway belongs to. Reported in `ListServicesResponse.environment` (field 4) and `gwag services list`. It does not change routing or the NATS cluster name.
- Admin API `GET /admin/services` (GraphQL `admin.listServices`): `environment` on the response, and `depends: [{service, lines}]` (never null) and `owner: {team, contact}` (omitted when not declared) on each service.
- Control plane start gate: `RegisterResponse.bindings` (field 3) and `HeartbeatResponse.bindings` (field 2) report, per binding in request order, a `BindingStatus {namespace, version, state, depends}`. `state` is `InstanceState` `READY`, `WAITING` (a dependency has no ready instance on any accepted line) or `DEGRADED` (was ready, a dependency is gone since; it keeps serving). Each `DependencyStatus {service, lines, present_lines}` names the accepted lines that have a ready instance now, so the service can pick the line to call. An empty list (older gateway) means ready.
- `ServiceInfo.instances` (field 8, `InstanceStatus {replica_id, registration_id, instance_id, addr, state, depends}`): every control-plane-registered instance with its start-gate state, waiting ones included. Admin API `GET /admin/services` carries it as `instances: [{replicaId, registrationId, instanceId, addr, state: ready|waiting|degraded, depends: [{service, lines, presentLines}]}]` (never null).
- A Register whose `depends` close a cycle in the (service, line) graph, counting every accepted line and every known instance across the cluster, is refused with `FailedPrecondition` and the cycle in the message (`greeter/v1 -> library/v1 -> greeter/v1`).
- `controlclient`: `Registration.Bindings()` (`[]BindingStatus {Namespace, Version, State, Depends []DependencyStatus}`), `Registration.PresentLines(service)` (lines of a dependency with a ready instance, for picking the client line), `State` (`StateReady` / `StateWaiting` / `StateDegraded`), and `Options.ReadyPollInterval` (default 1s).
- Per-instance drain. Control-plane RPC `DrainInstance(DrainInstanceRequest {instance, cancel})` → `DrainInstanceResponse {registration_id, instance_id, state, reasons, inflight, lines}` with enum `DrainState` (`SERVING`, `DRAINING`, `BLOCKED`, `SAFE_TO_STOP`). `instance` is a registration_id, or an instance_id carried by one registration. The gateway stops routing new requests to every line of that registration and reports `SAFE_TO_STOP` when nothing is in flight to it. It holds the drain as `BLOCKED` (routing continues, `reasons` names each dependent and its lines) while some dependent whose `depends` accept a line of the instance would have no accepted line with another ready instance. Idempotent: call again to poll; `cancel` withdraws it. Cluster: the request is stored in the new `gwag-drain` KV bucket and applies on every node; `SAFE_TO_STOP` needs routing stopped and 0 in flight on every live node. Drain and the start gate count the same ready set: a draining instance does not satisfy a dependent's gate, and a WAITING instance is not cover.
- Admin ops `drainInstance` (`POST /admin/instances/{instance}/drain`, body `{cancel}`) and `listInstances` (`GET /admin/instances`): each instance as `{registrationId, instanceId, addr, state, reasons, inflight, lines}`, `state` one of `serving`, `draining`, `blocked`, `safe_to_stop`. `GET /admin/services` gains `drains` (never null) per service: the instances on that line with a drain requested.
- `gwag drain INSTANCE [--wait] [--timeout 5m] [--cancel]` and `gwag drain --list`: drain one instance through `DrainInstance`; `--wait` exits 0 at `safe_to_stop`, 1 at the timeout.
- `controlclient.Registration.Drain(ctx)`: drain this registration and wait for `safe_to_stop`. `Options.DrainOnSIGTERM` (off by default) catches SIGTERM, drains (at most `Options.DrainTimeout`, default 60s), deregisters, then calls `Options.OnDrained` or re-raises SIGTERM. `Options.DrainPollInterval` sets the poll rate (default 500ms). `Registration.Close` is now safe to call more than once.

### Changed
- `controlclient.SelfRegister` blocks until the gateway reports every binding ready: while a binding waits on its dependencies it polls Heartbeat and logs the missing ones. Bindings without `Depends` return as before. It also retries an unreachable gateway (Unavailable, DeadlineExceeded, ResourceExhausted, Aborted) with backoff until ctx ends, instead of returning the first error; a refused Register still returns at once. Pass a ctx with a deadline to bound either wait.
- A control-plane binding with `depends` is not added to its pool until every dependency has a ready instance on at least one accepted line: until then it has no schema fields, takes no traffic and does not advance `stable`. It joins on its own when the dependencies register (standalone or on any cluster node). Bindings without `depends` register exactly as before. In cluster mode the registry KV value gains `waiting` (omitted when false).
- `ListServices` / `GET /admin/services` also list a (namespace, version) whose instances are all waiting, with `replica_count` 0.

### Fixed
- Admin `deprecate` (`POST /admin/services/{namespace}/{version}/deprecate`): an empty `reason` is now a 422 with a `body.reason` field error (GraphQL `extensions.fields`); it was a 500.

## v2.11.0 — 2026-11-06

GraphQL error extensions default to gat's `{status, code}` shape (see Migration), proto lists render non-null elements, Connect ingress in the `gwag` binary, gRPC for gat BYO OpenAPI ops, and a sweep of export, validation and ingress fixes.

### Added
- `gwag` binary (`gwag up`, plain serve, `gwag serve --graphql` / `--mcp`) mounts `gw.ConnectHandler()` at `/api/connect/`: every registered proto service over Connect, gRPC-Web and gRPC (`/api/connect/package.Service/Method`).
- gat `RegisterGRPC` serves the OpenAPI operations of a BYO `ServiceRegistration` (unary, and `itemSchema` streams as server streams). They were in the GraphQL schema and the rendered proto, but a gRPC client got Unimplemented. Nested lists and maps are unboxed on the way in and boxed on the way out.
- **`WithLegacyErrorCodes()`** (experimental) keeps the GraphQL error extensions from before v2.11 (`{code: "NOT_FOUND"}` on a Reject, no code on other errors). For migration; removed in v3.

### Changed
- OpenAPI streamed responses (SSE, JSONL, NDJSON, json-seq): an item that fails to decode is skipped and logged (rate limited) instead of ending the subscription with an error. The subscriber sees no event for it. Errors from the body itself still end the stream. `ir.OpenAPIStreamReader.Next` no longer returns decode errors; `OnSkip` reports them.
- REST ingress: an OpenAPI or GraphQL upstream that fails on a context deadline answers 504 (was 500), and one that fails on cancellation answers 499 (was 500). `gat.ErrorCode` now checks `context.Canceled` / `DeadlineExceeded` ahead of a `GetStatus()` method, so the gRPC ingress gives DeadlineExceeded / Canceled for the same errors (was Internal).
- Base64 validation (`WithValidation` / gat `Validation`) now also covers multipart string parts that are not file uploads, and `oneOf` / `anyOf` with several branches (passes when any branch of matching JSON type accepts). Responses are checked in `warn` mode only and never fail a call.
- **GraphQL error extensions default to gat's shape**, `{status: 404, code: "not_found"}`: the lower-case gRPC code the gRPC and REST ingresses give the same error, plus its HTTP status, on every dispatch error. Was the upper-case `Code` name on a Reject and no code otherwise; an upstream 409 is now `already_exists` (was `INVALID_ARGUMENT`). Planned for v3, moved forward.
- Proto list fields render non-null elements: `[Book!]` (was `[Book]`), in GraphQL and in the OpenAPI export. A proto list element can never be null. Input lists now refuse a null element at validation.

### Deprecated
- `WithConnectErrorCodes()` does nothing; its shape is the default.

### Fixed
- gat BYO OpenAPI dispatch errors carry the upstream HTTP status, so an upstream 404 / 409 / 503 is NotFound / AlreadyExists / Unavailable over gRPC and has `status` / `code` GraphQL error extensions, instead of Internal with none. Transport failures are 502, undecodable responses 502, argument validation 400.
- gRPC ingress: two proto slots declaring the same fully-qualified procedure were served by a random one per schema rebuild. The first slot in (namespace, version) order now serves it, and each rebuild logs one warning per service naming the serving and skipped slots.
- OpenAPI 3.1 to 3.0 downgrade: an operation whose security was only `mutualTLS` lost its `security` and inherited the document-level requirements, so it read stricter than the source. It now gets `security: []`; the original stays in `x-gwag-oas31` and returns on the way back to 3.1.
- OpenAPI export dropped nullability on fields it built from the IR (GraphQL, proto and MCP origins). A list element that may be null (`[Int]`, `[[Int]!]`) now renders `items` with `nullable: true` (3.0) or `type: [X, "null"]` / `oneOf: [$ref, {type: null}]` (3.1); a `$ref` element becomes `allOf: [$ref], nullable: true`. A required-but-nullable field renders nullable. Re-ingest gives the same nullability.
- Cross-kind OpenAPI export (GraphQL or proto origin): an op's list parameter, list request body and list response exported as the element schema. They export as arrays now; map args as objects.

### Migration
- GraphQL clients that match `extensions.code` against upper-case names (`NOT_FOUND`) must match lower-case gRPC codes (`not_found`), or the server sets `WithLegacyErrorCodes()` until they do. Codes can also differ beyond case: an upstream 409 is `already_exists`, an upstream 502/503/504 is `unavailable`.

## v2.10.0 — 2026-11-06

Nested lists and maps: `[][2]float64`, `[[T]]` and map of map render and dispatch correctly over GraphQL, OpenAPI and gRPC, and inline `JSON` literals parse.

### Added
- **`ir.TypeRef.List` / `ir.ListType`** (experimental): the element of a repeated slot can itself be a list, with its own element nullability. `TypeRef.IsList()` and `TypeRef.Leaf()` go with it. `ir.IsListRow(md)` and `ir.IsMapValueBox(md)` identify the proto row and map-value boxes.

### Fixed
- Nested lists flattened. An OpenAPI array of array (huma `[][2]float64`), a downstream-GraphQL `[[T]]`, and an MCP array of array rendered as `[T]`, so a query that sent `[[1,2]]` failed validation. They now render as nested GraphQL lists in both directions, export as nested OpenAPI arrays, and export to proto as a repeated `<Field>Row { repeated T value = 1; }` box. gRPC ingress (gat huma ops, and gwag OpenAPI / GraphQL slots) unboxes rows on the way in and boxes them on the way out, so the handler or upstream sees plain nested arrays.
- Map of map over gRPC ingress. gat huma ops and gwag OpenAPI / GraphQL slots took a `map<K, map<K, V>>` through its `<Field>Value` proto box without unboxing, so handlers and upstreams saw `{"a":{"value":{...}}}` and responses came back empty. Boxes now convert in both directions.
- gat gRPC ingress: a huma map with 64-bit integer values (`map[string]int`) failed to bind, since protojson sends int64 as a decimal string and map values had no string-to-number path. Maps are now walked like structs.
- An inline literal for a `JSON` or custom-scalar argument (`cells: {a: {x: 1}}`) reached handlers and upstreams as GraphQL AST nodes; gat failed to bind it. It now parses to plain values (`ir.JSONLiteral`, experimental). A variable nested inside such a literal is refused at validation; pass the whole argument as a variable.

## v2.9.0 — 2026-10-31

Relay deploys and sub status: `Relay.Shutdown` closes sockets cleanly, and the generated runtime reports each subscription's real state and retries failures.

### Added
- **`Relay.Shutdown(ctx)`** sends close 1001 ("server restarting") to every socket, closes them, and answers new ones with 503, so a deploy is not a 1006 for every client. `http.Server.Shutdown` does not touch hijacked connections: call it from `srv.RegisterOnShutdown`.
- **Relay generated runtime: per-sub status.** `useSub` returns `{status, error}`, and `subscribeRaw` takes an `onState` callback: `pending`, `subscribed` (the relay acked), or `failed` (the mint threw or the relay refused the cap). `setMintRetry` tunes the retry.

### Fixed
- Relay generated runtime: a failed mint dropped the sub until the next reconnect, and a relay `err` frame was ignored. Both now retry with jittered backoff, 2 s to 60 s, while the sub is mounted and the socket is open.

## v2.8.0 — 2026-10-31

Relay liveness and cheap compression: a `ping` op, a heartbeat and live state in the generated runtime, and `WithCompression`, which compresses a fan-out once rather than once per socket.

### Added
- **`relay.WithCompression()`** negotiates permessage-deflate (no context takeover) on the multiplex relay's sockets. Off by default. A data frame is compressed once per event and sub id and shared among sockets, so a fan-out does not compress per socket.
- **Relay `ping` op.** A client frame `{"op":"ping","id":…}` gets `{"op":"pong","id":…}`, so a browser (which cannot send WebSocket ping control frames) can keep a socket alive through a proxy idle timeout and detect a dead one. Before this, the relay answered `ping` with `{"op":"err","err":"unknown op"}`.
- **Relay generated runtime: heartbeat and live state.** The runtime pings every 20 s, replaces a socket that stops answering, and exports `useLiveSocket`, `subscribeLive`, `liveSnapshot` and `setHeartbeat`. React 18 or later is required for `useLiveSocket`.
- Relay generated runtime: wire sub ids are `<channel>#<n>`, so sockets on a channel can share compressed frames. No relay change is needed; a mismatch of runtime and relay versions works.

## v2.7.0 — 2026-10-31

OpenAPI 3.2 ingest, a 3.0 / 3.1 export option with down-conversion, base64 validation, and one pub/sub transport under gat and gwag.

### Migration
- **OpenAPI `format: byte` outside `multipart/form-data` is now a GraphQL `String`**, not `Upload`, and `contentEncoding: base64` ingests the same way. Clients pass base64 text. It used to reach the upstream as a multipart body or query value holding a Go struct. `format: byte` in `multipart/form-data` is still `Upload`.
- **Proto `bytes` fields are base64 both ways** (proto3 JSON). GraphQL args used to take the raw string, and output was base64 on gwag's default path but the raw string on gat and gwag's map path. Clients that send raw text to a `bytes` arg must base64-encode it; gat clients reading `bytes` output must decode it. Input accepts standard or URL-safe base64, padded or not. Upload-marked `bytes` fields are unchanged.
- **Standalone gwag gateways have pub/sub.** `ps/v1` appears in the schema, admin service lists and MCP tools without `WithCluster`. Tests that count services or slots see one more.
- **`WithChannelAuth` patterns** with a `>` before the last token, or an empty token, make `New` panic. Write `a.>` (rest of the channel) or `a.*.b` (one token).

### Added
- **OpenAPI base64 validation** (experimental). Args declared `format: byte` or `contentEncoding: base64` are checked before dispatch. `WithValidation("strict"|"warn"|"off")` (gwag), `Validation(...)` (gat); `strict` is the default, so bad base64 fails the call without reaching the upstream.
- **OpenAPI version of the schema route** (experimental). `WithOpenAPIVersion("3.0"|"3.1")` (gwag), `OpenAPIVersion("3.0"|"3.1")` (gat) or `?openapi=3.0|3.1` write every spec in that version. `3.1` writes `"openapi": "3.1.0"` with JSON Schema 2020-12 forms: `nullable` becomes a `"null"` type entry, boolean exclusive bounds become numbers, a schema `example` becomes `examples`; a 3.2 source is converted down. `3.0` converts 3.1 and 3.2 sources (huma writes 3.1.0) down to `"openapi": "3.0.3"`, for client generators that read only 3.0; what 3.0 cannot express is kept in `x-gwag-oas31`, `x-gwag-oas32`, `x-webhooks`, `x-additional-operations` and `x-item-schema`. Without the option or parameter the output is unchanged: synthesized specs 3.0.0, OpenAPI sources as declared. New `ir.RenderOpenAPIWith`, `ir.RenderOpenAPIOptions{Version}`, `ir.ParseOpenAPIVersion`, `ir.OpenAPI30Version`. See docs/gat.md "OpenAPI version" for every converted construct and which are lossy.
- OpenAPI ingest reads `x-additional-operations` (QUERY and custom methods) and `x-item-schema` (streamed responses), so a 3.0 spec written by gwag registers with the same operations as its source, on GraphQL, gRPC and REST ingress.
- **OpenAPI 3.2 `query` and `additionalOperations`.** A QUERY operation becomes a GraphQL Query field with a `body` arg; a custom method becomes a Mutation. gwag, gat and REST ingress send the declared method and body upstream. HEAD / OPTIONS / TRACE / CONNECT operations are still skipped, now with a warning at registration (new `ir.UnsupportedOpenAPIOperations`). Cross-kind OpenAPI 3.0 export puts these methods under the `x-additional-operations` Path Item extension.
- **OpenAPI 3.2 streamed responses as GraphQL subscriptions.** An upstream OpenAPI operation whose success response has `itemSchema` on `text/event-stream`, `application/jsonl`, `application/x-ndjson` or `application/json-seq` ingests as a subscription (one upstream request per subscriber), in gwag and in gat BYO-IR registrations. gwag also relays it on `IngressHandler` in the declared media type and serves it as a server-streaming gRPC method; streams count against `MaxStreams` / `MaxStreamsTotal`. An operation that also offers `application/json` stays unary. New `ir.OpenAPIStreamOf`, `ir.OpenAPIStream`, `ir.NewOpenAPIStreamReader` (experimental). See docs/gat.md "Streamed responses from an upstream spec".
- **`gat.Bus`** (experimental): one pub/sub transport interface under gat's `PubSub` and gwag's `ps.pub` / `ps.sub`. Implementations `gat.NewLocalBus`, `gat.NewPeerMeshBus` (what `EnablePeerMesh` installs), and `Gateway.UseBus` to plug one in. `gat.MatchChannel` is the shared NATS-style matcher; `gw/gat/bustest` is a conformance suite for new buses.
- **`gw/gat/gatnats`** (experimental): `gatnats.New(nc)` is a `gat.Bus` over a NATS connection, in its own package so `gat` does not import nats.go.
- **Single-node pub/sub.** A gwag gateway without `WithCluster` installs the `ps` slot and serves `ps.pub` / `ps.sub` in-process.
- **Pub/sub degrade mode.** In cluster mode, when the NATS connection is down, `ps.pub` / `ps.sub` deliver on this node only and switch back to NATS on reconnect. New gauge `go_api_gateway_pubsub_bus_degraded`; `HealthHandler` body gains `pubsub` (`nats` / `local` / `local_degraded`) and `degraded: true` while degraded (status code unchanged). See docs/pubsub.md "Transport".

### Changed
- OpenAPI ingest: a nullable `$ref` (`allOf: [$ref]` or a one-branch `oneOf` with `nullable: true`, or the 3.1 `oneOf: [$ref, {type: null}]`) now becomes a nullable field of the referenced type. It used to fall back to a scalar, so the GraphQL type of such fields changes.
- **Subscription errors keep their extensions.** An error a subscription dispatcher returns before the stream starts (e.g. a proto stream over `MaxStreams`) now carries `extensions.code` on the GraphQL `error` frame (was message only).
- **Standalone gwag gateways** now have the `ps/v1` slot: the schema includes `ps.pub` (Query) and `ps_sub` / `ps_stable_sub` (Subscription), and `ps/v1` appears in admin service listings, service stats, and the MCP schema tools, as it already did in cluster mode. `WithChannelBinding` without a cluster no longer panics in `New`.
- **Channel patterns with a `>` that is not the last token** (e.g. in `WithChannelAuth` / `WithChannelBinding`) now match nothing, as in NATS and gat. gwag used to treat such a `>` as "match the rest". `New` now panics on such a `WithChannelAuth` pattern (or one with an empty token): the rule would match nothing, and its channels would fall back to the default HMAC tier, which can be weaker than the tier the rule declared.

### Fixed
- **Repeated and map fields of a proto request message** rendered as a scalar GraphQL arg (`xs: Int`), so a list could not be sent. They are now `[T]` args, and map fields take the JSON scalar.
- **Unset proto fields with presence** (proto2 `optional`, proto3 `optional`, oneof members) read as zero values on gwag's default GraphQL path, and a proto2 enum without a 0 value read as `0`. They now read `null`, as on the map path and in protojson.
- **Proto `map<K,V>` fields** in gRPC args and responses (gwag and gat) panicked in the codec. They now convert as a JSON object: string keys, values in proto3 JSON form (enum names, base64 bytes, 64-bit integers as strings).
- **`uint64` / `fixed64` args** above 2^63-1 were refused, and negative values wrapped. They now parse over the full unsigned range, and a negative value is an error. `uint32` args refuse negative values too.
- `ClusterOptions.ClientListen` with port 0 now picks a free port. natsd read 0 as its default 4222.
- **docs/pubsub.md** showed `ps.pub` as a `Mutation` returning `Boolean!` and `ps.sub` nested under `Subscription.ps`. The real fields are `Query.ps.pub(...)` returning `Gwag_Ps_V1_PubResponse` and `Subscription.ps_sub(...)`, with `ts` as a string and `payloadType` in camelCase.
- **docs/gat-pubsub.md** described gwag pub/sub as durable; it is NATS core, without persistence or replay.

## v2.6.0 — 2026-10-30

gwag's GraphQL and gRPC ingress now run on gat's: one request path, so gwag gains batching, Connect and gRPC-Web.

### Migration
- **gwag GraphQL clients that send malformed requests** now get 400 instead of 200 with errors. This covers invalid JSON, an empty query, and `variables` that is not an object.
- **gat `MaxOperations` refuses the whole request.** This applies to a document or to a batch counted as a whole. Before, the first n operations ran and the rest failed. Raise `MaxOperations` if a client sends more.
- **Status codes are more specific.**
  - gRPC clients of OpenAPI and GraphQL upstreams see AlreadyExists, Unavailable and similar codes where they saw InvalidArgument or Internal.
  - REST clients see the upstream's own HTTP status where they saw 400 or 500.
  - gat handlers that return a `*connect.Error` or a gRPC status keep that code where they got Internal.
- **Pub/sub auth failures** carry `PERMISSION_DENIED` in `extensions.code` (none before).

### Added
- **Batched GraphQL requests.** A POST body that is a JSON array runs each document in order and answers with an array of results; `WithMaxOperations(n)` also caps the batch: at most `n` documents, and at most `n` operations summed over them (over the cap, the whole batch is refused with 400 and nothing runs). (The GraphQL handler now runs on gat's request path.)
- **gat ingress hooks** (experimental). `PreRequest`, `WSUpgrade`, `GraphiQL`, `ParseContentType`, `AcceptStringVariables`, `UsePlanCache`, `EchoComplete` let a host wrap gat's GraphQL handler; see docs/gat.md "Ingress hooks". New `gat.Request`, `gat.RequestHook`, `gat.RequestParser`.
- **Connect and gRPC-Web ingress.** `ConnectHandler()` (experimental) serves the routes `GRPCUnknownHandler` serves over Connect, gRPC-Web and gRPC on an `http.Handler`. Both now run on gat's gRPC ingress; see docs/operations.md "gRPC, Connect and gRPC-Web".
- **gat gRPC routes from the host** (experimental). `SetGRPCRoutes` serves host-supplied methods (`gat.GRPCRoute`) over Connect, gRPC and gRPC-Web (`GRPCHandler`) and on a grpc-go server (`GRPCStreamHandler`); the table swaps atomically. `PreCall` hooks every gRPC call; `gat.ErrorCode` is the one error-to-code mapping both use. See docs/gat.md "gRPC routes you supply".
- **`WithConnectErrorCodes()`** (experimental). GraphQL dispatch errors carry gat's extensions shape, `{status, code: "not_found"}` (lower-case gRPC code, as the gRPC and REST ingresses classify the error), instead of `{code: "NOT_FOUND"}`. Default stays upper case until v3. New `ir.RuntimeOptions.WrapError`.
- **`gat.HTTPStatus(code)`** (experimental). A gRPC / Connect code as an HTTP status, by the Connect protocol's table.

### Changed
- **GraphQL endpoint: 400 on bad input.** A body that is not valid JSON, an empty or missing query, or `variables` that are not an object (a JSON string holding one is still accepted; invalid `variables` on GET included) now answer 400 instead of 200 with a GraphQL error or silently dropped variables.
- **GraphQL endpoint: other input changes.** A multipart body over `WithUploadLimit` answers 413 (was 400). On POST, `query` / `variables` in the URL no longer override the body. A WebSocket upgrade without the `graphql-transport-ws` subprotocol is accepted and closed with 1008 (was served as an HTTP request).
- **gat `MaxOperations` refuses the whole document.** A document selecting more operations than the cap now fails validation and nothing runs (was: the first `n` ran and the rest got field errors). In a batch the cap counts the operations of all its documents together: a batch over it is refused whole with 400.
- **gRPC ingress: more specific status codes.** An OpenAPI or downstream-GraphQL upstream's HTTP error status now picks the gRPC code (`gat.ErrorCode`): 409 AlreadyExists (was InvalidArgument), 412 FailedPrecondition, 413 ResourceExhausted, 501 Unimplemented, 502/503/504 Unavailable (were Internal). GraphQL `extensions.code` is unchanged.
- **REST ingress: upstream statuses relayed.** `IngressHandler` answers a failed dispatch with the upstream's own HTTP status (was: 4xx other than 400/401/403/404/429 became 400, every 5xx became 500). An error with no HTTP status uses `gat.HTTPStatus(gat.ErrorCode(err))`: a gRPC upstream's status picks the REST status (was 500 for all); a cancelled or timed-out call is 499 / 504 (was 500).
- **gat gRPC error codes.** A handler error that is a `*connect.Error` or carries a gRPC status keeps its code (was Internal unless it had an HTTP status); `context.Canceled` / `DeadlineExceeded` answer Canceled / DeadlineExceeded (was Internal). Wrapped errors are unwrapped.
- **Pub/sub auth failures carry a code.** A `ps.pub` / `ps.sub` refused by channel auth (HMAC or delegate) now fails with PermissionDenied: GraphQL `extensions.code` `PERMISSION_DENIED` (was none), gRPC PermissionDenied, REST 403. Messages are unchanged.
- **gat 413 carries `extensions.code`** (`BAD_REQUEST`), as gwag's does.

### Fixed
- **gRPC upstream timeouts recognised.** A gRPC upstream whose copy of the deadline reset the stream just before the gateway's own timer fired was reported as the upstream's DeadlineExceeded, not as the upstream timeout: no `upstream timeout after` message, and no count toward replica ejection.

## v2.5.0 — 2026-10-30

### Migration
- **gwag GraphQL endpoint limits.** A request selecting more than 64 operations is now refused, and a body over 1 MiB gets 413. If a client sends larger requests, set `WithMaxOperations(n)` / `WithMaxRequestBytes(n)`, or pass 0 to turn a limit off. gat had these defaults already.
- **gat with no schema.** A gat handler whose schema is not assembled yet answers 503 `NOT_READY` instead of 200 with an error.
- **Dependencies.** huma v2.40.0, grpc v1.85.0, mcp-go v1.4.0 and golang.org/x/net v0.62.0 are the new minimums.

### Added
- **`WithMetricPrefix(prefix)`.** Opt in to `gwag_*` Prometheus metric names; default stays `go_api_gateway_*` until v3.
- **Request limits.** `WithMaxOperations(n)`, `WithMaxRequestBytes(n)`, `WithMaxDepth(n)`, `WithoutIntrospection()` on the GraphQL endpoint; see docs/operations.md "Request limits". Shared with gat (`gw/internal/gqlguard`).
- **Trusted documents.** `WithTrustedDocuments(docs, enforce)` and `ParseTrustedDocuments`: persisted-document ids, Apollo APQ and GET by id, as in gat.
- **Upstream call timeouts.** `Timeout(d)` (per service),
  `WithUpstreamTimeout(d)` (gateway default) and
  `ServiceBinding.timeout_ms` (control plane) bound unary dispatch to
  gRPC, OpenAPI and downstream-GraphQL upstreams. Off by default.
- **Retry on a different replica** when a unary call provably never
  reached an upstream (connect refused, no connection). Delivered
  requests, 5xx and timeouts are never retried.
- **Replica ejection.** `WithReplicaEjection(failures, cooldown)`: a
  replica with 5 consecutive failures (default) gets no traffic for
  10 s (default). Fails open when every replica is ejected.
- **Integer scalars** (experimental). `gat.IntegerScalar(name, desc)` for a Go type's `huma.SchemaProvider`, or `x-gwag-scalar: <Name>` on an OpenAPI integer schema, renders that field as its own GraphQL scalar: a JSON number exact within ±(2^53-1), input as number or decimal string. REST stays integer/int64, proto int64. New `ir.TypeRef.ScalarName`, `ir.ScalarExtension`.
- **gat `VersionedOnly(true)`** (experimental). Render each GraphQL operation once, at `ns.v1.op`, without the unversioned `ns.op` copy. Default off. New `ir.RuntimeOptions.VersionedOnly`.
- **gat `SetSchema`** (experimental). Serve a host-assembled schema and replace it at run time; in-flight requests keep the schema they started with. A gat handler with no schema yet answers 503 `NOT_READY` (was 200 with a "Must provide schema" error).

### Security
- **Default request limits.** The GraphQL endpoint now refuses documents selecting more than 64 operations and bodies over 1 MiB (413). Raise or turn them off with `WithMaxOperations(0)` / `WithMaxRequestBytes(0)`.
- **GraphiQL no longer runs linked queries.** A browser GET with `?query=` executed the query server-side, outside the plan cache, so it skipped the request limits and the no-mutations-over-GET rule: a link could fire a mutation with the visitor's cookies. The page now only prefills the query.

### Fixed
- **Cluster puts retried.** Deprecate and MCP-config writes retry a put lost to a replica change instead of failing the admin call; the deprecated bucket now follows the cluster replica count.
- **uint64 proto fields.** A set `uint64` / `fixed64` field no longer panics on the map conversion path (gRPC ingress, subscriptions, pub/sub, runtime transforms); it renders as a decimal string.

## v2.4.0 — 2026-10-26

### Added
- **gat `@export` within one document.** A field reading a variable that
  another field of the same query exports runs after it: gat splits the
  document into stages at plan time and merges their data into one
  response in query field order. One request, one result type.
- **gat batched requests with `@export`.** A POST body that is an array
  of operations runs them in order; `@export(as: "x")` on a field passes
  its values (deduplicated; a list under a list) to the operations after
  it as `$x`. The answer to "fetch these, then the things they reference"
  in one round trip, without links or nested resolvers.
- **`ir.RuntimeOptions.Directives`** declares extra directives on the
  rendered schema; `ir.PrintSchemaSDL` prints them.
- **gat request attributes.** `gat.Attrs` is a bag scoped to the
  inbound request (GraphQL HTTP request, gRPC call, graphql-ws
  connection) that every fused operation shares;
  `gat.LockIfAbsentAndFill` computes a value once while concurrent
  callers wait, so a session lookup runs once per request, not once per
  operation. `gat.AttrsMiddleware` scopes REST requests.
- **gat parallel queries.** `ParallelQueries(n)` runs up to `n` query
  operations of one GraphQL request concurrently; mutations stay
  serial. Off by default.
- **gat request limits.** `MaxOperations(n)` caps operations per GraphQL
  request (aliases included; excess ones fail with 400, not dispatched).
  `MaxRequestBytes(n)` caps the GraphQL body.
- **gat trusted documents.** `TrustedDocuments(docs, enforce)` serves
  requests naming a document by `documentId`, the APQ `sha256Hash`
  extension, or `?documentId=` over GET; `enforce` refuses query text.
  `ParseTrustedDocuments` reads graphql-codegen's persisted-documents
  manifest.
- **gat combines fused operations' cache headers.** `Cache-Control`,
  `ETag`, `Last-Modified` and `Vary` from each operation's huma output
  merge into the GraphQL response: identical values pass through,
  otherwise the most restrictive combination; ETags combine into one
  weak ETag. GET with a matching `If-None-Match` gets 304.
- **gat `MaxDepth` and `DisableIntrospection`.** Validation rules run by
  the plan cache, so each distinct query is checked once.
  `DisableIntrospection` also stops serving `{prefix}/schema/*`.
- **gat `HookDispatch`.** A per-operation hook that receives the
  request context and returns the one the handler sees, plus a done
  callback with the error — for trace spans. No tracing dependency.

### Security
- **gwag refuses mutations over GET.** `GET /graphql?query=mutation…`
  ran the mutation upstream, cookies forwarded by `ForwardHeaders`
  included. It now gets 405 before execution, as GraphQL over HTTP
  requires.
- **gat refuses mutations over GET.** `GET {prefix}/graphql?query=mutation…`
  ran the mutation, so any page could trigger one with a visitor's
  cookies (a link or an `<img src>`; `SameSite=Lax` still sends cookies
  on top-level GETs). A GET whose operation is a mutation now gets 405
  before anything runs; GraphQL over HTTP forbids them there.

### Fixed
- **OpenAPI array query parameters and array responses keep their list
  type.** Ingest dropped it: `?ids=a&ids=b` became a `String` argument
  and a JSON-array response a single object (fields read as null), in
  gat and gwag, over GraphQL and gRPC. Outbound OpenAPI calls now send a
  list argument as repeated keys instead of `ids=[a b]`, and gat's gRPC
  ingress answers list operations under the response message's `value`.
- **gat GET requests read `variables` and `extensions`.** They were
  ignored, so a GET query with variables failed.
- **gat `ObserveDispatch` now fires for gRPC calls.** gRPC ingress
  invoked handlers directly and was never observed; observation now
  wraps the handler invocation both surfaces share.

### Changed
- **GraphQL and proto types change for OpenAPI array parameters and
  array responses** (the fix above): `ids: String` → `ids: [String!]`,
  `listThings: Thing` → `listThings: [Thing!]`. Regenerate clients.
- **gat core API is stable**: `Gateway`, `New`, `ServiceRegistration`,
  `Handler`, `Register`, `RegisterHuma`, `RegisterSSE`, `RegisterGRPC`,
  `HandleMux`, and the `/graphql`, `/schema/*` and gRPC routes. The
  rest of `gw/gat` stays experimental (`docs/stability.md`).
- **gat runs at most 64 operations per GraphQL request** by default
  (`MaxOperations`); excess aliases fail with 400 instead of
  dispatching. `MaxOperations(0)` removes the cap.
- **gat refuses GraphQL bodies over 1 MiB with 413** (`MaxRequestBytes`
  default), matching huma's REST limit for the same operation. Set
  `MaxRequestBytes(0)` for no limit.
- **graphql-go fork v2.1.0**: `PlanCacheOptions.Rules`, `Plan.IsMutation`.
- **gat allocates less per request.** Mounted endpoints use the
  adapter's own request on humago/humachi/humamux/humaflow, and binary
  proto requests decode straight into the input struct (gatbench:
  GraphQL Get 88 → 74 allocs/op, Connect proto Get 69 → 59).

## v2.3.1 — 2026-10-22

### Security
- **gat runs huma's input checks on GraphQL and gRPC calls.** Parameter
  and body schema validation (`maxLength`, `pattern`, `minimum`, `enum`,
  formats, ...) and input `Resolve` methods ran over REST only; GraphQL
  and gRPC-ingress calls (unary, `RegisterSSE`, `Subscribe`) reached the
  handler with input REST rejects. A failure is now a 422, or the status
  a Resolver error carries, as GraphQL `extensions.status` or the mapped
  gRPC code.

## v2.3.0 — 2026-10-22

### Added
- **gat: `RegisterSSE`, the paired drop-in for huma's `sse.Register`.**
  One handler serves REST (Server-Sent Events), a GraphQL subscription
  over `graphql-transport-ws`, and a gRPC server-streaming method on
  `RegisterGRPC`, behind the op's huma middlewares. Several event types
  render as a GraphQL union and a proto message with one `oneof`.

### Deprecated
- **gat: `Subscribe`.** Use `RegisterSSE`, which defines the stream in
  huma and also serves it over REST.

### Fixed
- **Proto rendering of unions.** A service with a union type (OpenAPI
  `oneOf`) produced a descriptor set that failed to build
  (`"….AB" not found`), so gat's `RegisterGRPC` failed for it. A
  union now renders as a message holding one `oneof value`.
- **gat: `Subscribe` streams over gRPC.** v2.2.0 listed them as
  server-streaming methods in `/schema/proto` but `RegisterGRPC` did
  not mount them; they are now served.

## v2.2.0 — 2026-10-21

### Added
- **gat: `Subscribe` registers GraphQL subscriptions.** `gat.Subscribe[I, O]`
  takes a huma operation and a source `func(ctx, *I) (<-chan *O, error)`;
  the field is served over `graphql-transport-ws` on `{prefix}/graphql`
  (WebSocket upgrade on `GET`), with the op's huma middlewares applied.
  `gat.ConnectionInitPayload(ctx)` exposes the `connection_init` payload.

## v2.1.3 — 2026-10-21

### Security
- **gat: huma middlewares now run on GraphQL and gRPC dispatch.** An op
  registered with `gat.Register` called its handler directly over
  `/graphql` and gRPC ingress, skipping both `api.UseMiddleware` and
  `huma.Operation.Middlewares`. An op guarded only by a huma middleware
  (auth, role check) was open on those surfaces. The chain now runs in
  huma's order; a middleware that writes an error instead of calling
  `next` becomes a GraphQL error (with `extensions.status`) or the
  mapped Connect code. Middlewares see the transport request's headers,
  `Method()` is the op's method, `Param()` returns `""`, and
  `BodyReader()` is empty.

## v2.1.2 — 2026-10-11

### Fixed
- **SDL descriptions ending in `"` now parse.** A one-line description
  like `e.g. "match 0.97"` rendered as `"""…"match 0.97""""`, which
  closes the block string early; graphql-js and codegen rejected the
  whole schema. Such descriptions now use the multi-line form. An
  embedded `"""` is now escaped as `\"""` (was `\"\"\"`, which put
  the backslashes into the parsed description).
- **`RetractStable` and cluster `Register` survive a KV replica
  change.** The gateway raises the KV buckets' replica count as peers
  join; a put sent during that change could get no ack and fail after
  5s. For a retract, the local retract stayed applied but was not
  persisted; for a registration, the call failed. Both puts now retry
  with a 2s per-attempt timeout inside a 10s budget. Heartbeat TTL
  refreshes retry inside their existing 5s budget.

### Changed
- Go dependencies updated to current minor/patch versions: nats-server
  v2.15.1, nats.go v1.54.0, grpc v1.84.0, otel v1.47.0, mcp-go v1.2.1,
  prometheus client_golang v1.25.0, protoreflect v1.19.0 (v2 module
  v2.0.0 stable), golang.org/x/net v0.61.0. No source changes.
- UI: graphql 17; graphql-request replaced by a fetch client. GraphQL
  errors now read as the error text joined with "; ".

## v2.1.1 — 2026-09-15

### Fixed
- **Subscriptions no longer leak a goroutine per cancelled subscriber.**
  graphql-go v2.0.0 → v2.0.1. `runSubscription` stops reading once its
  context is cancelled; graphql-go's producer then blocked forever on
  its next result send, and its result channel never closed.
- **Cluster tracking retries after a failed start, and `Close` no longer
  hangs.** If the initial self-registration Put failed (for example,
  timing out while JetStream settles at boot), the half-started tracker
  stayed installed. The boot retry loop saw it and stopped, so the
  gateway ran without peer refresh, the reconciler or KV watches, and
  `Close` blocked forever waiting on loops that never started. `Close`
  also no longer hangs when it runs while that start is still in flight,
  as it can against the background start `ControlPlane` launches.

## v2.1.0 — 2026-09-02

### Changed
- **mcp-go v0.52.0 → v1.0.0, kin-openapi 0.138 → 0.149.** Both are
  0.x-major bumps and needed one-line source changes inside gwag. No
  gwag API changed.

## v2.0.1 — 2026-09-02

### Fixed
- **`gat` GraphQL responses set cookies again.** The handler emitted
  the cookie sink before it executed the plan, so the sink was always
  empty and every `Set-Cookie` was dropped. The request still returned
  200 with its data; only the header was missing, so a login mutation
  reported success and set no session. The REST surface was not
  affected. Present from v1.4.1 through v2.0.0.

### Changed
- Go and UI dependencies updated to current minor/patch versions.

## v2.0.0 — 2026-09-01

### Changed
- **Import paths gain a `/v2` suffix.**
  `github.com/iodesystems/gwag` → `github.com/iodesystems/gwag/v2`.
  Required, not cosmetic: gwag's exported API is typed in graphql-go
  values — `*graphql.Scalar`, `graphql.Output`, `*graphql.Schema`,
  `graphql.Fields`, `*graphql.ResolveInfo` — and those types now come
  from `github.com/IodeSystems/graphql-go/v2`. To Go, a type from the
  v1 path and the same type from the v2 path are different types, so a
  caller holding a v1 `*graphql.Scalar` can no longer pass it in. That
  is a MAJOR break under `docs/stability.md`, and v1.4.1 consumers stay
  frozen where they are until they choose to move.

- **graphql-go v1.2.0 → v2.0.0.** `ExecuteParams.ConcurrentThunks` was
  removed upstream; gwag never set it, so nothing here changed but the
  path.

### Fixed
- **Thunked resolvers no longer serialize under append-mode execution.**
  graphql-go's append walker awaited each thunk as its own resolver
  returned, so resolvers that start a goroutine and return a thunk
  waiting on it ran end to end instead of overlapping. A selection set
  now resolves every field before awaiting any thunk. Measured on 20
  fields sleeping 1 ms each: 21.8 ms → 1.49 ms.

### Added
- **`graphql.DoWriter` / `graphql.DoAppend`** are available from
  graphql-go v2 for writing a response body without building the
  `map[string]interface{}` result tree. gwag does not use them yet.

### Performance
- Plan-time allocation in graphql-go dropped: response-key JSON is
  built once per selection set on first append-mode use rather than
  once per field at plan time. `PlanQuery` on a 100-field query went
  325 → 224 allocs/op.

## v1.4.1 — 2026-08-18

### Security
- **The GraphQL parser now caps nesting depth.** Via
  `github.com/IodeSystems/graphql-go` v1.1.0 → v1.2.0. The parser
  descended one Go stack frame per nesting level with no limit, and a
  stack overflow in Go is a fatal runtime error that `recover` cannot
  catch — so a deeply enough nested document killed the gateway process
  rather than failing the request. Roughly three million levels (a ~6 MB
  body) were needed to reach it.

  `WithUploadLimit` did not stand in the way: it defaults to unlimited,
  and it gates the multipart and tus paths, not a plain
  `application/json` GraphQL POST. A stock gateway was exposed.

  The cap is `parser.DefaultMaxDepth` (1000), and gwag picks it up with
  no configuration — it parses with zero-valued `ParseOptions`, which
  selects the default. The limit counts *descent* only: sibling fields,
  list elements and object fields cost no depth, so wide documents are
  unaffected, and 1000 sits far above any real query. Selection sets,
  list values, input-object values and list types each recurse
  independently and all four are capped. Set `ParseOptions.MaxDepth`
  negative to disable the check for trusted input.
- **Three more hostile-query blow-ups are closed** in the same bump:
  syntax-error formatting is linear rather than quadratic,
  abstract-field selections plan lazily instead of exponentially, and
  the overlapping-fields validator terminates on cyclic fragment
  spreads. Each is driven by the query document alone.

## v1.4.0 — 2026-08-17

### Changed
- **gat's GraphQL ingress answers `200` for a query that resolved to
  errors**, where it previously answered `400`. An errors envelope is a
  successful transport exchange under GraphQL-over-HTTP, and this is
  what the full `gw/` gateway has always done — gat was the outlier.
  Transport-level rejections are unaffected: a body that isn't JSON, or
  one carrying no query, is still a `400`. Clients that branch on the
  HTTP status to detect a failed query must read the `errors` key
  instead.
- **gat's GraphQL ingress runs on a plan cache + append-mode executor.**
  It served every request through `graphql.Do` — re-parsing, re-validating
  and re-planning the query each time, then building a `map[string]any`
  result tree to encode. It now looks the query up in a
  `graphql.PlanCache` and runs `ExecutePlanAppend`, writing response JSON
  straight into a pooled buffer, the same path `gw/` moved to in v1.0.0.
  Measured on a 25-row list: 535µs → 119µs, 2180 → 462 allocs; a point
  lookup goes 271µs → 25µs, 1041 → 89 allocs. GraphQL error `extensions`
  (`status` / `code`) still reach the response envelope.
- **gat's Connect/gRPC ingress no longer routes through JSON or an
  intermediate map.** Requests were marshalled to JSON text with
  `protojson` and parsed back into a `map[string]any`, which was then
  bound into the handler's input struct by tag; responses went the other
  way, `json.Marshal` then `protojson.Unmarshal`. Three mount-time plans
  replace all of it: `bindPlan` writes proto fields straight into the
  input struct through cached field paths, `messageToArgs` walks a
  message directly when the bind plan declines, and an encode plan
  writes the response.

  Dropping the map also drops the repair work it required — `assignValue`
  existed largely to undo protojson's encoding, parsing a 64-bit integer
  back out of the string `"5"`. Reading the message directly means an
  int64 is an int64 the whole way.

  Each plan refuses to form for anything it cannot prove it handles
  identically — enums, proto maps, and any Go type with a custom
  JSON/text marshaler or unmarshaler (`time.Time`, `json.RawMessage`) —
  and that operation keeps the older path. Correctness never depends on
  the analysis being clever; only speed does. All three are held against
  their predecessors by differential tests, because `bindInput` silently
  skips what it cannot map and a divergence would surface as a handler
  reading a zero value rather than as an error.

  The per-request scan for the body argument also moved to mount time.
- **gat writes Connect responses straight to proto wire bytes.** The
  response was assembled as a `dynamicpb` message and handed to
  `proto.Marshal`. A memory profile of a 25-row response put roughly
  four fifths of the request's allocations in that arrangement:
  `dynamicpb.NewMessage` once per row, `protoreflect.Value` boxing per
  field, and proto's reflective `marshalMessageSlow`, which is the only
  marshaller available because `dynamicpb` carries no generated fast
  path. The encode plan already knows which Go field feeds which proto
  field, so the bytes are now emitted directly and no message is built.

  gat also mounts its own binary codec, which passes `AllowPartial` to
  `proto.Marshal` on the paths that still need it. proto3 has no
  required fields, so the initialization check it skips could only ever
  succeed while walking the whole message to find that out.

  Emitting the wire format means owning its rules — implicit presence,
  packed versus unpacked repeated fields, length-prefixed nested
  messages, sign-extended negative 32-bit varints. Each is asserted
  byte-for-byte against `proto.Marshal` rather than reasoned about,
  since a mistake yields output that still parses but means something
  else. Responses whose encode plan declines still take the old path.

  On a 25-row list: **Connect binary 100µs → 30µs, 429 → 129 allocs;
  gRPC-Web 87µs → 32µs, 390 → 137 allocs.** That moves gat from ~4.8×
  a hand-written connect-go service to ~1.4×. The JSON codec is
  unchanged — protojson needs a real message, so that path still
  materialises one.

- **The maintainer benchmark directory `perf/` is now `compare/`.**
  `bench/` and `perf/` are synonyms and neither name said which was
  which. `bench/` measures gwag against itself (renders `docs/perf.md`,
  driven by `bin/bench`); `compare/` measures gwag and gat against other
  tools (renders `compare/comparison.md` and
  `compare/gatbench/results.md`, driven by `compare/run.sh`). No shipped
  artefact moves — `docs/perf.md`, `bench/perf-scenarios.yaml` and the
  `bin/bench perf` subcommand keep their names.

### Fixed
- **gat's OpenAPI dispatch reused Go's `http.DefaultClient`**, whose
  `MaxIdleConnsPerHost` is 2. Past two concurrent in-flight requests it
  fell back to a fresh connection and TCP handshake per dispatch, so
  connection churn — not the upstream — became the ceiling. In the
  competitor sweep gat's OpenAPI scenario walled at 20k RPS with p99
  74ms where gwag, which has always pre-baked a tuned client for this
  exact reason, reached 50k. gat now uses the same defaults
  (`MaxIdleConnsPerHost` 1024, 90s idle timeout), shared package-wide
  so every dispatcher draws on one pool.

### Added
- `compare/gatbench/` — a separate Go module benchmarking gat's three
  surfaces against the tools you'd otherwise hand-write each with
  (gqlgen, connect-go, grpc-gateway) on one shared workload. Kept out
  of the root module so those dependencies stay out of every downstream
  `go.sum`. Results are spliced into `compare/README.md` and the root
  README by `compare/gatbench/report.sh`, so the published tables can't
  drift from the numbers.

## v1.3.2 — 2026-07-26

### Fixed
- gat now binds request parameters promoted out of an **exported embedded
  struct**. huma documents and binds them, and gat rendered them into the
  GraphQL schema and the proto request message — but `bindInput` matched
  tags against top-level field indices only, so the argument arrived and
  was silently dropped. Same zero-value failure as the unexported-embed
  trap, one layer down and on the GraphQL/gRPC path specifically. Tag
  lookup now carries a field path and descends exported embeds (matching
  huma), allocating pointer-embedded structs on the way.

## v1.3.1 — 2026-07-26

### Fixed
- **The v1.3.0 embedded-parameter check was over-broad and rejected
  working code.** The real huma rule is *exportedness*, not embedding:
  `_findInType` skips `!f.IsExported()` before it reads a tag, and an
  embedded field takes its name from its type — so embedding an
  **unexported** type (`scope`) hides every parameter inside it, while an
  **exported** embed (`Scope`) is documented and bound correctly. v1.3.0
  refused to mount on both. It now reports only what huma genuinely
  cannot reach: a parameter on an unexported field, or one sitting inside
  an unexported embedded struct (exportedness does not recover below an
  unexported embed). Error text and `docs/gat.md` corrected — the fix is
  to export the type, not to flatten the struct.

### Added
- `gat.CheckEmbeddedParams(types...)` — the same detector, exported, for
  inputs gat never captures: operations registered with plain
  `huma.Register`, or a huma codebase not using gat at all. gat cannot
  recover those Go types from the OpenAPI document precisely because the
  parameter is missing from it, so this is a test assertion you call
  yourself.

## v1.3.0 — 2026-07-26

### Added
- gat now **refuses to mount** when an operation declares request
  parameters on an anonymous embedded struct, naming every lost
  parameter and the fix. huma does not read `query` / `path` / `header`
  / `cookie` tags from an embedded struct — they are absent from the
  OpenAPI document *and* are never bound at runtime, so the handler
  silently receives zero values and any default it falls back to looks
  like correct behaviour. gat cannot repair it (huma owns parameter
  discovery) but it is the layer that notices, because GraphQL and proto
  are built from that same document. `g.AllowEmbeddedParams(true)`
  downgrades the check to a log warning while an existing codebase is
  flattened. Documented in `docs/gat.md` with a pointer from the README.

  This can newly fail a gateway that previously started — hence a minor,
  not a patch. If it fires, those parameters were already being dropped.

### Fixed
- UI build (`bin/build`) no longer falls back to `ui/fallback/`. Two
  generated-type aliases indexed `[number]` into a nullable array
  (`NonNullable<X['services'][number]>` instead of
  `NonNullable<X['services']>[number]`), which failed `tsc` and cascaded
  into implicit-`any` errors; fixing the types then surfaced a genuinely
  unguarded `inj.landings` (nullable in the schema) on the injectors
  page, which now renders the same placeholder for null as for empty.

## v1.2.1 — 2026-07-26

### Fixed
- Enum values now survive a round-trip into a dynamic proto message.
  `renderProtoEnum` prefixes the descriptor spelling with the enum type
  (`ADMIN` → `Role_ADMIN`) because proto scopes enum *value* names to
  the enclosing package, so two enums sharing a value would collide at
  files-registry build. Every other surface speaks the bare name, but
  both value converters looked the bare name up directly against the
  prefixed descriptor and failed with `unknown enum value "ADMIN" for
  pets.v1.Role` — any cross-format call carrying an enum into gRPC
  errored. New `ir.EnumValueByExternalName` bridges the two spellings
  (proto-origin enums keep verbatim names, so both are tried) and both
  converters go through it.
- Proto rendering of map fields. `map<K,V>` has no standalone type in
  proto3 — it is sugar for a nested `<Field>Entry` message marked
  `map_entry` plus a `repeated` field — and the canonical render path
  left that arm unimplemented. Because the empty `case` shadowed the
  `default` arm, a map produced a field with **no type at all** and
  `FileDescriptorSet` construction failed with `cannot resolve type:
  invalid name reference: ""`, so a gat server with a Go `map` anywhere
  in a request or response body would not start. Every other shape
  proto cannot express degrades to `string`; maps were the one that
  turned that into a hard failure.

  Maps now render properly, including **arbitrary nesting**:
  `map<string, map<string, map<string, T>>>` boxes each inner map in a
  synthesised `<Field>Value` wrapper, since proto3 forbids a map as a
  map value. Non-integral map keys degrade to `string` (proto restricts
  keys to integral and string types), and a value shape proto still
  cannot express degrades to `string` rather than to nothing.

## v1.2.0 — 2026-07-13

### Fixed
- OpenAPI / MCP ingest now numbers synthesised enum values 0-based in
  declaration order instead of leaving every value at `0`. The proto
  renderer reads `EnumValue.Number` verbatim, so the old all-zero
  numbering failed `FileDescriptorSet` construction ("conflicting
  non-aliased values on number 0") and kept a gat server from starting
  whenever an ingested schema had a string enum field.
- OpenAPI `nullable: true` (and the 3.1 `type: [..., "null"]` spelling)
  now maps onto GraphQL nullability: a field is rendered non-null only
  when it is required *and* not nullable. A required-but-nullable field
  previously rendered `T!` and errored at query time when its value was
  null.
- `{prefix}/schema/graphql` now answers introspection `POST` queries by
  delegating to the GraphQL execution handler (the `GET` view still
  serves SDL / `?format=json`). graphql-codegen and similar tools
  introspect via `POST`, so they can target the documented schema URL
  instead of getting a 405.

## v1.1.0 — 2026-05-31

### Added
- Admin UI "Sign Token" panel — a GUI for `gwag sign`. A form (channel,
  TTL, optional kid) calls the existing `signSubscriptionToken` admin op
  and shows the returned hmac / timestamp / kid for an operator to hand
  to a subscription client. Bearer-gated like the other admin writes.
- `gateway.WithDestructiveReads(paths...)` — opt specific GET/HEAD paths
  into the admin-auth gate (for reads that have side effects or expose
  sensitive data), instead of `AdminMiddleware` treating every GET as a
  public read. Exact match, or prefix with a trailing `*`. The gate
  applies wherever the path is reached, including the `admin_*` GraphQL
  field that dispatches a GET to it. See
  [`docs/admin-auth.md`](./docs/admin-auth.md).
- Global `gwag` context fallback. When a directory has no project-local
  `./.gw/credentials.json`, login resolution falls back to
  `~/.config/gwag/credentials.json` (XDG_CONFIG_HOME-aware) — kubectl-
  style, so a login set once globally applies in every directory unless
  a project overrides it. `gwag login` / `logout` / `use` take `--global`
  to manage that file; `gwag context` prints the resolved source path;
  `gwag up` still writes the project-local context.
- OAuth/JWT token-exchange outbound auth. `gateway.TokenExchangeClient`
  / `TokenExchangeConfig` / `NewTokenExchangeTransport` perform RFC 8693
  token exchange: the inbound caller's bearer (read from the HTTP request
  or the gRPC ingress metadata) is swapped at a configurable issuer for
  an upstream-scoped token, cached until shortly before expiry (bounded
  by `MaxCachedTokens`; concurrent misses coalesce via singleflight
  without holding the cache lock during the network call) and attached
  outbound.
  Preserves caller identity while re-minting for the upstream's
  audience — the third outbound-identity mode next to `ForwardHeaders`
  (passthrough) and `ServiceAccountTransport` (gateway's own identity).
  Install via `WithOpenAPIClient` / `OpenAPIClient`. See
  [`docs/admin-auth.md`](./docs/admin-auth.md).
- Service-account outbound auth helper. `gateway.ServiceAccountTransport`
  (an `http.RoundTripper`) + the `ServiceAccountClient` shortcut and
  `TokenSource` / `StaticToken` attach the gateway's *own* credential to
  outbound OpenAPI / downstream-GraphQL dispatches — distinct from
  forwarding the caller's token. Install via `WithOpenAPIClient` /
  `OpenAPIClient`. Defaults to `Authorization: Bearer <token>`; header +
  scheme are configurable. See [`docs/admin-auth.md`](./docs/admin-auth.md).
- `gwag --admin-data-dir DIR` flag. Persists (and reloads) the boot
  admin token to `<DIR>/admin-token` for ad-hoc `gwag` startups, mirroring
  the example gateway and `gwag up`. A stable token means
  `gwag sign --gateway --bearer $(cat <DIR>/admin-token)` works against a
  local gateway instead of falling back to local `--secret HEX` signing.
- Static `--openapi` / `--graphql` registration flags for the `gwag`
  command, alongside the existing `--proto`. `--openapi SPEC=[NS@]URL`
  ingests a spec (file or http URL) and forwards to the upstream base;
  `--graphql [NS@]URL` introspects a downstream GraphQL endpoint at
  boot. Both repeatable; compose with `--control-plane` runtime
  registration. A CLI-driven static pipeline no longer needs the
  control plane for those two kinds.
- SDL-visible annotation carriage. An upstream author can attach
  custom directives to an operation / type / field — `x-gwag-annotations`
  on an OpenAPI (incl. huma) operation or schema, or a `@gql Name(args)`
  proto leading comment — and gwag carries them verbatim into every
  egress: real GraphQL SDL directives (with synthesized `directive`
  declarations so the document stays valid), `x-gwag-annotations` on the
  rendered OpenAPI, and `@gql` comments in the proto SDL view. Metadata
  only; runtime policy stays in transforms. New surface: `ir.Annotation`
  / `ir.AnnotationArg`, `ir.Operation/Type/Field.Annotations`,
  `ir.AnnotationIndex` + `ir.NewAnnotationIndex`,
  `ir.RuntimeOptions.AnnotationSink`, and `ir.PrintSchemaSDL` now takes
  an optional `*AnnotationIndex`. Two limits: GraphQL is a destination
  only (introspection can't carry applied directives, so downstream
  GraphQL directives can't be read), and the directives appear in the
  served SDL but not the introspection JSON. See
  [`docs/directives.md`](./docs/directives.md).
- `@ref` source-of-truth markers. A `@ref
  <workspace-relative-path>[:<symbol>]` Doxygen tag an upstream author
  attaches to an operation / type / field is now captured at ingest
  (proto leading comment, OpenAPI `x-ref` extension, or GraphQL
  description line) into the new `ir.Operation/Type/Field.Ref` field and
  re-emitted across every format the gateway serves: a `@ref` line in
  the GraphQL SDL + introspection descriptions, `x-ref` on OpenAPI
  operations, and a method leading comment in the proto SDL view. The
  marker is propagated, never synthesized — it carries through unchanged
  so [tslsmcp](https://github.com/iodesystems/tslsmcp) can link
  generated artifacts back to source. Purely informational; runtime
  behavior unchanged. First marker wins (multi-source deferred).
- gat pubsub. The embedded `gat` translator gains a publish/subscribe
  primitive — `Gateway.PubSub()` with `Publish` / `Subscribe` and
  NATS-style channel patterns (`*` one segment, `>` the rest),
  best-effort buffered delivery. `EnablePeerMesh(PeerMesh{Peers,
  Auth})` adds best-effort cross-node fanout: `Publish` fire-and-
  forgets the event to every peer gat over HTTP (HMAC-signed), and
  each peer fans it out local-only — one hop, no re-broadcast.
  Per-peer bounded queue + circuit breaker so a dead peer never
  stalls the publisher. `RegisterHTTP` / `RegisterHuma` mount a
  WebSocket subscribe stream at
  `{prefix}/_gat/subscribe?channel=PATTERN`, plus the internal
  peer-receive endpoint. `EnableSubscribeAuth(secret)` gates the
  subscribe endpoint behind HMAC tokens minted by
  `gat.SignSubscribeToken` — a key independent of the peer-mesh
  `Auth`. `(*gat.Gateway).SignSubscribeToken(channel)` is the
  import-boundary method form: server-side code holding the gateway
  signs against the configured secret without threading it (errors if
  `EnableSubscribeAuth` is unset). `gat.Gateway` gains `Close()`. See
  [`docs/gat-pubsub.md`](./docs/gat-pubsub.md). Static peer list
  only — a dynamic `PeerProvider` is a followup.

### Changed
- OpenAPI ingest now maps inline (non-`$ref`) object variants in a
  `oneOf` / `anyOf` to real GraphQL union members instead of collapsing
  the union to a JSON scalar. Each inline object variant is synthesised
  as a named type (`<hint>VariantN`), so it also round-trips back out as
  a named OpenAPI component. Non-object variants (scalars) still fall
  back, since a GraphQL union can only hold objects.
- Outbound dispatches now propagate distributed-tracing / correlation
  headers (W3C `traceparent` / `tracestate` / `baggage`, B3,
  `x-request-id`, AWS / GCP trace headers) from the inbound request, so
  a trace survives the gateway hop without configuring OpenTelemetry.
  OpenAPI / downstream-GraphQL forward them as HTTP headers alongside
  the auth allowlist; proto / gRPC dispatch bridges them onto the
  outbound gRPC metadata (inbound source is the HTTP request, or the
  incoming gRPC metadata on the gRPC ingress). An OTel `TracerProvider`,
  when set, still overwrites `traceparent` with the gateway's span
  context. An empty `ForwardHeaders()` opts out of HTTP forwarding,
  trace headers included.

### Removed
- **BREAKING (SemVer deviation):** `ir.RenderGraphQL(svcs) string` is
  removed. It shipped `// Stability: stable` in v1.0.0, so strictly this
  is a 2.0 change — we are making a deliberate exception in this minor
  because it had **zero non-test callers** and silently disagreed with
  the served schema (flat output on `Query`, skipping the
  namespace/version projection and descriptions the runtime path
  applies). The served `/schema/graphql` SDL has always come from the
  runtime path, not this function. **Migration:** build the schema with
  `ir.RenderGraphQLRuntime(svcs, registry, opts)` and serialize with
  `ir.PrintSchemaSDL(schema)`. If you need the old flat renderer, pin to
  `v1.0.x`.

### Fixed
- gat embedded dispatch — in-process huma handler results are now
  JSON-normalized before GraphQL resolution. Previously the raw Go
  value was handed to the graphql-go runtime, so custom
  `json.Marshaler` types and sealed-union responses (`oneOf` +
  discriminator) could not resolve. (`gw/gat/dispatch_inproc.go`)
- gat OpenAPI-origin enums — the graphql-go runtime value for an enum
  member now binds to the enum name (string) rather than the default
  ordinal number, so enum fields in JSON-shaped data resolve instead
  of returning null. (`gw/ir/render_graphql_runtime.go`)
- gat bind — protojson-encoded numbers (int64/uint64 land as JSON
  strings to preserve precision past `Number.MAX_SAFE_INTEGER`) now
  coerce into numeric Go fields with bounds-checked `strconv` parsing,
  both at top-level args and recursively through nested body structs
  and slices. Consumer huma input structs can declare `int64` /
  `uint64` fields directly without ad-hoc `string` shims at every
  call site. (`gw/gat/dispatch_inproc.go`)

## v1.0.0 — 2026-05-16

### Added
- Downstream MCP server ingestion — a fourth ingest kind alongside
  proto / OpenAPI / GraphQL. `gw.AddMCP(transport, target, opts...)`
  connects to an MCP server, runs `tools/list`, and re-exposes each
  tool as a GraphQL Mutation; `tools/call` dispatches at request
  time. Transports: `MCPStdio` (subprocess), `MCPHTTP` (Streamable
  HTTP), `MCPSSE`. CLI: `gwag --mcp-upstream NS:TRANSPORT:TARGET`
  (repeatable, also on `gwag up`). New public symbols `gw.AddMCP`,
  `gw.MCPTransport` + `MCPStdio` / `MCPHTTP` / `MCPSSE`, `ir.IngestMCP`,
  `ir.KindMCP`, `ir.MCPToolOrigin`. Tools-only — resources and
  prompts are not ingested. See [`docs/mcp.md`](./docs/mcp.md).
- `gwag --mcp` mounts the outbound MCP server at `/mcp` on the bare
  `gwag` command (previously only `gwag serve` / `gwag up`).
- Proto `bytes` field → `Upload` arg binding. Mark a `bytes` field
  with `[(gwag.upload.v1.upload) = true]` (extension declared at
  `gwag/upload/v1/options.proto`) and the gateway exposes it as a
  GraphQL `Upload` arg; the proto dispatcher reads the upload body
  (inline graphql-multipart-spec or tus-staged) into the field,
  capped by `WithUploadLimit`. Closes the proto-side gap in the
  upload story — OpenAPI `multipart/form-data` already mapped to
  `Upload`. New IR helper `ir.IsUploadField`; new public symbol
  `gw/proto/upload/v1.E_Upload`. Worked end-to-end in
  `gw/upload_proto_test.go`.
- `ir.AppendDispatcher` (gw/ir/dispatch.go) — optional capability
  interface for dispatchers that can emit their result as JSON bytes
  directly. The runtime renderer prefers `DispatchAppend` over
  `Dispatch` when a registered dispatcher implements it, routing
  through the fork's `ExecutePlanAppend` walker's `ResolveAppend`
  fast path (no per-field resolver tree, no full-tree map
  allocation, no leaf-emitter coercion round-trip). The built-in
  `protoDispatcher`, `openAPIDispatcher`, `graphQLDispatcher` (flat-
  upstream forwarder), and `graphqlGroupDispatcher` (nested-
  namespace forwarder) all implement it; the middleware chain
  (`backpressureMiddleware`, `quotaMiddleware`,
  `callerIDEnforceMiddleware`) passes the capability through. Adopters
  with BYO dispatchers (`gat.ServiceRegistration.Dispatchers`) opt in
  by implementing the method; plain `Dispatcher` continues to work
  unchanged.

  Measured impact (Ryzen 9 3900X, loopback, -benchtime=2s, single
  greeter / pets-style schema):

  | Bench | Before (Dispatch) | After (DispatchAppend) | Speedup |
  |---|---|---|---|
  | `BenchmarkProtoSchemaExec*`   | 436 µs / 68 KB / 937 allocs | 201 µs / 11.5 KB / 187 allocs | 2.2× / 5.9× / 5.0× |
  | `BenchmarkOpenAPISchemaExec*` | 391 µs / 68 KB / 937 allocs | 151 µs / 8.9 KB / 102 allocs | 2.6× / 7.6× / 9.2× |

  Fields whose return type contains a Union or Interface take the
  append path on graphql-origin (upstream resolves `__typename`,
  `prefixResponseTypenames` rewrites to the local prefix) and on
  openAPI-origin (the walker carries IR Service + type prefix to
  discriminate variants via `DiscriminatorProperty` / `Mapping`, then
  a required-fields heuristic, mirroring `IRTypeBuilder.unionFor`'s
  `ResolveType`). Proto-origin abstract returns stay on the Dispatch
  path until proto ingest synthesizes `TypeUnion` and the
  dispatcher carries the discriminator closure.

  New helper files: `gw/proto_append.go` (proto-message → JSON walker
  with selection projection + snake_case→camelCase renaming),
  `gw/openapi_append.go` (JSON-byte projector walking the local
  selection AST, type-aware for Union dispatch).
- `gwag serve --graphql URL [--mcp] [--mcp-include GLOB]`
  (gw/cmd/gwag/serve.go) fronts one downstream GraphQL service with
  the full gateway — metrics, backpressure, subscription proxy, and
  an optional `/mcp` mount. `--openapi` and `--proto` keep using the
  lighter gat translator. README "How to use it" lists all four
  shapes (standalone, gat, MCP exposure, CLI shortcut).
- `gw.WithMCPInclude(globs...)` / `gw.WithMCPExclude(globs...)` /
  `gw.WithMCPAutoInclude()` (gw/gateway.go) seed the MCP allowlist at
  construction. Operators declare which operations agents can call
  before any runtime admin edit, so the `/mcp` endpoint serves a
  meaningful surface from the first request without an out-of-band
  admin POST. Globs are dot-segmented (`*` matches one segment, `**`
  matches any number). Internal `_*` namespaces remain filtered first;
  neither Include nor AutoInclude can override that. Runtime
  `/api/admin/mcp/*` edits still override the seed.
- `gw.MountMCP(mux, opts...)` and `gateway.MCPPath(path)`
  (gw/mcp_server.go) wrap `MCPHandler()` in `AdminMiddleware` and
  register it on a `*http.ServeMux` in one call. Default path
  `/mcp`; override with `MCPPath`. Used by `gwag up` and the multi
  example so the standard gwag binary exposes MCP out of the box.
- `mcpIncludeRemove` and `mcpExcludeRemove` admin huma operations
  (`POST /api/admin/mcp/include/remove`, `POST /api/admin/mcp/exclude/remove`)
  — explicit removal sibling to the existing include / exclude
  endpoints. Needed so the UI can manage entries without overwriting
  the full list.
- `docs/mcp.md` — full MCP surface reference (tools, allowlist
  semantics, mounting, cluster behavior, runtime control). Linked
  from the README and from `docs/admin-auth.md`.
- `gat.RegisterHTTP(mux, g, prefix)` (gw/gat/http_register.go) — the
  huma-free counterpart of `RegisterHuma`. Mounts gat's four HTTP
  endpoints (`/graphql` + three schema views) on any `HandleMux` after
  `gat.New(regs...)` has built the gateway. `gwag serve` uses this
  path internally.
- `gwag serve` subcommand (gw/cmd/gwag/serve.go) boots the embedded
  gat translator against one upstream service described by an OpenAPI
  spec or `.proto` file. `gwag serve --openapi spec.yaml --to
  http://localhost:8081` or `gwag serve --proto greeter.proto --to
  localhost:50051`. No NATS, no admin, no cluster — for those use
  `gwag up`. `--addr`, `--prefix`, `--namespace`, `--version`
  overrides. Mounts `/graphql` + `/schema/{graphql,proto,openapi}`
  on a plain `http.ServeMux`. Doc: `docs/gat.md` "gwag serve — CLI
  shortcut".
- `gat.RegisterHTTP(mux, g, prefix)` (gw/gat/http_register.go) — the
  huma-free counterpart of `RegisterHuma`. Mounts gat's four HTTP
  endpoints (`/graphql` + three schema views) on any `HandleMux` after
  `gat.New(regs...)` has built the gateway. `gwag serve` uses this
  path internally.
- `gat.ProtoFile(path, target)` and `gat.ProtoSource(entry, body, imports, target)`
  (gw/gat/proto.go) compile a `.proto` via `protocompile`, ingest via
  `ir.IngestProto`, dial the target gRPC server, and return
  `[]ServiceRegistration` ready for `gat.New(...)`. The returned
  dispatchers do `grpc.ClientConn.Invoke` per call with `dynamicpb`
  marshal/unmarshal — no pool, no replicas, no backpressure (gat is
  the simple-start variant; pull full gwag for those). Default
  transport is insecure; mTLS or custom dials route through the
  existing `ServiceRegistration.Dispatchers` BYO path. Namespace and
  version derive from the proto package: `greeter.v1` →
  `greeter`/`v1` (trailing `vN` is the version), `pets` →
  `pets`/`v1`. Unary-only; server-streaming is not in gat's scope.
  Doc: `docs/gat.md` "Front a remote gRPC service from a .proto".
- `gw.WithWSLimit(gw.WSLimitOptions{MaxPerIP, RatePerSec, Burst, TrustedIPs})`
  (gw/ws_limit.go) caps the graphql-transport-ws Upgrade path per
  peer IP — a concurrent-connection semaphore plus a token-bucket
  rate limit on the handshake itself. Operators running gwag at the
  edge (no upstream reverse proxy / CDN terminating WebSockets) get
  the minimum DoS guard; an unset option leaves the upgrade path
  uncapped, preserving the pre-v1 behaviour. Rejected upgrades
  return HTTP 429 and increment
  `go_api_gateway_ws_rejected_total{reason}` with reason
  `max_per_ip` or `rate_limit`. Peer IP comes from `r.RemoteAddr`
  (port stripped); spoofable headers like `X-Forwarded-For` are
  not honoured — operators behind a trusted proxy pin its address
  in `TrustedIPs` and rely on the proxy for per-origin limits.
  Doc: `docs/operations.md` "WebSocket upgrade caps".
- `gw.WithTracer(trace.TracerProvider)` (gw/gateway.go) installs an
  OpenTelemetry TracerProvider for distributed tracing. Per-request
  server-kind spans land at every ingress shape: GraphQL
  (`gateway.graphql`), HTTP/JSON (`gateway.http`), gRPC
  (`gateway.grpc`), plus `.subscription` variants for SSE and gRPC
  server-streaming. Each upstream call opens a nested client-kind
  span (`gateway.dispatch.proto`, `gateway.dispatch.openapi`,
  `gateway.dispatch.graphql`, `gateway.dispatch.internal`,
  `gateway.dispatch.proto.subscription`) and injects W3C TraceContext
  on the outbound side — `traceparent` rides HTTP headers
  (OpenAPI / downstream GraphQL) and gRPC metadata (proto unary +
  server-streaming) so downstream services join the same trace. Spans
  carry the canonical `gateway.ingress` / `gateway.namespace` /
  `gateway.method` / `gateway.version` attribute set plus
  ingress-specific `http.*` / `rpc.*` keys from the OpenTelemetry
  semantic conventions. Inbound `traceparent` on the request is
  honoured so the gateway joins the caller's trace. When the option is
  unset, a noop tracer is wired and per-request cost stays near zero.
  Default builds depend on `go.opentelemetry.io/otel` directly.
- `Upload` GraphQL scalar (`gw.UploadScalar`, `*gw.Upload`) — exposed
  in every assembled schema so clients can declare `mutation
  ($f: Upload!)` against upload-capable fields. End-to-end multipart
  uploads via two complementary wire shapes: the
  graphql-multipart-request-spec parser handles inline single-request
  files at `POST /api/graphql`; tus.io v1.0 chunked uploads land at
  `gw.UploadsTusHandler()` (`/api/uploads/tus`) and the upload-id
  rides in GraphQL variables. The `Upload` scalar accepts both forms
  at `ParseValue`; the dispatcher opens the body lazily via
  `(*Upload).Open(ctx, store)` regardless of which wire shape
  delivered it.
- OpenAPI ingest recognises `multipart/form-data` request bodies and
  flattens `type: string, format: binary` schema properties into
  top-level `Upload!` (or `[Upload!]!`) GraphQL args. The
  `openAPIDispatcher` forwards as `multipart/form-data` upstream
  preserving the captured `Filename` and `Content-Type`. The
  gateway's re-exposed REST surface (`gw.IngressHandler`) accepts
  multipart inbound too. Proto-side binding deferred.
- `gw.UploadStore` interface (`Create` / `Append` / `Info` / `Open`
  / `Delete`) backing both wire shapes; default
  `gw.FilesystemUploadStore` impl with 24-hour TTL eviction.
- `gw.WithUploadStore(UploadStore)` plugs a custom store;
  `gw.WithUploadDataDir(string)` shorthand installs the default
  filesystem store (gateway owns lifecycle, `Close` shuts it down);
  `gw.WithUploadLimit(int64)` caps per-upload total bytes at both
  parsers.
- `docs/uploads.md` — adopter-facing docs covering both wire shapes,
  configuration knobs, error shapes, and outbound dispatch
  semantics.
- `docs/stability.md` — SemVer contract for 1.x. Defines what's
  locked, what's allowed to change, and what's experimental.
- `// Stability: stable` and `// Stability: experimental` godoc
  markers on every exported symbol in `gw/`, `gw/ir`, `gw/gat`,
  `gw/controlclient`. 187 stable + 14 experimental.
- `docs/api-audit.md` — pre-v1 export classification with per-symbol
  rationale.
- `CHANGELOG.md` (this file) and `RELEASE.md` (release process).
- `ir.OperationGroup.OriginKind` and `ir.OperationGroup.SchemaID`
  (gw/ir/ir.go) — group-level metadata so the runtime renderer can
  distinguish graphql-origin groups (which now install a single
  forwarding resolver at the top of the chain) from passthroughs.
  Group SchemaIDs use a `_group_<path>` suffix to avoid colliding
  with leaf Operation SchemaIDs. `PopulateSchemaIDs` populates them.

### Fixed
- `gw.AddGraphQL` against nested-namespace upstreams. Previously the
  per-leaf forwarder dropped the parent group chain: a local
  `{ ns { greeter { hello(...) } } }` produced an upstream
  `{ hello(...) }` that any sane GraphQL server rejects ("Cannot
  query field 'hello' on type 'Query'"). Now a single
  `graphqlGroupDispatcher` registers at each top-level graphql
  group; it captures the local sub-selection, forwards once with
  the container path intact (`{ greeter { hello(...) } }`), and a
  new `graphqlGroupChildResolver` demuxes the response map back
  into per-leaf field positions keyed by alias-or-name. Sibling
  leaves under one group ride a single upstream round trip —
  preserves GraphQL's batching across fields, no longer
  N round-trips. Aliases (including same-field-different-args, the
  case where aliasing is structurally required) round-trip
  correctly. Affected schemas: gwag-fronting-gwag, Hot Chocolate's
  grouped operations, some Apollo subgraph layouts. Pinned by
  `TestGraphQLIngest_NestedNamespaceForwarding`,
  `TestGraphQLIngest_DeepNestedNamespaceForwarding`,
  `TestGraphQLIngest_InlineFragmentInGroup`,
  `TestGraphQLIngest_GroupDispatcherIsolatedAcrossVersions`, and
  `TestGraphQLIngest_AliasInGroupSelection`.

### Changed
- OpenAPI namespace derivation now **preserves the spec title's
  case** — a spec titled `Pets` yields the GraphQL namespace `Pets`,
  not `pets`. Previously the library `AddOpenAPI` path preserved
  case while the `gat` translator and `gwag serve --openapi` lower-
  cased it, so the same spec produced a different schema depending
  on how it was registered. All three paths now route through one
  shared rule, `ir.SanitizeNamespace` (case preserved, spaces /
  dashes → `_`, leading digit guarded). This also fixes a latent
  bug where a digit-leading title produced an invalid GraphQL
  identifier.
- `gw.MCPConfig` and `gw.MCPHandler` promoted from `Stability:
  experimental` to `Stability: stable`. Same for the
  `/api/admin/mcp/*` huma operations. The four MCP tools
  (`schema_list` / `schema_search` / `schema_expand` / `query`) and
  the allowlist shape (AutoInclude / Include / Exclude with
  dot-segmented globs) are part of the 1.x contract.
- `gw.AddAdminEvents()` no longer requires a configured cluster — it
  registers the schema fragment unconditionally so SDL-driven codegen
  consumers pick up the `admin_events_watchServices` subscription
  field. Runtime subscriptions return a clean "subscription broker
  not available" error when no cluster resolves; cluster-mode
  behaviour is unchanged.
- Cluster-mode MCP allowlist: the first runtime mutation now merges
  into the local `WithMCPInclude(...)` seed instead of replacing it.
  Previously `tryMutateMCPConfig` started from `MCPConfig{}` when
  the JetStream KV bucket had no record, so the first
  `/api/admin/mcp/include` POST blew the seed away. The CAS path
  now falls back to the local snapshot on `ErrKeyNotFound`; after
  the first Put, KV stays authoritative as before.
- `gwag serve --mcp` now works with `--openapi` and `--proto`, not
  only `--graphql`. When `--mcp` (or `--mcp-include`) is set, the run
  promotes from the lite gat path onto the full `*gateway.Gateway`
  so `/mcp` shares dispatcher, plan cache, and metrics with every
  other ingress. The `/api/graphql` + `/api/schema/*` URL shape
  matches the existing `--graphql` flow; `--prefix` is ignored when
  `--mcp` is on.
- `examples/multi`: MCP mount moved from `/api/mcp` to `/mcp`, and
  the example seeds the allowlist with `WithMCPInclude("greeter.**",
  "library.**", "admin.**")`. Operators previously needed to POST to
  `/api/admin/mcp/include` before any tool returned useful results;
  the seed makes the demo work end-to-end on first boot.
- `gw/ir` SDL renderer emits one block-string per description (rather
  than one block per source line). Resolves a "consecutive
  triple-quoted strings" parse error from GraphQL lexers (including
  graphql-codegen) when descriptions contained blank or multi-line
  text. The rendered SDL is semantically equivalent.
- Wire-level identifiers renamed `go-api-gateway` → `gwag` across
  JetStream KV bucket names (`gwag-{registry,peers,stable,
  deprecated,mcp-config}`), the default NATS cluster name
  (`gwag`), the MCP server-info string returned by `MCPHandler`,
  and the UI's `localStorage` keys (`gwag:admin-token`,
  `gwag:admin-token-changed`). Pre-1.0 cleanup so the project
  ships with one consistent identifier.
- `github.com/IodeSystems/graphql-go` bumped 0.8.1 → 1.0.0 — first
  tagged release of the fork. `ExecutePlanAppend` walker available;
  gateway-side wiring is a follow-on perf step.

### Removed (pre-1.0 cleanup; no SemVer contract yet)
- `gw/`: unexported every admin-internal helper that escaped:
  - Stats / history wire types (`StatsSnapshot`, `MethodStatsSnapshot`,
    `HistoryBucket`, `ServiceHistory`, `SubjectInfo`) and their
    methods (`Snapshot`, `History`, `ActiveSubjects`).
  - MCP admin wire types (`Schema{Expand,List,Search}*`,
    `MCP{QueryInput,ResponseWithEvents,EventsBundle,ChannelEvent}`)
    and admin-only methods (`MCPAllows`, `MCPConfigSnapshot`,
    `MCPQuery`, `MCPSchema{Expand,List,Search}`, `SetMCPConfig`).
    `MCPHandler` stays public (the canonical wiring uses it).
  - Injector inventory types (`InjectorEntry`, `InjectorFrame`,
    `InjectorLanding`, `InjectorRecord`, `InjectorKind`, `InjectorState`
    + their consts) and the `InjectorInventory()` method.
  - `BackpressureConfig` + `BackpressureMiddleware` (internal
    plumbing; `BackpressureOptions` + `WithBackpressure` remain
    the public surface).
  - `Hide{Type,Path}Rewrite` / `Nullable{Type,Path}Rewrite` —
    concrete `SchemaRewrite` impls type-asserted inside `gw/` only.
  - `HeaderInjector` and `Transform.{Headers,Inventory}` — `Transform`
    is now opaque; construct via `InjectHeader` / `InjectType` /
    `InjectPath`.
  - `InternalProto{,Subscription}Handler` callback types.
  - `With{Admin,HTTP}Auth`, `IsAdminAuth`, `WithHTTPRequest` — set
    on ctx inside the gateway, never called from adopter code.
- `gw/`: dropped IR re-export shims (`RenderGraphQLRuntime`,
  `RenderGraphQLRuntimeFields`, `RuntimeOptions`, `IRTypeBuilder`,
  `IRTypeBuilderOptions`, `IRTypeNaming`, `NewIRTypeBuilder`).
  Callers import `gw/ir` directly.
- `gw/gat`: dropped six dead exports (`Option`, `config`, `As`,
  `Version`, `ServiceOption`, `ServiceConfig`) — declared but never
  applied anywhere. Per-registration knobs would go on
  `ServiceRegistration` as plain fields if/when needed.

## v0 — pre-1.0 development

Capsule history of what landed before the 1.0 surface lock. Granular
log lives in `git log`; this is the orientation for adopters reading
the changelog cold.

### Ingest pipeline
- **Proto ingest** via `protocompile` with `SourceInfoStandard` so
  comments survive into SDL + the MCP corpus. Three entry points:
  `AddProto(path)`, `AddProtoFS(fs, entry)`, `AddProtoBytes(entry, body)`.
- **OpenAPI 3.x ingest** via `kin-openapi`. Mirror entry points:
  `AddOpenAPI(spec)`, `AddOpenAPIBytes(spec)`.
- **GraphQL stitching** via boot-time introspection with a
  namespace-prefixed type mirror: `AddGraphQL(endpoint)`.
- Symmetric raw-source wire format across the control plane:
  `ServiceBinding.proto_source` + `proto_imports`, `openapi_spec`.
  The compiled-FileDescriptorSet path was retired pre-1.0.

### Schema rebuild + IR
- Format-agnostic intermediate representation (`gw/ir`) that lifts
  proto / OpenAPI / GraphQL into a common shape. Ingest happens once
  at registration; schema rebuild reads `slot.ir` directly.
- `RenderGraphQL{,Runtime,RuntimeFields}` for client-facing SDL,
  `RenderProtoFiles` for FDS export, `RenderOpenAPI` for the
  OpenAPI re-emit. Each format honours `FlatOperations` if the
  upstream format doesn't natively support nesting.
- Auto-internal namespaces: any namespace prefixed `_` is hidden
  from the public schema (used internally for admin / quota /
  pubsub-auth in-process services).
- Tier model: `unstable` (single overwrite slot), `vN` (locked
  once registered; differing schema-hash → `AlreadyExists`),
  `stable` (computed alias to highest-ever `vN`, monotonic; only
  `RetractStable` walks it back). `--allow-tier` per-deploy policy
  replaces the older `--environment` knob.

### Dispatch
- Reflection-based unary dispatch is the canonical path. Codegen +
  plugin paths are roadmap, not shipped.
- Per-pool backpressure (`BackpressureOptions`) with separate
  unary in-flight caps, per-replica caps, and gateway-wide stream
  caps. No gateway-wide unary cap — pools are the isolation primitive.
- Outbound transport pluggability: `WithOpenAPIClient(c)` / per-
  source `OpenAPIClient(c)` for HTTP; `LoadMTLSConfig` + `WithTLS`
  for mTLS on outbound gRPC.

### Pub/sub
- `gwag.ps.v1.PubSub` proto rides NATS for multi-listener fan-out
  with channel-named topics and a typed payload registry.
- Server-streaming gRPC at egress: each subscriber opens its own
  upstream stream. The earlier "auto-transform every `stream Resp`
  into a NATS subscription" path was retired pre-1.0 — the schema
  no longer lies about an upstream stream that didn't open.
- HMAC + per-pattern auth tiers (`WithChannelAuth(pattern, tier)`):
  `ChannelAuthOpen`, `ChannelAuthHMAC`, `ChannelAuthDelegate`.
  Strictest-tier-wins for wildcard subs. Sign-side: bearer-gated
  `SignSubscribeToken{,WithKid}`.

### Caller identity
- Three extractor styles via `WithCallerID{Public,HMAC,Delegated}`
  + the `WithCallerIDExtractor` seam for custom shapes.
- `WithCallerIDMetricsTopK` caps Prometheus label cardinality;
  overflow folds into the `OtherCallerID = "__other__"` bucket.
- Kid-aware HMAC rotation via `SignCallerIDTokenWithKid`.

### Admin surface
- Operations defined via huma (`gw/admin_huma.go`), mounted at
  `/admin/*`, and self-ingested as OpenAPI so the SDL gains nested
  `Query.admin.*` / `Mutation.admin.*` fields. The same path any
  external huma service takes.
- `AdminMiddleware`: bearer-gated mutations, public reads. Pluggable
  `AdminAuthorizer` delegate registered under `_admin_auth/v1`;
  boot token underneath as the always-works hatch.
- `WithAdminDataDir` persists the boot token under
  `<dir>/admin-token`; pair with `gwag login`/`use`/`context` for
  multi-cluster credential management.

### Multi-gateway clustering
- Embedded NATS via `StartCluster(ClusterOptions)`. JetStream KV
  buckets hold the registry, peer roster, stable map, deprecation
  state, and MCP config.
- Reconcilers on every node sync local slot state from KV watches —
  proto, OpenAPI, and GraphQL bindings ride in the same value
  shape; `slot.kind` picks the install / remove path.
- The gateway publishes its own service-change events on
  `admin_events_watchServices` so UIs can react to pool churn
  without polling.

### Embedded translator (`gw/gat`)
- `gat.New()` + `gat.Register{,Huma,HTTP,GRPC}` ship a single-server
  in-process translator that turns huma operations into both a
  GraphQL surface and connect-go gRPC endpoints. No NATS, no
  admin, no MCP — the minimum-cost entry for a huma app that wants
  typed multi-format clients.
- Proto ingest via `gat.ProtoFile` / `gat.ProtoSource` and the
  `gwag serve` subcommand for single-upstream embedded boot.
- Package classified `experimental`; surface may shift on a minor
  release.

### MCP integration
- `MCPHandler()` mounts an MCP-streamable HTTP endpoint with four
  tools: `schema_list`, `schema_search`, `schema_expand`,
  `gateway_query`. Operator-curated allowlist lives in the
  cluster `gwag-mcp-config` KV bucket; `SetMCPConfig` writes
  through.

### Operability
- Prometheus metrics under the `gateway_*` namespace: dispatch
  histograms, queue depth (per-pool + per-replica), backpressure
  rejects, admin-auth outcomes, caller-id label budgets, MCP
  audit counters.
- `WithPprof` mounts the runtime profiler under
  `AdminMiddleware`.
- Health endpoint (`HealthHandler`) flips to 503 during
  `Drain(ctx)`. Graceful drain waits for in-flight unary +
  active subscriptions before close.
- `WithRequestLog(io.Writer)` opt-in JSON-per-request sink with
  total time, self time, and per-dispatch counts.

### Benchmarking
- `bench/` self-perf harness with `bench traffic` (escalating-
  target-RPS sweep) + `bench perf` (knee detection, p95/p99
  reports → `docs/perf.md`).
- `perf/` competitor matrix scaffolding for head-to-head against
  graphql-mesh, Apollo Router, WunderGraph. Hermetic Docker
  build; scenarios declared in `perf/competitors.yaml`.

### Tooling / DX
- `gwag` binary (`gw/cmd/gwag`) with subcommands: `up` (zero-config
  NATS + admin + UI), `peer {list,forget}`, `services list`,
  `schema {fetch,diff}`, `sign`, `login`, `logout`, `context`, `use`.
- Embedded React + MUI v6 admin console at `ui/` with
  TanStack Router. `ui/embed.go` bundles the built SPA into the
  Go binary.
- `bin/build` single-entry build wrapper handling UI codegen +
  Vite build + Go build with fallback seeding so
  `go:embed all:dist` never breaks an unprepared tree.

### Won't fix (called out intentionally)
- **No Apollo Federation.** Stitching is correct for the common
  case; federation's entity-merging is overkill for most teams.
  See `docs/federation.md`.
- **No AsyncAPI export.** GraphQL SDL with Subscription types
  covers TS codegen; AsyncAPI's TS tooling is patchier.
- **One Register call = one address contributing to N pools, not
  N independent bindings.** Bindings share lifetime. Run two
  binaries for independent lifecycles.
