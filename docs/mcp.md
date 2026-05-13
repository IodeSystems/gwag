# MCP — exposing the gateway to LLM agents

LLM agents speak the Model Context Protocol over Streamable HTTP.
gwag mounts an MCP server that wraps four tools around the same
in-process executor every other ingress hits. Agents discover the
gateway's schema, narrow to operations that match their task, and
execute GraphQL through one channel — no second SDK, no separate
auth surface.

The operator decides what an agent can see by curating an allowlist.
Default-deny: an agent sees only operations matched by the include
list (or by the universe minus the exclude list, when auto-include
is on). Internal `_*` namespaces are filtered first regardless.

## Quick start

```go
gw := gateway.New(
    gateway.WithMCPInclude("greeter.**", "library.**"),
)
mux := http.NewServeMux()
mux.Handle("/api/graphql", gw.Handler())
gw.MountMCP(mux)   // registers /mcp behind AdminMiddleware
http.ListenAndServe(":8080", mux)
```

Verify with a bearer-gated `tools/list`:

```bash
TOKEN=...                       # boot token (printed at startup)
curl -s -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

A runnable client driver (Go, using `mark3labs/mcp-go`) lives at
[`examples/multi/cmd/mcp-demo`](../examples/multi/cmd/mcp-demo/main.go).
Boot the multi example (`cd examples/multi && ./run.sh`) and run
`go run ./cmd/mcp-demo` against it.

## Tools

| Tool | Purpose |
|---|---|
| `schema_list` | List every operation the allowlist exposes. |
| `schema_search` | Filter by dot-segmented path glob + regex over name / arg names / doc body. |
| `schema_expand` | Full structured contract of one op or type plus its transitive type closure. |
| `query` | Execute a GraphQL operation against the gateway in-process. |

### `schema_list`

No arguments. Returns one entry per allowed operation:

```json
[
  {"path":"greeter.hello", "kind":"Query", "namespace":"greeter", "version":"v1", "description":"Say hello."},
  {"path":"library.searchBooks", "kind":"Query", "namespace":"library", "version":"v1", "description":"…"}
]
```

`description` is the first line of the IR doc; the full body is on
`schema_expand`.

### `schema_search`

```json
{
  "pathGlob": "library.**",
  "regex": "book"
}
```

Both arguments are optional. `pathGlob` uses the same dot-segmented
shape as the allowlist (`*` = one segment, `**` = zero or more).
`regex` (Go syntax) is matched against op name, every arg name, and
the description body. Both filters AND together; an empty input
matches every allowed op.

Each result row carries the search-list entry shape plus typed args
(`String!`, `[User!]!`, …) and the output type name.

### `schema_expand`

```json
{"name": "greeter.hello"}
```

`name` is either an op path (resolves first) or a type name. The
response includes:

- the op or type definition (whichever matched)
- every type transitively reachable through args and output

Use this when an agent needs to know what to send and what to expect
back without scraping SDL.

### `query`

```json
{
  "query": "{ greeter { hello(name: \"world\") { greeting } } }",
  "variables": { "name": "world" },
  "operationName": "Greet"
}
```

`variables` and `operationName` are optional. The tool dispatches
in-process — same plan cache, same dispatchers, same metrics path —
and wraps the executor response in:

```json
{
  "response": { "data": …, "errors": … },
  "events":   { "level": "none", "channels": [] }
}
```

The `events` slot is the v1 placeholder for the future subscription
stitching contract. In v1 it is always `level: "none"` with no
channels; agents that ignore it today won't break when subscriptions
ride in additively.

## Allowlist

`MCPConfig` is three fields:

```go
type MCPConfig struct {
    AutoInclude bool
    Include     []string
    Exclude     []string
}
```

Modes:

- **`AutoInclude: false`** (default) — surface is exactly what
  `Include` matches.
- **`AutoInclude: true`** — surface is every public operation **minus**
  `Exclude`.

Glob semantics are dot-segmented:

| Pattern | Matches |
|---|---|
| `greeter.hello` | exact op |
| `greeter.*` | every direct child of the `greeter` namespace |
| `greeter.**` | the namespace and everything under it (any depth) |
| `*.list*` | one-segment namespace, op name beginning `list` |
| `**` | every op (only meaningful with `AutoInclude: true`) |

`*` and `**` are the dot-segmented globs; other `path.Match`
metacharacters apply *within* a segment.

Internal `_*` namespaces (admin auth, quota, pubsub auth) are filtered
before the allowlist is consulted. No pattern can override that.

## Seeding the allowlist at construction

Three options, all `Stability: stable`:

```go
gateway.WithMCPInclude("greeter.**", "admin.deprecate")
gateway.WithMCPExclude("admin.**")
gateway.WithMCPAutoInclude()
```

The seed is the gateway's initial state. In **cluster mode** the seed
is used the first time the cluster KV bucket is empty — i.e., the
first runtime mutation merges into the seed rather than replacing
it. After any operator-driven Put, KV is authoritative and the seed
is no longer consulted (every node converges via the watch loop).

In **standalone mode** the seed lives in process memory; runtime
mutations apply in place under the gateway mutex.

## Mounting

```go
gw.MountMCP(mux)                              // /mcp, AdminMiddleware
gw.MountMCP(mux, gateway.MCPPath("/agent"))   // custom path
```

`MountMCP` wraps `MCPHandler()` in `AdminMiddleware`, so every MCP
RPC is bearer-gated against the boot token plus any
`AdminAuthorizer` delegate. The handler itself is stateless — every
request opens a fresh session.

Operators who want a different posture (mTLS-by-proxy, custom auth)
can call `gw.MCPHandler()` directly and wrap as needed.

## Runtime control

Six admin endpoints, all under `/api/admin/mcp/` and gated by
`AdminMiddleware`:

| Method | Path | Body | Effect |
|---|---|---|---|
| `GET`  | `/admin/mcp` | — | Read current MCPConfig. |
| `POST` | `/admin/mcp/include` | `{"path":"…"}` | Append to include list (idempotent). |
| `POST` | `/admin/mcp/include/remove` | `{"path":"…"}` | Remove from include list (idempotent). |
| `POST` | `/admin/mcp/exclude` | `{"path":"…"}` | Append to exclude list (idempotent). |
| `POST` | `/admin/mcp/exclude/remove` | `{"path":"…"}` | Remove from exclude list (idempotent). |
| `POST` | `/admin/mcp/auto-include` | `{"autoInclude":true\|false}` | Toggle the mode. |

The same operations are surfaced as GraphQL fields under the `admin`
namespace (`Mutation.admin.mcpInclude(body: {…})`, etc.) via the
huma → OpenAPI → self-ingest path. A GraphQL UI can curate the
surface without a parallel REST client.

## Cluster semantics

The cluster-wide MCPConfig lives in the JetStream KV bucket
`gwag-mcp-config` (single key `config`, TTL=0). Each gateway
reconciles a local mirror via a `WatchAll(IncludeHistory)` loop, so
a node that joins after a Put converges to the cluster state without
an extra fetch.

Writes are CAS over the bucket revision (10-attempt retry, 100 ms
backoff, 2 s per-attempt timeout). On a fresh KV (no record), the
first write falls back to the local seed as the starting state, so
`WithMCPInclude(...)` survives the first runtime mutation.

When two nodes Put concurrently, one CAS succeeds and the other
retries against the new revision. The retry's mutator function
re-applies against the now-authoritative state, so an "include +
include" race converges to both entries; an "include + remove" race
preserves the operator's intent on whichever node landed second.

## Stability

| Symbol | Stability |
|---|---|
| `gw.MCPConfig` (autoInclude / include / exclude) | stable |
| `gw.MCPHandler()` | stable |
| `gw.MountMCP(mux, opts...)`, `gw.MCPPath(path)` | stable |
| `gw.WithMCPInclude(...)`, `gw.WithMCPExclude(...)`, `gw.WithMCPAutoInclude()` | stable |
| `/api/admin/mcp/*` huma routes (paths + JSON shapes) | stable |
| Four MCP tool names + argument shapes | stable |
| `mcpResponseWithEvents.events.level == "none"` (v1) | stable (additive evolution allowed) |
| MCP tool **descriptions** (the prompt strings) | experimental — wording may evolve |

Glob semantics (dot-segmented `*` / `**`) and the internal-namespace
filter are part of the stable contract. Adding new MCP tools is a
MINOR bump; removing or renaming an existing tool is a MAJOR bump.

## Limitations

- **No streaming.** Subscriptions are not exposed as MCP tools in
  v1. Agents that need event streams should subscribe over WebSocket
  (`/api/graphql`) against operations the allowlist exposes; the
  `mcpResponseWithEvents.events` envelope is reserved for the v2
  stitching design.
- **All-or-nothing bearer auth.** `AdminMiddleware` gates the entire
  `/mcp` mount on the admin token. Per-agent auth tiers and
  caller-id-tagged metrics are roadmap items, not v1 surface.
- **Schema search is single-pass.** The `regex` filter walks the
  description body once per allowed op; no precomputed index. Fine
  for the hundreds-of-ops regime adopters have seen so far.
